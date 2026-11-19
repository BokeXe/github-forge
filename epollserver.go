//go:build linux

package main

// Single-threaded epoll HTTP server with busy-polling.
//
// Why: in the trial pod (gVisor, node shared with many pods), the
// measured production penalty over local runs is dominated by wakeup
// and scheduling latency, not server CPU: halving server CPU and
// cutting allocations 9x moved the production score only 931 -> 934,
// while the top of the leaderboard runs "single-threaded epoll" and
// "busy-polled keep-alive" designs. A goroutine-per-connection server
// pays a futex wakeup per request on both the accept and read paths;
// under gVisor each of those syscalls is amplified, and on an
// oversubscribed node the woken thread may wait for a CPU.
//
// This server keeps one OS thread spinning in epoll_wait(timeout=0)
// (falling back to a 1ms wait after a long idle stretch), accepts and
// serves every connection itself, and runs the existing handlers
// synchronously. There is no per-request goroutine, no channel, and no
// wakeup between "request bytes arrived" and "response bytes written":
// the spinning thread sees the readiness event on its next poll, a few
// hundred nanoseconds later. The cost is one busy CPU core, which the
// entry's 2-CPU pod quota already budgets for.
//
// Requests are parsed from a per-connection byte buffer with the same
// readFastRequest used by the goroutine server, so wire behaviour
// (keep-alive, chunked bodies, 100-continue, HEAD, 400 on malformed
// input) is identical.

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"syscall"
)

type epollServer struct {
	handler  http.Handler
	listenFd int
	epollFd  int
	port     int
	conns    map[int32]*epollConn
	closed   atomic.Bool
}

type epollConn struct {
	fd           int
	remote       string
	inBuf        []byte
	outBuf       []byte
	wantWrite    bool
	closeAfter   bool
	continueSent bool
}

func newEpollServer(addr string, handler http.Handler) (*epollServer, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ip4 := tcpAddr.IP.To4()
	if tcpAddr.IP == nil {
		ip4 = net.IPv4zero
	}
	if ip4 == nil {
		return nil, fmt.Errorf("epoll server: only IPv4 listen addresses supported, got %q", addr)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	sa := &syscall.SockaddrInet4{Port: tcpAddr.Port}
	copy(sa.Addr[:], ip4)
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if err := syscall.Listen(fd, 1024); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	port := bound.(*syscall.SockaddrInet4).Port
	return &epollServer{
		handler:  handler,
		listenFd: fd,
		port:     port,
		conns:    make(map[int32]*epollConn),
	}, nil
}

func (s *epollServer) Port() int { return s.port }

func (s *epollServer) Close() error {
	s.closed.Store(true)
	if s.epollFd > 0 {
		syscall.Close(s.epollFd)
	}
	return syscall.Close(s.listenFd)
}

func (s *epollServer) Serve() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	s.epollFd = epfd
	defer syscall.Close(epfd)
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, s.listenFd, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(s.listenFd)}); err != nil {
		return err
	}
	events := make([]syscall.EpollEvent, 128)
	readBuf := make([]byte, 64*1024)
	emptySpins := 0
	for {
		if s.closed.Load() {
			return http.ErrServerClosed
		}
		timeout := 0
		if emptySpins > 3000 {
			timeout = 1 // 1ms: idle fallback so a quiet trial does not spin forever
		}
		n, err := syscall.EpollWait(epfd, events, timeout)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if s.closed.Load() {
				return http.ErrServerClosed
			}
			return err
		}
		if n == 0 {
			emptySpins++
			continue
		}
		emptySpins = 0
		for i := 0; i < n; i++ {
			ev := events[i]
			if int(ev.Fd) == s.listenFd {
				s.acceptAll(epfd)
				continue
			}
			conn := s.conns[ev.Fd]
			if conn == nil {
				continue
			}
			if ev.Events&(syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
				s.closeConn(epfd, conn)
				continue
			}
			if ev.Events&syscall.EPOLLIN != 0 {
				if !s.readConn(epfd, conn, readBuf) {
					continue
				}
			}
			if len(conn.outBuf) > 0 {
				s.flushConn(epfd, conn)
			}
			if conn.closeAfter && len(conn.outBuf) == 0 {
				s.closeConn(epfd, conn)
			}
		}
	}
}

