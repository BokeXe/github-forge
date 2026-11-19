package main

// MCP surface: POST /mcp, JSON-RPC 2.0 over Streamable HTTP (plain JSON
// answers). Faithful port of mcp.ts. Speaks both eras the suite accepts:
//   modern 2026-07-28: server/discover (no handshake, no session)
//   legacy 2025-11-25 and earlier: initialize + notifications/initialized,
//   optional session id.
//
// Handler signature per CONTRACT.md: HandleMcp(w, r, store, token).
// Shared helpers from core.go / rest.go (assumed signatures, see report):
// Viewer, RequireAuth, RequestID, RepoToJSON, IssueToJSON, PullToJSON,
// CreateIssueInternal, CreatePullInternal, MergePullInternal. Repo creation
// uses (*Store).CreateRepo from store.go directly (mcp.ts createRepoRecord
// prefers the store's own createRepo).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	mcpServerName      = "github-forge"
	mcpServerVersion   = "1.0.0"
	mcpPreferredVer    = "2026-07-28"
	mcpJSONContentType = "application/json; charset=utf-8"
)

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// The tool list is a fixed literal: two tools/list calls return byte-identical
// arrays. This is JSON.stringify(TOOLS) from mcp.ts, byte for byte, built once.
const mcpToolsJSON = `[{"name":"create_repo","description":"Create a repository for the authenticated user.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login."},"name":{"type":"string","description":"Repository name."},"description":{"type":"string","description":"Repository description."},"private":{"type":"boolean","description":"Whether the repository is private."}},"required":["name"]}},{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login."},"repo":{"type":"string","description":"Repository name."}},"required":["owner","repo"]}},{"name":"list_repos","description":"List repositories owned by the authenticated user.","inputSchema":{"type":"object","properties":{},"required":[]}},{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login."},"repo":{"type":"string","description":"Repository name."},"title":{"type":"string","description":"Issue title."},"body":{"type":"string","description":"Issue body."}},"required":["owner","repo","title"]}},{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login."},"repo":{"type":"string","description":"Repository name."},"state":{"type":"string","description":"Issue state filter: open, closed, or all."}},"required":["owner","repo"]}},{"name":"get_issue","description":"Get a single issue by number.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login."},"repo":{"type":"string","description":"Repository name."},"issue_number":{"type":"integer","description":"Issue number."}},"required":["owner","repo","issue_number"]}},{"name":"create_pull_request","description":"Create a pull request from a head branch into a base branch.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login."},"repo":{"type":"string","description":"Repository name."},"title":{"type":"string","description":"Pull request title."},"body":{"type":"string","description":"Pull request body."},"head":{"type":"string","description":"Head branch name."},"base":{"type":"string","description":"Base branch name."}},"required":["owner","repo","title","head","base"]}},{"name":"merge_pull_request","description":"Merge a pull request with a squash commit.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login."},"repo":{"type":"string","description":"Repository name."},"pull_number":{"type":"integer","description":"Pull request number."}},"required":["owner","repo","pull_number"]}}]`

// mcpToolsListResult is the cached tools/list result bytes, built once.
var mcpToolsListResult = json.RawMessage(`{"tools":` + mcpToolsJSON + `}`)

type mcpToolDef struct {
	name     string
	required []string
}

var mcpToolDefs = []mcpToolDef{
	{name: "create_repo", required: []string{"name"}},
	{name: "get_repo", required: []string{"owner", "repo"}},
	{name: "list_repos", required: []string{}},
	{name: "create_issue", required: []string{"owner", "repo", "title"}},
	{name: "list_issues", required: []string{"owner", "repo"}},
	{name: "get_issue", required: []string{"owner", "repo", "issue_number"}},
	{name: "create_pull_request", required: []string{"owner", "repo", "title", "head", "base"}},
	{name: "merge_pull_request", required: []string{"owner", "repo", "pull_number"}},
}

func mcpFindTool(name string) *mcpToolDef {
	for i := range mcpToolDefs {
		if mcpToolDefs[i].name == name {
			return &mcpToolDefs[i]
		}
	}
	return nil
}

func mcpToolResult(payload any, isError bool) map[string]any {
	var text string
	if s, ok := payload.(string); ok {
		text = s
	} else {
		b, _ := json.Marshal(payload)
		text = string(b)
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": payload,
		"isError":           isError,
	}
}

type mcpErrorObj struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type mcpEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpErrorObj    `json:"error,omitempty"`
}

func mcpResultBytes(id any, result any) []byte {
	rb, _ := json.Marshal(result)
	b, _ := json.Marshal(mcpEnvelope{JSONRPC: "2.0", ID: id, Result: rb})
	return append(b, '\n')
}

func mcpErrorBytes(id any, code int, message string, data any) []byte {
	e := &mcpErrorObj{Code: code, Message: message}
	if data != nil {
		db, _ := json.Marshal(data)
		e.Data = db
	}
	b, _ := json.Marshal(mcpEnvelope{JSONRPC: "2.0", ID: id, Error: e})
	return append(b, '\n')
}

