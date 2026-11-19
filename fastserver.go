package main

// Minimal HTTP/1.1 server replacing net/http's Server on the hot path.
//
// Why: the suite's workloads are thousands of small requests (2,000 issue
// reads, 400 push exchanges, 250 clone exchanges). net/http allocates a
// Header map with canonicalised keys, a url.URL, a request context and a
// background-read goroutine per connection, and formats a Date header on
// every reply. Under the trial pod's gVisor runtime every syscall and
// allocation is amplified. This server parses only what the handlers use
// (method, request target, the few headers they read, a Content-Length or
// chunked body), hands handlers a plain http.Request, buffers the reply,
// and writes it with a single Write.
//
// Behaviour kept compatible with what the handlers and clients rely on:
// keep-alive, Content-Length and chunked request bodies, Expect:
// 100-continue, HEAD body suppression, Connection: close, HTTP/1.0.
// Malformed request lines/headers answer 400 and close.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const fastMaxBody = (64 << 20) + 1 // handlers enforce 64 MiB themselves

type fastServer struct {
	handler http.Handler
	ln      net.Listener
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closed  bool
}

func newFastServer(ln net.Listener, handler http.Handler) *fastServer {
	return &fastServer{handler: handler, ln: ln, conns: make(map[net.Conn]struct{})}
}

func (s *fastServer) Close() error {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	return s.ln.Close()
}

func (s *fastServer) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return http.ErrServerClosed
			}
			return err
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.SetNoDelay(true)
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				conn.Close()
			}()
			s.serveConn(conn)
		}()
	}
}

var errBadRequest = errors.New("bad request")

func equalFoldASCII(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}

// readLine reads one CRLF (or LF) terminated line without the terminator.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		// Rare over-long line: accumulate fragments.
		var full []byte
		for err == bufio.ErrBufferFull {
			full = append(full, line...)
			line, err = r.ReadSlice('\n')
		}
		if err != nil {
			return nil, err
		}
		full = append(full, line...)
		line = full
	} else if err != nil {
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return line, nil
}

var fastReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 32*1024) },
}

func (s *fastServer) serveConn(conn net.Conn) {
	br := fastReaderPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer fastReaderPool.Put(br)
	for {
		req, closeAfter, err := readFastRequest(br, conn)
		if err != nil {
			if err == errBadRequest {
				io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}
			return
		}
		rw := fastWriterPool.Get().(*fastResponseWriter)
		rw.reset()
		s.handler.ServeHTTP(rw, req)
		err = rw.writeTo(conn, req.Method)
		fastWriterPool.Put(rw)
		if err != nil {
			return
		}
		if closeAfter || req.Close {
			return
		}
	}
}