func (s *epollServer) acceptAll(epfd int) {
	for {
		fd, sa, err := syscall.Accept(s.listenFd)
		if err != nil {
			return // EAGAIN: no more pending connections
		}
		syscall.SetNonblock(fd, true)
		syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		remote := ""
		if in4, ok := sa.(*syscall.SockaddrInet4); ok {
			remote = fmt.Sprintf("%d.%d.%d.%d:%d", in4.Addr[0], in4.Addr[1], in4.Addr[2], in4.Addr[3], in4.Port)
		}
		conn := &epollConn{fd: fd, remote: remote}
		s.conns[int32(fd)] = conn
		syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)})
	}
}

func (s *epollServer) closeConn(epfd int, conn *epollConn) {
	syscall.EpollCtl(epfd, syscall.EPOLL_CTL_DEL, conn.fd, nil)
	syscall.Close(conn.fd)
	delete(s.conns, int32(conn.fd))
}

// readConn reads available bytes and processes every complete request.
// Returns false when the connection was closed.
func (s *epollServer) readConn(epfd int, conn *epollConn, readBuf []byte) bool {
	for {
		n, err := syscall.Read(conn.fd, readBuf)
		if n > 0 {
			conn.inBuf = append(conn.inBuf, readBuf[:n]...)
		}
		if err != nil {
			if err == syscall.EAGAIN {
				break
			}
			s.closeConn(epfd, conn)
			return false
		}
		if n == 0 {
			s.closeConn(epfd, conn)
			return false
		}
		if n < len(readBuf) {
			break
		}
	}
	s.processRequests(conn)
	return true
}

type epollContinueWriter struct{ conn *epollConn }

func (w epollContinueWriter) Write(p []byte) (int, error) {
	if !w.conn.continueSent {
		w.conn.continueSent = true
		w.conn.outBuf = append(w.conn.outBuf, p...)
	}
	return len(p), nil
}

func (s *epollServer) processRequests(conn *epollConn) {
	for len(conn.inBuf) > 0 && !conn.closeAfter {
		rdr := bytes.NewReader(conn.inBuf)
		br := bufio.NewReaderSize(rdr, 32*1024)
		req, closeAfter, err := readFastRequest(br, epollContinueWriter{conn}, conn.remote)
		if err != nil {
			if err == errBadRequest {
				conn.outBuf = append(conn.outBuf, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"...)
				conn.closeAfter = true
				conn.inBuf = nil
			}
			// Otherwise the request is incomplete: wait for more bytes.
			return
		}
		remaining := br.Buffered() + rdr.Len()
		consumed := len(conn.inBuf) - remaining
		if consumed < 0 {
			consumed = len(conn.inBuf)
		}
		conn.inBuf = conn.inBuf[consumed:]
		conn.continueSent = false
		rw := fastWriterPool.Get().(*fastResponseWriter)
		rw.reset()
		s.handler.ServeHTTP(rw, req)
		conn.outBuf = append(conn.outBuf, rw.bytes(req.Method)...)
		fastWriterPool.Put(rw)
		if closeAfter || req.Close {
			conn.closeAfter = true
		}
	}
}

func (s *epollServer) flushConn(epfd int, conn *epollConn) {
	for len(conn.outBuf) > 0 {
		n, err := syscall.Write(conn.fd, conn.outBuf)
		if n > 0 {
			conn.outBuf = conn.outBuf[n:]
		}
		if err != nil {
			if err == syscall.EAGAIN {
				break
			}
			s.closeConn(epfd, conn)
			return
		}
		if n == 0 {
			break
		}
	}
	wantWrite := len(conn.outBuf) > 0
	if wantWrite != conn.wantWrite {
		conn.wantWrite = wantWrite
		events := uint32(syscall.EPOLLIN)
		if wantWrite {
			events |= syscall.EPOLLOUT
		}
		syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, conn.fd, &syscall.EpollEvent{Events: events, Fd: int32(conn.fd)})
	}
}
