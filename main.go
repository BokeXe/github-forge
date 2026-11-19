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
	"strconv"
	"strings"
	"time"
)

// The trial pod gives the entry 2 CPUs. Go 1.23 sizes GOMAXPROCS from the
// host's core count, which oversubscribes a 2-CPU cgroup quota (measured
// locally: default 4 threads scored 953-955, capped 2 scored 956, capped 1
// scored 958-962 on a shared 1.5-CPU VM where the client competes for the
// same quota — in the pod the env has its own container, so 2 is the cap
// that matches the entry's own limit and keeps clone parallelism).
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
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return err
		}
		fmt.Printf("[forge] listening on http://127.0.0.1:%d\n", port)
		return http.Serve(ln, handler)
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

	srv := &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

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
