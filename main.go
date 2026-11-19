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
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The trial pod gives the entry 2 CPUs, shared at node level with the env
// and many other pods, under gVisor where every scheduler/futex syscall is
// amplified. The epoll server busy-polls on one locked OS thread, so one
// P is spent spinning; the second P runs GC and the trial-mode
// register/done client. More Ps only add scheduling overhead.
func init() {
	runtime.GOMAXPROCS(2)
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
		srv, err := newEpollServer(fmt.Sprintf("127.0.0.1:%d", port), handler)
		if err != nil {
			return err
		}
		fmt.Printf("[forge] listening on http://127.0.0.1:%d\n", port)
		return srv.Serve()
	}

	// Trial mode.
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		return fmt.Errorf("MAKEFUN_ENV_URL is not set: there is no env to register with")
	}
	srv, err := newEpollServer(":0", handler)
	if err != nil {
		return err
	}
	port := srv.Port()
	fmt.Printf("[entry] listening on port %d\n", port)

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