func mcpWrite(w http.ResponseWriter, status int, body []byte, extraHeaders map[string]string) {
	h := w.Header()
	h.Set("Content-Type", mcpJSONContentType)
	if body != nil {
		h.Set("Content-Length", strconv.Itoa(len(body)))
	}
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func mcpDeclaredVersion(params map[string]any, r *http.Request) (string, bool) {
	if meta, ok := params["_meta"].(map[string]any); ok && meta != nil {
		v := meta["io.modelcontextprotocol/protocolVersion"]
		if v == nil {
			v = meta["protocolVersion"]
		}
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	if header := r.Header.Get("Mcp-Protocol-Version"); header != "" {
		return header, true
	}
	return "", false
}

func mcpRequireRepo(store *Store, args map[string]any) (*Repo, error) {
	owner := gqlStringOrEmpty(args["owner"])
	if _, isStr := args["owner"].(string); !isStr {
		owner = Viewer
	}
	repoName := gqlStringOrEmpty(args["repo"])
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		repo = store.GetRepo(Viewer, repoName)
	}
	if repo == nil {
		return nil, fmt.Errorf("repository not found: %s/%s", owner, repoName)
	}
	return repo, nil
}

// mcpCallTool runs one tool. Caller must hold store.Mu (write lock).
func mcpCallTool(store *Store, name string, args map[string]any) (map[string]any, error) {
	switch name {
	case "create_repo":
		repoName := gqlStringValue(args["name"])
		if store.GetRepo(Viewer, repoName) != nil {
			return nil, fmt.Errorf("Repository creation failed: name already exists")
		}
		isPrivate, _ := args["private"].(bool)
		repo := store.CreateRepo(Viewer, repoName, gqlStringOrEmpty(args["description"]), isPrivate)
		return mcpToolResult(RepoToJSON(store, repo), false), nil
	case "get_repo":
		repo, err := mcpRequireRepo(store, args)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(RepoToJSON(store, repo), false), nil
	case "list_repos":
		repos := store.ListRepos(Viewer, true)
		// TS iterated the Map in insertion order; Go maps are unordered, so
		// sort by name for determinism (see report).
		sort.Slice(repos, func(a, b int) bool { return repos[a].Name < repos[b].Name })
		out := make([]any, 0, len(repos))
		for _, repo := range repos {
			out = append(out, RepoToJSON(store, repo))
		}
		return mcpToolResult(out, false), nil
	case "create_issue":
		repo, err := mcpRequireRepo(store, args)
		if err != nil {
			return nil, err
		}
		issue, err := CreateIssueInternal(store, repo, CreateIssueInput{Title: gqlStringOrEmpty(args["title"]), Body: gqlStringOrEmpty(args["body"])})
		if err != nil {
			return nil, err
		}
		return mcpToolResult(IssueToJSON(store, repo, issue), false), nil
	case "list_issues":
		repo, err := mcpRequireRepo(store, args)
		if err != nil {
			return nil, err
		}
		state := "open"
		if s, ok := args["state"].(string); ok {
			state = strings.ToLower(s)
		}
		list := make([]*Issue, 0, len(repo.Issues))
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				list = append(list, issue)
			}
		}
		sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
		out := make([]any, 0, len(list))
		for _, issue := range list {
			out = append(out, IssueToJSON(store, repo, issue))
		}
		return mcpToolResult(out, false), nil
	case "get_issue":
		repo, err := mcpRequireRepo(store, args)
		if err != nil {
			return nil, err
		}
		n, ok := gqlToInt(args["issue_number"])
		if !ok {
			return nil, fmt.Errorf("issue not found")
		}
		issue := repo.Issues[n]
		if issue == nil {
			return nil, fmt.Errorf("issue not found")
		}
		return mcpToolResult(IssueToJSON(store, repo, issue), false), nil
	case "create_pull_request":
		repo, err := mcpRequireRepo(store, args)
		if err != nil {
			return nil, err
		}
		pull, err := CreatePullInternal(store, repo, CreatePullInput{
			Head:  gqlStringOrEmpty(args["head"]),
			Base:  gqlStringOrEmpty(args["base"]),
			Title: gqlStringOrEmpty(args["title"]),
			Body:  gqlStringOrEmpty(args["body"]),
		})
		if err != nil {
			return nil, err
		}
		return mcpToolResult(PullToJSON(store, repo, pull), false), nil
	case "merge_pull_request":
		repo, err := mcpRequireRepo(store, args)
		if err != nil {
			return nil, err
		}
		n, ok := gqlToInt(args["pull_number"])
		if !ok {
			return nil, fmt.Errorf("pull request not found")
		}
		pull := repo.Pulls[n]
		if pull == nil {
			return nil, fmt.Errorf("pull request not found")
		}
		sha, message, err := MergePullInternal(repo, pull, "squash")
		if err != nil {
			return nil, err
		}
		// TS: { merged: true, sha, message, number, ...pullToJson(repo, pull) }
		result := PullToJSON(store, repo, pull)
		result["merged"] = true
		result["sha"] = sha
		result["message"] = message
		result["number"] = pull.Number
		return mcpToolResult(result, false), nil
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func HandleMcp(w http.ResponseWriter, r *http.Request, store *Store, token string) {
	method := r.Method
	if method == http.MethodDelete {
		// Legacy session teardown: best-effort, always fine.
		mcpWrite(w, 204, nil, nil)
		return
	}
	if method != http.MethodPost {
		body, _ := json.Marshal(map[string]any{"message": "method not allowed"})
		mcpWrite(w, 405, append(body, '\n'), nil)
		return
	}

	// Auth is decided before anything else about the request (incl. tool existence).
	if err := RequireAuth(r, token); err != nil {
		mcpWrite(w, 401, mcpErrorBytes(nil, -32000, "unauthorized", nil), nil)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<20)+1))
	if err != nil || len(raw) > 64<<20 {
		mcpWrite(w, 400, mcpErrorBytes(nil, -32700, "Parse error", nil), nil)
		return
	}
	var body any
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil {
			mcpWrite(w, 400, mcpErrorBytes(nil, -32700, "Parse error", nil), nil)
			return
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			mcpWrite(w, 400, mcpErrorBytes(nil, -32700, "Parse error", nil), nil)
			return
		}
	}
	bodyMap, isObject := body.(map[string]any)
	if !isObject || bodyMap == nil {
		mcpWrite(w, 400, mcpErrorBytes(nil, -32600, "Invalid Request", nil), nil)
		return
	}

	id, hasIDKey := bodyMap["id"]
	hasID := hasIDKey && id != nil
	rpcMethod := gqlStringOrEmpty(bodyMap["method"])
	params := map[string]any{}
	if p, ok := bodyMap["params"].(map[string]any); ok && p != nil {
		params = p
	}

	// Notifications carry no id and get no JSON-RPC reply.
	if !hasID {
		mcpWrite(w, 202, nil, nil)
		return
	}

	// A declared protocol version we do not support is a -32022 refusal naming usable versions.
	if declared, ok := mcpDeclaredVersion(params, r); ok {
		supported := false
		for _, v := range mcpSupportedVersions {
			if v == declared {
				supported = true
				break
			}
		}
		if !supported {
			mcpWrite(w, 200, mcpErrorBytes(id, -32022, "Unsupported protocol version: "+declared, map[string]any{"supported": mcpSupportedVersions}), nil)
			return
		}
	}

	switch rpcMethod {
	case "server/discover":
		serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
		result := map[string]any{
			"protocolVersion":   mcpPreferredVer,
			"supportedVersions": append([]string{}, mcpSupportedVersions...),
			"serverInfo":        serverInfo,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}
		mcpWrite(w, 200, mcpResultBytes(id, result), nil)
		return

	case "initialize":
		requested := gqlStringOrEmpty(params["protocolVersion"])
		chosen := mcpPreferredVer
		if requested != "" {
			for _, v := range mcpSupportedVersions {
				if v == requested {
					chosen = requested
					break
				}
			}
		}
		// A deterministic UUID-shaped session id (unique per initialize),
		// derived from the per-process request counter exactly as mcp.ts does.
		seqText := strings.TrimPrefix(RequestID(), "req-")
		seq, _ := strconv.ParseInt(seqText, 36, 64)
		sessionID := fmt.Sprintf("00000000-0000-4000-8000-%012x", seq)
		result := map[string]any{
			"protocolVersion": chosen,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		}
		mcpWrite(w, 200, mcpResultBytes(id, result), map[string]string{"mcp-session-id": sessionID})
		return

	case "ping":
		mcpWrite(w, 200, mcpResultBytes(id, map[string]any{}), nil)
		return

	case "tools/list":
		mcpWrite(w, 200, mcpResultBytes(id, mcpToolsListResult), nil)
		return

	case "tools/call":
		toolName := gqlStringOrEmpty(params["name"])
		args := map[string]any{}
		if a, ok := params["arguments"].(map[string]any); ok && a != nil {
			args = a
		}
		tool := mcpFindTool(toolName)
		if tool == nil {
			mcpWrite(w, 200, mcpErrorBytes(id, -32601, "Method not found: unknown tool "+toolName, nil), nil)
			return
		}
		var missing []string
		for _, key := range tool.required {
			v, present := args[key]
			if !present || v == nil {
				missing = append(missing, key)
				continue
			}
			if s, isStr := v.(string); isStr && s == "" {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			mcpWrite(w, 200, mcpErrorBytes(id, -32602, "Invalid params: missing required argument(s): "+strings.Join(missing, ", "), nil), nil)
			return
		}
		// Build the full response bytes under the write lock, write after unlock.
		store.Mu.Lock()
		result, callErr := mcpCallTool(store, toolName, args)
		var response []byte
		if callErr != nil {
			response = mcpResultBytes(id, mcpToolResult(map[string]any{"error": callErr.Error()}, true))
		} else {
			response = mcpResultBytes(id, result)
		}
		store.Mu.Unlock()
		mcpWrite(w, 200, response, nil)
		return
	}

	mcpWrite(w, 200, mcpErrorBytes(id, -32601, "Method not found: "+rpcMethod, nil), nil)
}
