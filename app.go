package main

// Integrated entry server: one process, one shared store, four surfaces.
// Faithful port of app.ts:
// - git smart HTTP  -> HandleGit      (/{owner}/{repo}.git/...)
// - REST v3         -> HandleRest     (/api/v3/...)
// - GraphQL v4      -> HandleGraphql  (/api/graphql)
// - MCP             -> HandleMcp      (/mcp)
// - GET /healthz    -> 200 {"ok":true} (env boot probe, no auth)
//
// HandleRest and HandleGit are provided by sibling modules (signatures in
// CONTRACT.md); this file only routes to them.

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const appJSONType = "application/json; charset=utf-8"

// appSendJSON mirrors app.ts sendJson: compact JSON plus a trailing newline,
// content-type and content-length only (no x-github-request-id on this path).
func appSendJSON(w http.ResponseWriter, status int, body any) {
	text, _ := json.Marshal(body)
	text = append(text, '\n')
	w.Header().Set("Content-Type", appJSONType)
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

// appPathOf mirrors app.ts pathOf: the raw request path before '?', keeping
// %-escapes exactly as sent. Go's EscapedPath preserves the escaped form
// (via RawPath when valid), so dot-segment or malformed targets still miss
// every route and answer 404 as before.
func appPathOf(r *http.Request) string {
	path := r.URL.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// NewApp builds the entry's HTTP handler over one shared store. The bearer
// token the suite presents is read once from MAKEFUN_SHOP_TOKEN (empty when
// unset, local dev only); the env var is fixed for the life of the process.
func NewApp(store *Store) http.Handler {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := appPathOf(r)

		// The env's boot check calls this twice: 200 {"ok":true}. No auth.
		if r.Method == http.MethodGet && path == "/healthz" {
			appSendJSON(w, 200, map[string]any{"ok": true})
			return
		}

		// REST v3 (includes the /api/v3 root). Checked before the git surface
		// so the read/write hot paths never pay the git route matching; a git
		// path cannot start with '/api/v3/'.
		if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
			HandleRest(w, r, store, token)
			return
		}

		// GraphQL v4.
		if path == "/api/graphql" {
			HandleGraphql(w, r, store, token)
			return
		}

		// Git smart HTTP owns /{owner}/{repo}.git/... — HandleGit answers, or
		// says it is not a git route by returning false.
		if HandleGit(w, r, store, token) {
			return
		}

		// MCP (streamable HTTP).
		if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
			HandleMcp(w, r, store, token)
			return
		}

		// Everything else, every method: GitHub's 404 body.
		appSendJSON(w, 404, map[string]any{"message": "Not Found"})
	})
}