func readFastRequest(br *bufio.Reader, conn net.Conn) (*http.Request, bool, error) {
	// Skip any stray blank lines before the request line (net/http does).
	var line []byte
	for {
		var err error
		line, err = readLine(br)
		if err != nil {
			return nil, true, err
		}
		if len(line) > 0 {
			break
		}
	}
	// Request line: METHOD SP TARGET SP HTTP/x.y
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return nil, true, errBadRequest
	}
	rest := line[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 <= 0 {
		return nil, true, errBadRequest
	}
	method := string(line[:sp1])
	target := string(rest[:sp2])
	proto := string(rest[sp2+1:])
	if !strings.HasPrefix(proto, "HTTP/") {
		return nil, true, errBadRequest
	}
	http10 := proto == "HTTP/1.0"

	header := make(http.Header, 4)
	contentLength := int64(-1)
	chunked := false
	expectContinue := false
	closeAfter := http10
	host := ""
	for {
		hl, err := readLine(br)
		if err != nil {
			return nil, true, err
		}
		if len(hl) == 0 {
			break
		}
		colon := bytes.IndexByte(hl, ':')
		if colon <= 0 {
			return nil, true, errBadRequest
		}
		rawKey := hl[:colon]
		value := strings.TrimSpace(string(hl[colon+1:]))
		// Only the headers handlers actually read are stored (plus the
		// framing headers parsed here). Comparing on raw bytes avoids a
		// canonicalisation allocation per header.
		switch {
		case equalFoldASCII(rawKey, "content-length"):
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				contentLength = n
			}
		case equalFoldASCII(rawKey, "transfer-encoding"):
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case equalFoldASCII(rawKey, "expect"):
			if strings.EqualFold(value, "100-continue") {
				expectContinue = true
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

	if expectContinue {
		if _, err := io.WriteString(conn, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return nil, true, err
		}
	}

	var body []byte
	if chunked {
		var err error
		body, err = readChunkedBody(br)
		if err != nil {
			return nil, true, err
		}
		contentLength = int64(len(body))
	} else if contentLength > 0 {
		readLen := contentLength
		if readLen > fastMaxBody {
			readLen = fastMaxBody
		}
		body = make([]byte, readLen)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, true, err
		}
		if contentLength > fastMaxBody {
			// Handlers will 413 on the truncated body; the connection
			// cannot be reused with unread bytes pending.
			closeAfter = true
		}
	} else {
		body = []byte{}
		if contentLength < 0 {
			contentLength = 0
		}
	}

	u, err := url.ParseRequestURI(target)
	if err != nil {
		// Absolute-form or odd targets: fall back to a plain parse so
		// routing still sees a path (net/http accepts absolute-form).
		u, err = url.Parse(target)
		if err != nil {
			return nil, true, errBadRequest
		}
	}

	req := &http.Request{
		Method:        method,
		URL:           u,
		Proto:         proto,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: contentLength,
		Host:          host,
		RequestURI:    target,
		RemoteAddr:    conn.RemoteAddr().String(),
		Close:         closeAfter,
	}
	if http10 {
		req.ProtoMinor = 0
	}
	return req, closeAfter, nil
}

func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, err
		}
		sizeText := string(line)
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || size < 0 {
			return nil, errBadRequest
		}
		if size == 0 {
			// Trailer headers until a blank line.
			for {
				tl, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if len(tl) == 0 {
					break
				}
			}
			return body, nil
		}
		if int64(len(body))+size > fastMaxBody {
			return nil, errBadRequest
		}
		start := len(body)
		body = append(body, make([]byte, size)...)
		if _, err := io.ReadFull(br, body[start:]); err != nil {
			return nil, err
		}
		// Trailing CRLF after each chunk.
		crlf := make([]byte, 2)
		if _, err := io.ReadFull(br, crlf); err != nil {
			return nil, err
		}
	}
}

var fastWriterPool = sync.Pool{
	New: func() any {
		return &fastResponseWriter{header: make(http.Header, 4)}
	},
}

func (w *fastResponseWriter) reset() {
	for k := range w.header {
		delete(w.header, k)
	}
	w.status = 0
	w.wroteHeader = false
	w.body.Reset()
}

type fastResponseWriter struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (w *fastResponseWriter) Header() http.Header { return w.header }

func (w *fastResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

func (w *fastResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

func (w *fastResponseWriter) writeTo(conn net.Conn, method string) error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	var out bytes.Buffer
	out.Grow(w.body.Len() + 256)
	out.WriteString("HTTP/1.1 ")
	out.WriteString(strconv.Itoa(w.status))
	out.WriteByte(' ')
	out.WriteString(http.StatusText(w.status))
	out.WriteString("\r\n")
	hasCL := false
	for key, values := range w.header {
		if strings.EqualFold(key, "Content-Length") {
			hasCL = true
		}
		for _, v := range values {
			out.WriteString(key)
			out.WriteString(": ")
			out.WriteString(v)
			out.WriteString("\r\n")
		}
	}
	if !hasCL && w.status != http.StatusNoContent && w.status != http.StatusNotModified {
		out.WriteString("Content-Length: ")
		out.WriteString(strconv.Itoa(w.body.Len()))
		out.WriteString("\r\n")
	}
	out.WriteString("\r\n")
	if method != http.MethodHead && w.status != http.StatusNoContent {
		out.Write(w.body.Bytes())
	}
	_, err := conn.Write(out.Bytes())
	return err
}
