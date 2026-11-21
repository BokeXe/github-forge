package main

// Single-threaded epoll reactor replacing the goroutine-per-connection
// fast server (v3 experiment E4).
//
// Why: in the trial pod the entry runs under gVisor, where every
// syscall is trapped into the Sentry and every goroutine wakeup is a
// futex + scheduler handoff across the server, the Sentry and the
// Node suite client sharing 2 CPUs. The goroutine server pays, per
// request: netpoller epoll_wait wakeup, futex wake of the parked
// connection goroutine, a scheduler handoff, then read + write
// syscalls. This reactor pays only: one epoll_wait (batched over all
// ready connections), read, write — all on one OS thread, with no
// goroutine handoff between the readiness event and the handler.
//
// It is NOT a busy-poll: epoll_wait blocks with no timeout, so an idle
// reactor consumes no CPU (the earlier busy-poll variant starved the
// suite client and scored worse in production).
//
// Semantics mirror fastserver.go: HTTP/1.1 keep-alive, Content-Length
// and chunked request bodies, Expect: 100-continue, HEAD body
// suppression (in the shared fastResponseWriter), Connection: close,
// HTTP/1.0, malformed requests answer 400 and close. Handlers run
// inline on the reactor thread; they are all short (cached responses,
// small packs), so one request never stalls the others for long.

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

type reactorConn struct {
	fd         int
	in         []byte
	out        []byte
	outOff     int
	closeAfter bool // close once out is flushed
	sent100    bool // 100-continue already sent for the pending request
	remoteAddr string
}

type reactor struct {
	handler http.Handler
	ln      net.Listener
	lnFile  *os.File // dup of the listener; must stay referenced or
	// the GC finalizer closes it and epoll loses the listener
	lnFd     int
	epFd     int
	wakeR    int
	wakeW    int
	conns    map[int]*reactorConn
	closed   bool
	closeErr error
}

func newReactor(ln net.Listener, handler http.Handler) (*reactor, error) {
	tcpLn, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, errors.New("reactor needs a TCP listener")
	}
	f, err := tcpLn.File()
	if err != nil {
		return nil, err
	}
	lnFd := int(f.Fd())
	// The File() dup is blocking; make it nonblocking for Accept4 loop.
	if err := syscall.SetNonblock(lnFd, true); err != nil {
		f.Close()
		return nil, err
	}
	// Verify O_NONBLOCK really stuck: a blocking listener would wedge
	// acceptAll forever. If it did not, refuse the reactor (the caller
	// falls back to the goroutine server).
	if flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(lnFd), uintptr(syscall.F_GETFL), 0); errno != 0 || flags&syscall.O_NONBLOCK == 0 {
		f.Close()
		return nil, errors.New("listener fd is not nonblocking")
	}
	epFd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		f.Close()
		return nil, err
	}
	var wake [2]int
	if err := syscall.Pipe2(wake[:], syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		f.Close()
		syscall.Close(epFd)
		return nil, err
	}
	r := &reactor{
		handler: handler,
		ln:      ln,
		lnFile:  f,
		lnFd:    lnFd,
		epFd:    epFd,
		wakeR:   wake[0],
		wakeW:   wake[1],
		conns:   make(map[int]*reactorConn),
	}
	if err := r.epollAdd(lnFd, syscall.EPOLLIN); err != nil {
		r.shutdown()
		return nil, err
	}
	if err := r.epollAdd(r.wakeR, syscall.EPOLLIN); err != nil {
		r.shutdown()
		return nil, err
	}
	return r, nil
}

func (r *reactor) epollAdd(fd int, events uint32) error {
	return syscall.EpollCtl(r.epFd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: events, Fd: int32(fd)})
}

func (r *reactor) epollMod(fd int, events uint32) {
	_ = syscall.EpollCtl(r.epFd, syscall.EPOLL_CTL_MOD, fd, &syscall.EpollEvent{Events: events, Fd: int32(fd)})
}

func (r *reactor) Close() error {
	// Wake the reactor loop; it performs the actual teardown.
	_, _ = syscall.Write(r.wakeW, []byte{1})
	return nil
}

func (r *reactor) shutdown() {
	for fd := range r.conns {
		syscall.Close(fd)
	}
	r.conns = make(map[int]*reactorConn)
	syscall.Close(r.wakeR)
	syscall.Close(r.wakeW)
	syscall.Close(r.epFd)
	if r.lnFile != nil {
		r.lnFile.Close()
	}
	r.ln.Close()
}

