package main

// The entry <-> env lifecycle, ported from the Node entry's main.ts:
// bind an ephemeral port, POST {"port": N} to $MAKEFUN_ENV_URL/register,
// poll GET $MAKEFUN_ENV_URL/done until {"done": true}, then exit 0.
//
// Local grading mode: when FORGE_PORT is set (or argv[1] is a port), serve
// on that port forever — sdk/run.ts drives it directly, exactly like the
// Node entry's launcher.ts.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// The trial pod gives the entry 2 CPUs, shared at node level with the env
// and many other pods, under gVisor where every scheduler/futex syscall is
// amplified. GOMAXPROCS matches the entry's own 2-CPU quota: enough
// parallelism for the 50-way clone concurrency and GC, without the
// oversubscription of Go 1.23's host-core default. (Measured in
// production: GOMAXPROCS=1 with this server scored 934; a busy-polling
// single-thread epoll variant scored 923 — the spin starved the env
// client at node level — so the goroutine server stays.)
func init() {
	runtime.GOMAXPROCS(2)
	// GC tuning (v3 experiment E1/E5): a trial is short (900 s wall,
	// the benchmark phase ~seconds of work) and the pod has 1024 MiB.
	// GC pauses and background GC CPU compete with the suite client,
	// so collect rarely (400% heap growth) with a soft memory limit
	// (768 MiB) as the ceiling. Pure GOGC=off was measured worse
	// locally in an earlier iteration, so it is not used.
	debug.SetGCPercent(400)
	debug.SetMemoryLimit(768 << 20)
}

const requestTimeout = 30 * time.Second

func retryDelay(attempt int) time.Duration {
	d := 250 * time.Millisecond << min(attempt, 8)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

func registerPort(client *http.Client, envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("content-type", "application/json")
			var res *http.Response
			res, err = client.Do(req)
			if err == nil {
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode == 200 || res.StatusCode == 409 {
					return nil
				}
				if res.StatusCode == 400 || res.StatusCode == 405 {
					return fmt.Errorf("register answered HTTP %d", res.StatusCode)
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(client *http.Client, envURL string) {
	for attempt := 0; ; attempt++ {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			data, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				var parsed struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(data, &parsed) == nil && parsed.Done {
					return
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	store := NewStore()
	handler := NewApp(store)

	// Local grading mode.
	portText := os.Getenv("FORGE_PORT")
	if portText == "" && len(os.Args) > 1 {
		portText = os.Args[1]
	}
	if portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil {
			return fmt.Errorf("bad port %q", portText)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return err
		}
		fmt.Printf("[forge] listening on http://127.0.0.1:%d\n", port)
		return serveReactor(ln, handler)
	}

	// Trial mode.
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		return fmt.Errorf("MAKEFUN_ENV_URL is not set: there is no env to register with")
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", port)

	var srv interface {
		Serve() error
		Close() error
	}
	if os.Getenv("FORGE_SERVER") == "goroutine" {
		srv = newFastServer(ln, handler)
	} else if r, err := newReactor(ln, handler); err == nil {
		srv = r
	} else {
		srv = newFastServer(ln, handler)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	client := &http.Client{Timeout: requestTimeout}
	if err := registerPort(client, envURL, port); err != nil {
		return err
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	done := make(chan struct{})
	go func() {
		waitForDone(client, envURL)
		close(done)
	}()
	select {
	case err := <-serveErr:
		return err
	case <-done:
	}
	srv.Close()
	fmt.Println("[entry] done")
	return nil
}