func (r *reactor) Serve() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	events := make([]syscall.EpollEvent, 128)
	for {
		n, err := syscall.EpollWait(r.epFd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			r.shutdown()
			return err
		}
		for i := 0; i < n; i++ {
			ev := events[i]
			fd := int(ev.Fd)
			switch fd {
			case r.wakeR:
				r.shutdown()
				return http.ErrServerClosed
			case r.lnFd:
				r.acceptAll()
			default:
				c := r.conns[fd]
				if c == nil {
					continue
				}
				if ev.Events&(syscall.EPOLLHUP|syscall.EPOLLERR) != 0 && ev.Events&syscall.EPOLLIN == 0 {
					r.closeConn(c)
					continue
				}
				if ev.Events&syscall.EPOLLIN != 0 {
					if !r.readConn(c) {
						continue
					}
				}
				if ev.Events&syscall.EPOLLOUT != 0 {
					r.flushConn(c)
				}
			}
		}
	}
}

func (r *reactor) acceptAll() {
	for {
		fd, sa, err := syscall.Accept4(r.lnFd, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
		if err != nil {
			return // EAGAIN or transient error: wait for next event
		}
		_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		remote := ""
		if sa != nil {
			if ip4, ok := sa.(*syscall.SockaddrInet4); ok {
				remote = net.IPv4(ip4.Addr[0], ip4.Addr[1], ip4.Addr[2], ip4.Addr[3]).String() + ":" + strconv.Itoa(ip4.Port)
			} else if ip6, ok := sa.(*syscall.SockaddrInet6); ok {
				remote = "[" + net.IP(ip6.Addr[:]).String() + "]:" + strconv.Itoa(ip6.Port)
			}
		}
		c := &reactorConn{fd: fd, remoteAddr: remote}
		r.conns[fd] = c
		if err := r.epollAdd(fd, syscall.EPOLLIN); err != nil {
			r.closeConn(c)
		}
	}
}

func (r *reactor) closeConn(c *reactorConn) {
	delete(r.conns, c.fd)
	syscall.Close(c.fd)
}

// readConn drains available bytes and processes every complete request.
// Returns false when the connection was closed.
func (r *reactor) readConn(c *reactorConn) bool {
	var tmp [32 * 1024]byte
	for {
		n, err := syscall.Read(c.fd, tmp[:])
		if n > 0 {
			c.in = append(c.in, tmp[:n]...)
			if len(c.in) > fastMaxBody+(256<<10) {
				r.closeConn(c)
				return false
			}
		}
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			if err == syscall.EINTR {
				continue
			}
			r.closeConn(c)
			return false
		}
		if n == 0 {
			r.closeConn(c)
			return false
		}
		if n < len(tmp) {
			break
		}
	}
	return r.processRequests(c)
}

func (r *reactor) processRequests(c *reactorConn) bool {
	for {
		req, consumed, status := parseReactorRequest(c.in, c.remoteAddr)
		switch status {
		case parseIncomplete:
			// Maybe headers are complete but the body is not: honour
			// Expect: 100-continue so the client starts sending.
			if !c.sent100 && reactorWantsContinue(c.in) {
				c.out = append(c.out, "HTTP/1.1 100 Continue\r\n\r\n"...)
				c.sent100 = true
				r.flushConn(c)
				if _, ok := r.conns[c.fd]; !ok {
					return false
				}
			}
			return true
		case parseBad:
			c.out = append(c.out, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"...)
			c.closeAfter = true
			r.flushConn(c)
			return false
		case parseOK:
			c.in = c.in[consumed:]
			c.sent100 = false
			rw := fastWriterPool.Get().(*fastResponseWriter)
			rw.reset()
			r.handler.ServeHTTP(rw, req)
			c.out = append(c.out, rw.bytes(req.Method)...)
			fastWriterPool.Put(rw)
			if req.Close {
				c.closeAfter = true
			}
			putFastRequest(req)
			if !r.flushConn(c) {
				return false
			}
			if c.closeAfter {
				return false
			}
		}
	}
}

// flushConn writes as much of c.out as the socket accepts. Returns
// false when the connection was closed.
func (r *reactor) flushConn(c *reactorConn) bool {
	for c.outOff < len(c.out) {
		n, err := syscall.Write(c.fd, c.out[c.outOff:])
		if n > 0 {
			c.outOff += n
		}
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				r.epollMod(c.fd, syscall.EPOLLIN|syscall.EPOLLOUT)
				return true
			}
			if err == syscall.EINTR {
				continue
			}
			r.closeConn(c)
			return false
		}
		if n == 0 {
			r.epollMod(c.fd, syscall.EPOLLIN|syscall.EPOLLOUT)
			return true
		}
	}
	c.out = c.out[:0]
	c.outOff = 0
	if c.closeAfter {
		r.closeConn(c)
		return false
	}
	r.epollMod(c.fd, syscall.EPOLLIN)
	return true
}

const (
	parseIncomplete = iota
	parseOK
	parseBad
)

// reactorWantsContinue reports whether the buffered bytes already hold
// a complete header block carrying Expect: 100-continue.
func reactorWantsContinue(buf []byte) bool {
	end := bytes.Index(buf, []byte("\r\n\r\n"))
	if end < 0 {
		return false
	}
	head := buf[:end]
	for len(head) > 0 {
		line := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			line, head = head[:i], head[i+1:]
		} else {
			head = nil
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		if equalFoldASCII(bytes.TrimRight(line[:colon], "\r"), "expect") &&
			strings.EqualFold(strings.TrimSpace(string(line[colon+1:])), "100-continue") {
			return true
		}
	}
	return false
}

// parseReactorRequest parses one HTTP request from buf. It mirrors
// readFastRequest's semantics exactly: same header whitelist, same
// framing rules, same close defaults. consumed counts the bytes of
// the request (headers + body) when status is parseOK.
func parseReactorRequest(buf []byte, remoteAddr string) (req *http.Request, consumed int, status int) {
	// Skip stray blank lines before the request line.
	rest := buf
	skipped := 0
	for {
		if len(rest) == 0 {
			return nil, 0, parseIncomplete
		}
		if rest[0] == '\n' {
			rest, skipped = rest[1:], skipped+1
			continue
		}
		if rest[0] == '\r' {
			if len(rest) < 2 {
				return nil, 0, parseIncomplete
			}
			if rest[1] == '\n' {
				rest, skipped = rest[2:], skipped+2
				continue
			}
		}
		break
	}
	headEnd := bytes.Index(rest, []byte("\r\n\r\n"))
	sepLen := 4
	if headEnd < 0 {
		headEnd = bytes.Index(rest, []byte("\n\n"))
		sepLen = 2
	}
	if headEnd < 0 {
		if len(rest) > 64<<10 {
			return nil, 0, parseBad
		}
		return nil, 0, parseIncomplete
	}
	head := rest[:headEnd]
	bodyStart := headEnd + sepLen

	// Request line.
	nl := bytes.IndexByte(head, '\n')
	var reqLine []byte
	var headerLines []byte
	if nl < 0 {
		reqLine = head
	} else {
		reqLine, headerLines = head[:nl], head[nl+1:]
	}
	reqLine = bytes.TrimSuffix(reqLine, []byte("\r"))
	sp1 := bytes.IndexByte(reqLine, ' ')
	if sp1 <= 0 {
		return nil, 0, parseBad
	}
	afterMethod := reqLine[sp1+1:]
	sp2 := bytes.IndexByte(afterMethod, ' ')
	if sp2 <= 0 {
		return nil, 0, parseBad
	}
	method := string(reqLine[:sp1])
	target := string(afterMethod[:sp2])
	proto := string(afterMethod[sp2+1:])
	if !strings.HasPrefix(proto, "HTTP/") {
		return nil, 0, parseBad
	}
	http10 := proto == "HTTP/1.0"

	req = fastRequestPool.Get().(*http.Request)
	header := req.Header
	contentLength := int64(-1)
	chunked := false
	closeAfter := http10
	host := ""
	for len(headerLines) > 0 {
		var hl []byte
		if i := bytes.IndexByte(headerLines, '\n'); i >= 0 {
			hl, headerLines = headerLines[:i], headerLines[i+1:]
		} else {
			hl, headerLines = headerLines, nil
		}
		hl = bytes.TrimSuffix(hl, []byte("\r"))
		if len(hl) == 0 {
			continue
		}
		colon := bytes.IndexByte(hl, ':')
		if colon <= 0 {
			putFastRequest(req)
			return nil, 0, parseBad
		}
		rawKey := hl[:colon]
		value := strings.TrimSpace(string(hl[colon+1:]))
		switch {
		case equalFoldASCII(rawKey, "content-length"):
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				contentLength = n
			}
		case equalFoldASCII(rawKey, "transfer-encoding"):
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case equalFoldASCII(rawKey, "connection"):
			lv := strings.ToLower(value)
			if strings.Contains(lv, "close") {
				closeAfter = true
			} else if strings.Contains(lv, "keep-alive") {
				closeAfter = false
			}
		case equalFoldASCII(rawKey, "host"):
			host = value
		case equalFoldASCII(rawKey, "authorization"):
			header["Authorization"] = append(header["Authorization"], value)
		case equalFoldASCII(rawKey, "git-protocol"):
			header["Git-Protocol"] = append(header["Git-Protocol"], value)
		case equalFoldASCII(rawKey, "mcp-protocol-version"):
			header["Mcp-Protocol-Version"] = append(header["Mcp-Protocol-Version"], value)
		case equalFoldASCII(rawKey, "content-type"):
			header["Content-Type"] = append(header["Content-Type"], value)
		case equalFoldASCII(rawKey, "accept"):
			header["Accept"] = append(header["Accept"], value)
		}
	}

	avail := rest[bodyStart:]
	var body []byte
	var pooledBody []byte
	bodyLen := 0
	if chunked {
		decoded, n, st := decodeChunked(avail)
		if st != parseOK {
			putFastRequest(req)
			return nil, 0, st
		}
		body, bodyLen = decoded, n
		contentLength = int64(len(body))
	} else if contentLength > 0 {
		readLen := contentLength
		if readLen > fastMaxBody {
			readLen = fastMaxBody
		}
		if int64(len(avail)) < readLen {
			putFastRequest(req)
			return nil, 0, parseIncomplete
		}
		pooledBody = fastBodyPool.Get().([]byte)
		if int64(cap(pooledBody)) < readLen {
			pooledBody = make([]byte, readLen)
		} else {
			pooledBody = pooledBody[:readLen]
		}
		copy(pooledBody, avail[:readLen])
		body = pooledBody
		bodyLen = int(readLen)
		if contentLength > fastMaxBody {
			closeAfter = true
		}
	} else {
		body = []byte{}
		if contentLength < 0 {
			contentLength = 0
		}
	}

	if err := parseRequestTargetInto(req.URL, target); err != nil {
		if pooledBody != nil {
			fastBodyPool.Put(pooledBody[:0])
		}
		putFastRequest(req)
		return nil, 0, parseBad
	}

	req.Method = method
	req.Proto = proto
	req.ProtoMajor = 1
	req.ProtoMinor = 1
	req.Body = &fastBodyReader{Reader: bytes.NewReader(body), buf: pooledBody}
	req.ContentLength = contentLength
	req.Host = host
	req.RequestURI = target
	req.RemoteAddr = remoteAddr
	req.Close = closeAfter
	if http10 {
		req.ProtoMinor = 0
	}
	return req, skipped + bodyStart + bodyLen, parseOK
}

// decodeChunked decodes a chunked body from the front of buf.
// Returns the decoded bytes and total consumed bytes.
func decodeChunked(buf []byte) (body []byte, consumed int, status int) {
	pos := 0
	for {
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl < 0 {
			return nil, 0, parseIncomplete
		}
		sizeText := strings.TrimSpace(string(bytes.TrimSuffix(buf[pos:pos+nl], []byte("\r"))))
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, 0, parseBad
		}
		pos += nl + 1
		if size == 0 {
			// Trailer headers until a blank line.
			for {
				nl := bytes.IndexByte(buf[pos:], '\n')
				if nl < 0 {
					return nil, 0, parseIncomplete
				}
				line := bytes.TrimSuffix(buf[pos:pos+nl], []byte("\r"))
				pos += nl + 1
				if len(line) == 0 {
					return body, pos, parseOK
				}
			}
		}
		if int64(len(body))+size > fastMaxBody {
			return nil, 0, parseBad
		}
		if int64(len(buf)-pos) < size+2 {
			return nil, 0, parseIncomplete
		}
		body = append(body, buf[pos:pos+int(size)]...)
		pos += int(size) + 2 // chunk + trailing CRLF
	}
}

// serveReactor runs the reactor unless FORGE_SERVER=goroutine selects
// the goroutine server (kept for A/B measurement).
func serveReactor(ln net.Listener, handler http.Handler) error {
	if os.Getenv("FORGE_SERVER") == "goroutine" {
		return newFastServer(ln, handler).Serve()
	}
	r, err := newReactor(ln, handler)
	if err != nil {
		return newFastServer(ln, handler).Serve()
	}
	return r.Serve()
}
