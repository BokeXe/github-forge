package main

// Shared helpers for the REST v3 / GraphQL v4 / MCP modules of the
// github-forge entry — a faithful port of core.ts. The store itself lives
// in store.go (Store, Repo, Issue, Pull, HashObject, PutObject, GetObject,
// NowIso, RepoKey, ...); this file adds everything core.ts layered on top:
// HTTP plumbing, global ids, git object helpers, commit/tree parse+build,
// diffs, and the issue/pull mutation internals shared by all three
// surfaces so they all see one store.
//
// Concurrency: the Node original is single-threaded. In Go every function
// here that touches store state assumes the caller holds store.Mu —
// mutations under Lock, pure reads under RLock (see CONTRACT.md). The one
// exception is RequestID, which is atomic.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	// Viewer is the login of the authenticated user (core.ts VIEWER).
	Viewer = "adaiversen"
	// VIEWER is an alias kept for TS-shaped call sites.
	VIEWER = Viewer
	// ServerName is the MCP/server name (core.ts SERVER_NAME).
	ServerName = "github-forge"
	// ServerVersion is the server version (core.ts SERVER_VERSION).
	ServerVersion = "1.0.0"
	// GitEpoch is the fixed author/committer epoch for commits this
	// module creates: 2025-01-01T00:00:00Z.
	GitEpoch = 1735689600
)

// HttpError is an HTTP error that maps straight onto a JSON
// {"message": ...} reply.
type HttpError struct {
	Status  int
	Message string
}

func (e *HttpError) Error() string { return e.Message }

// Err builds an HttpError (core.ts err()).
func Err(status int, message string) *HttpError {
	return &HttpError{Status: status, Message: message}
}

// AsHttpError unwraps err to *HttpError when it is one.
func AsHttpError(err error) (*HttpError, bool) {
	var he *HttpError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

// StringPtr returns a pointer to a copy of s (for Pull.MergedAt etc.).
func StringPtr(s string) *string { return &s }

// DerefString returns *p, or "" when p is nil.
func DerefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------------------------------------------------------------------------
// Store shapes (store.go provides the real types)
// ---------------------------------------------------------------------------

// GetRepo returns the repo for owner/name or nil. Caller must hold
// store.Mu (read or write). Thin wrapper over (*Store).GetRepo so TS
// call sites port directly.
func GetRepo(store *Store, owner, name string) *Repo {
	return store.GetRepo(owner, name)
}

// EnsureRepoMaps makes sure a repo has every collection the store
// promises. Caller must hold store.Mu in write mode (it may allocate).
func EnsureRepoMaps(repo *Repo) *Repo {
	if repo.Refs == nil {
		repo.Refs = make(map[string]string)
	}
	if repo.Objects == nil {
		repo.Objects = make(map[string]*GitObject)
	}
	if repo.Issues == nil {
		repo.Issues = make(map[int]*Issue)
	}
	if repo.Pulls == nil {
		repo.Pulls = make(map[int]*Pull)
	}
	if repo.Labels == nil {
		repo.Labels = make(map[string]*Label)
	}
	if repo.PackCache == nil {
		repo.PackCache = make(map[string][]byte)
	}
	if repo.NextIssue == 0 {
		repo.NextIssue = len(repo.Issues) + 1
	}
	if repo.NextPull == 0 {
		repo.NextPull = len(repo.Pulls) + 1
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "main"
	}
	if repo.HeadTarget == "" {
		repo.HeadTarget = "refs/heads/main"
	}
	return repo
}

// CreateRepoRecord creates a repo record in the store (idempotent, like
// store.CreateRepo) and assigns its database id. Caller must hold
// store.Mu in write mode.
func CreateRepoRecord(store *Store, owner, name, description string, isPrivate bool) *Repo {
	repo := store.CreateRepo(owner, name, description, isPrivate)
	EnsureRepoMaps(repo)
	DatabaseID(store, repo)
	return repo
}

// SetRef points ref at oid. Caller must hold store.Mu (write).
func SetRef(repo *Repo, ref, oid string) {
	repo.Refs[ref] = oid
	repo.RefsVersion++
}

// GetRef returns the oid ref points at, or "" when it does not exist.
func GetRef(repo *Repo, ref string) string {
	return repo.Refs[ref]
}

// BranchRef is the full refname for a branch.
func BranchRef(branch string) string {
	return "refs/heads/" + branch
}

// BranchTip returns the oid a branch points at, or "" when missing.
func BranchTip(repo *Repo, branch string) string {
	return repo.Refs[BranchRef(branch)]
}

// ResolveCommit resolves a ref-ish (branch name, full ref, or raw commit
// oid) to a commit oid, or "" when it resolves to nothing. An empty
// refish means the default branch.
func ResolveCommit(repo *Repo, refish string) string {
	if refish == "" {
		return BranchTip(repo, repo.DefaultBranch)
	}
	if oid, ok := repo.Refs[refish]; ok {
		return oid
	}
	if oid := repo.Refs[BranchRef(refish)]; oid != "" {
		return oid
	}
	if _, ok := repo.Objects[refish]; ok {
		return refish
	}
	return ""
}

// ---------------------------------------------------------------------------
// Stable ids
// ---------------------------------------------------------------------------

// DatabaseID returns the repo's numeric database id, assigning the next
// one from store.NextDBID (starting at 10000) when unassigned. Assigning
// mutates the store, so a caller that might trigger assignment must hold
// store.Mu in write mode; once assigned, reads under RLock are safe.
func DatabaseID(store *Store, repo *Repo) int {
	if repo.DBID > 0 {
		return repo.DBID
	}
	repo.DBID = store.NextDBID
	store.NextDBID++
	return repo.DBID
}

// DatabaseIDPeek returns the repo's database id without assigning one
// (0 = unassigned). Safe under RLock.
func DatabaseIDPeek(repo *Repo) int {
	return repo.DBID
}

// B64URLEncode is base64url without padding, as JS Buffer 'base64url'.
func B64URLEncode(text string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

// B64URLDecode decodes base64url text (padded or not). ok is false when
// the text is not valid base64url.
func B64URLDecode(text string) (string, bool) {
	if b, err := base64.RawURLEncoding.DecodeString(text); err == nil {
		return string(b), true
	}
	if b, err := base64.URLEncoding.DecodeString(text); err == nil {
		return string(b), true
	}
	return "", false
}

// RepoGlobalID is the GraphQL node id for a repository.
func RepoGlobalID(repo *Repo) string {
	return "R_" + B64URLEncode("repo:"+repo.Owner+"/"+repo.Name)
}

// IssueGlobalID is the GraphQL node id for an issue.
func IssueGlobalID(repo *Repo, number int) string {
	return "I_" + B64URLEncode(fmt.Sprintf("issue:%s/%s:%d", repo.Owner, repo.Name, number))
}

// PullGlobalID is the GraphQL node id for a pull request.
func PullGlobalID(repo *Repo, number int) string {
	return "P_" + B64URLEncode(fmt.Sprintf("pull:%s/%s:%d", repo.Owner, repo.Name, number))
}

// CommentGlobalID is the GraphQL node id for an issue comment.
func CommentGlobalID(repo *Repo, issueNumber, commentID int) string {
	return "C_" + B64URLEncode(fmt.Sprintf("comment:%s/%s:%d:%d", repo.Owner, repo.Name, issueNumber, commentID))
}

// GlobalID is a decoded opaque global id.
type GlobalID struct {
	Kind   string // "repo" | "issue" | "pull" | "comment"
	Owner  string
	Name   string
	Number int // issue/pull number; 0 for repos
}

// DecodeGlobalID decodes an opaque global id back to its coordinates.
// ok is false when it is not one of ours.
func DecodeGlobalID(id string) (GlobalID, bool) {
	if len(id) < 3 {
		return GlobalID{}, false
	}
	switch id[0] {
	case 'R', 'I', 'P', 'C':
	default:
		return GlobalID{}, false
	}
	if id[1] != '_' {
		return GlobalID{}, false
	}
	decoded, ok := B64URLDecode(id[2:])
	if !ok {
		return GlobalID{}, false
	}
	parts := strings.Split(decoded, ":")
	if len(parts) < 2 {
		return GlobalID{}, false
	}
	kind := parts[0]
	full := parts[1]
	slash := strings.Index(full, "/")
	if slash < 0 {
		return GlobalID{}, false
	}
	owner := full[:slash]
	name := full[slash+1:]
	number := 0
	if len(parts) > 2 {
		if n, ok := parseJSInt(parts[2]); ok {
			number = n
		}
	}
	if owner == "" || name == "" {
		return GlobalID{}, false
	}
	return GlobalID{Kind: kind, Owner: owner, Name: name, Number: number}, true
}

// parseJSNumber parses s the way JS Number(s) does, for the forms the
// suite can plausibly send: decimal, exponent, hex 0x, Infinity, empty
// (0). ok is false for NaN results.
func parseJSNumber(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, true
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f, true
	}
	// JS Number also accepts unsigned 0x/0b/0o integer literals.
	if len(t) > 2 && t[0] == '0' {
		var base int
		switch t[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			if u, err := strconv.ParseUint(t[2:], base, 64); err == nil {
				return float64(u), true
			}
		}
	}
	return 0, false
}

// parseJSInt is parseJSNumber restricted to finite integers, mirroring
// Number.isInteger(Number(s)).
func parseJSInt(s string) (int, bool) {
	f, ok := parseJSNumber(s)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
		return 0, false
	}
	if f > float64(math.MaxInt) || f < float64(math.MinInt) {
		return 0, false
	}
	return int(f), true
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

const maxBodyBytes = 64 * 1024 * 1024

// ReadRawBody reads the whole request body, rejecting bodies over
// 64 MiB with a 413 HttpError (as core.ts readRawBody).
func ReadRawBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBodyBytes {
		return nil, Err(413, "request body too large")
	}
	return data, nil
}

// ParseJSONBody parses raw as JSON. Empty body -> {}. Malformed -> 400.
func ParseJSONBody(raw []byte) (any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, Err(400, "malformed JSON body")
	}
	return v, nil
}

// ReadJSONBody reads and parses a request body as JSON.
func ReadJSONBody(r *http.Request) (any, error) {
	raw, err := ReadRawBody(r)
	if err != nil {
		return nil, err
	}
	return ParseJSONBody(raw)
}

var requestCounter atomic.Int64

// RequestID is the X-GitHub-Request-Id value: a per-process counter,
// "req-<base36>", unique per reply and deterministic across replay.
func RequestID() string {
	n := requestCounter.Add(1)
	return "req-" + strconv.FormatInt(n, 36)
}

// MarshalJSON marshals v like JS JSON.stringify: no HTML escaping, no
// trailing newline. (encoding/json sorts map keys; the suite parses
// JSON, so key order does not matter.)
func MarshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func marshalJSONLine(v any) []byte {
	b, err := MarshalJSON(v)
	if err != nil {
		return []byte("{\"message\":\"internal error\"}\n")
	}
	return append(b, '\n')
}

// SendJSON sends body as JSON with a trailing newline (204: empty body).
func SendJSON(w http.ResponseWriter, status int, body any, extraHeaders map[string]string) {
	if status == 204 {
		SendJSONText(w, status, "", extraHeaders)
		return
	}
	SendJSONText(w, status, string(marshalJSONLine(body)), extraHeaders)
}

// SendJSONText sends an already-serialized JSON string as-is (callers
// include the trailing newline, as sendJson emits).
func SendJSONText(w http.ResponseWriter, status int, text string, extraHeaders map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-Id", RequestID())
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	if status != 204 {
		h.Set("Content-Length", strconv.Itoa(len(text)))
	}
	w.WriteHeader(status)
	if status != 204 {
		_, _ = io.WriteString(w, text)
	}
}

// SendJSONBuffer sends an already-serialized JSON buffer.
func SendJSONBuffer(w http.ResponseWriter, status int, buf []byte, extraHeaders map[string]string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(buf)))
	h.Set("X-GitHub-Request-Id", RequestID())
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// SendError sends {"message": ...} with the given status.
func SendError(w http.ResponseWriter, status int, message string) {
	if message == "" {
		message = "error"
	}
	SendJSON(w, status, map[string]any{"message": message}, nil)
}

// BearerToken extracts the bearer token from the Authorization header.
func BearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	t := strings.TrimSpace(header)
	if !strings.HasPrefix(t, "Bearer") {
		return "", false
	}
	rest := t[len("Bearer"):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// bearerHeaderValue is "Bearer <token>" computed once at startup (the
// token is fixed for the life of the process), so hot auth checks are a
// single string compare instead of a concatenation per request.
var bearerHeaderValue string

func SetBearerHeader(token string) {
	if token != "" {
		bearerHeaderValue = "Bearer " + token
	}
}

func BearerHeaderMatches(r *http.Request, token string) bool {
	if bearerHeaderValue != "" {
		return r.Header.Get("Authorization") == bearerHeaderValue
	}
	return r.Header.Get("Authorization") == "Bearer "+token
}

// IsAuthorized is true when the request carries exactly the token.
func IsAuthorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	bt, ok := BearerToken(r)
	return ok && bt == token
}

// RequireAuth returns a 401 HttpError unless the request is authorized.
func RequireAuth(r *http.Request, token string) error {
	if !IsAuthorized(r, token) {
		return Err(401, "requires authentication")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Git objects (standard git object formats, so the git smart-HTTP
// surface reads the same objects this module writes). HashObject,
// PutObject and GetObject live in store.go.
// ---------------------------------------------------------------------------

// PutBlob stores content as a blob and returns its oid.
func PutBlob(repo *Repo, content []byte) string {
	return PutObject(repo, "blob", content)
}

// CommitInfo is the parsed header+message of a commit object.
type CommitInfo struct {
	Tree    string
	Parents []string
	Message string
}

// ParseCommit parses a commit object's bytes.
func ParseCommit(data []byte) CommitInfo {
	text := string(data)
	split := strings.Index(text, "\n\n")
	header := text
	message := ""
	if split >= 0 {
		header = text[:split]
		message = text[split+2:]
	}
	info := CommitInfo{Parents: []string{}, Message: message}
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "tree ") {
			info.Tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		}
	}
	return info
}

// CreateCommit writes a commit object with the fixed viewer identity
// and GitEpoch, and returns its oid.
func CreateCommit(repo *Repo, treeOid string, parents []string, message string) string {
	lines := []string{"tree " + treeOid}
	for _, parent := range parents {
		lines = append(lines, "parent "+parent)
	}
	ident := fmt.Sprintf("%s <%s@example.invalid> %d +0000", Viewer, Viewer, GitEpoch)
	lines = append(lines, "author "+ident)
	lines = append(lines, "committer "+ident)
	data := []byte(strings.Join(lines, "\n") + "\n\n" + message)
	return PutObject(repo, "commit", data)
}

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses a tree object's bytes.
func ParseTree(data []byte) []TreeEntry {
	entries := []TreeEntry{}
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		mode := string(data[offset:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries
}

// treeNode is a nested path tree: values are []byte (file) or treeNode.
type treeNode map[string]any

func treeNodeFromFiles(files map[string][]byte) treeNode {
	root := treeNode{}
	for path, content := range files {
		var parts []string
		for _, p := range strings.Split(path, "/") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		node := root
		for i, part := range parts {
			if i == len(parts)-1 {
				node[part] = content
			} else {
				child, ok := node[part].(treeNode)
				if !ok {
					child = treeNode{}
					node[part] = child
				}
				node = child
			}
		}
	}
	return root
}

func writeTreeNode(repo *Repo, node treeNode) string {
	names := make([]string, 0, len(node))
	for name := range node {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		value := node[name]
		var entry TreeEntry
		if child, ok := value.(treeNode); ok {
			entry = TreeEntry{Mode: "40000", Name: name, OID: writeTreeNode(repo, child)}
		} else {
			entry = TreeEntry{Mode: "100644", Name: name, OID: PutBlob(repo, value.([]byte))}
		}
		buf.WriteString(entry.Mode + " " + entry.Name + "\x00")
		oidBytes, _ := hex.DecodeString(entry.OID)
		buf.Write(oidBytes)
	}
	return PutObject(repo, "tree", buf.Bytes())
}

// BuildTree builds a tree object for a flat path -> content map and
// returns the tree oid. (Stores blobs/trees in the repo, as core.ts.)
func BuildTree(repo *Repo, files map[string][]byte) string {
	return writeTreeNode(repo, treeNodeFromFiles(files))
}

// FileEntry is one file at a commit: its blob oid and content.
type FileEntry struct {
	OID     string
	Content []byte
}

// FilesAtCommit returns all files at a commit: path -> {oid, content}.
// Empty map for an unknown commit.
func FilesAtCommit(repo *Repo, commitOid string) map[string]FileEntry {
	out := make(map[string]FileEntry)
	commitObj := GetObject(repo, commitOid)
	if commitObj == nil || commitObj.Type != "commit" {
		return out
	}
	tree := ParseCommit(commitObj.Data).Tree
	var walk func(treeOid, prefix string)
	walk = func(treeOid, prefix string) {
		treeObj := GetObject(repo, treeOid)
		if treeObj == nil || treeObj.Type != "tree" {
			return
		}
		for _, entry := range ParseTree(treeObj.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, path)
			} else if blob := GetObject(repo, entry.OID); blob != nil {
				out[path] = FileEntry{OID: entry.OID, Content: blob.Data}
			}
		}
	}
	walk(tree, "")
	return out
}

// WalkedCommit is one commit in a history walk.
type WalkedCommit struct {
	SHA     string
	Message string
	Tree    string
	Parents []string
}

// WalkCommits returns commits reachable from a tip, newest first
// (DFS, first-parent order).
func WalkCommits(repo *Repo, tip string) []WalkedCommit {
	out := []WalkedCommit{}
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := GetObject(repo, oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		parsed := ParseCommit(obj.Data)
		out = append(out, WalkedCommit{SHA: oid, Message: parsed.Message, Tree: parsed.Tree, Parents: parsed.Parents})
		for i := len(parsed.Parents) - 1; i >= 0; i-- {
			stack = append(stack, parsed.Parents[i])
		}
	}
	return out
}

// AncestorsOf returns the set of commit oids reachable from tip.
func AncestorsOf(repo *Repo, tip string) map[string]bool {
	out := make(map[string]bool)
	for _, c := range WalkCommits(repo, tip) {
		out[c.SHA] = true
	}
	return out
}

// MergeBase returns the first common ancestor of a and b (walking a's
// history newest-first), or "" when there is none.
func MergeBase(repo *Repo, a, b string) string {
	bAncestors := AncestorsOf(repo, b)
	for _, commit := range WalkCommits(repo, a) {
		if bAncestors[commit.SHA] {
			return commit.SHA
		}
	}
	return ""
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// LineDiffCounts counts added/deleted lines between two file contents
// by line-multiset difference (exactly as core.ts).
func LineDiffCounts(oldContent, newContent []byte) (additions, deletions int) {
	oldLines := splitLines(string(oldContent))
	newLines := splitLines(string(newContent))
	oldCounts := make(map[string]int)
	for _, line := range oldLines {
		oldCounts[line]++
	}
	for _, line := range newLines {
		if oldCounts[line] > 0 {
			oldCounts[line]--
		} else {
			additions++
		}
	}
	remaining := make(map[string]int)
	for _, line := range newLines {
		remaining[line]++
	}
	for _, line := range oldLines {
		if remaining[line] > 0 {
			remaining[line]--
		} else {
			deletions++
		}
	}
	return additions, deletions
}

// DiffFiles diffs base files -> head files (both path -> {oid, content}),
// sorted by filename.
func DiffFiles(baseFiles, headFiles map[string]FileEntry) []FileDiff {
	out := []FileDiff{}
	pathSet := make(map[string]bool)
	for p := range baseFiles {
		pathSet[p] = true
	}
	for p := range headFiles {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	empty := []byte{}
	for _, path := range paths {
		base, hasBase := baseFiles[path]
		head, hasHead := headFiles[path]
		switch {
		case hasBase && !hasHead:
			_, dels := LineDiffCounts(base.Content, empty)
			out = append(out, FileDiff{Filename: path, Status: "removed", Additions: 0, Deletions: dels, Changes: dels})
		case !hasBase && hasHead:
			adds, _ := LineDiffCounts(empty, head.Content)
			out = append(out, FileDiff{Filename: path, Status: "added", Additions: adds, Deletions: 0, Changes: adds})
		case hasBase && hasHead && base.OID != head.OID:
			adds, dels := LineDiffCounts(base.Content, head.Content)
			out = append(out, FileDiff{Filename: path, Status: "modified", Additions: adds, Deletions: dels, Changes: adds + dels})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Issues / pulls (shared by REST, GraphQL, MCP so all three see one store)
// ---------------------------------------------------------------------------

// CreateIssueInput is the input to CreateIssueInternal. Callers extract
// JSON values with typeof checks first (non-string title/body -> "",
// non-array labels -> nil), mirroring core.ts.
type CreateIssueInput struct {
	Title  string
	Body   string
	Labels []string
}

// CreateIssueInternal creates an issue. Caller must hold store.Mu (write).
func CreateIssueInternal(store *Store, repo *Repo, input CreateIssueInput) (*Issue, error) {
	EnsureRepoMaps(repo)
	if input.Title == "" {
		return nil, Err(422, "title is required")
	}
	for _, name := range input.Labels {
		if _, ok := repo.Labels[name]; !ok {
			return nil, Err(422, "label does not exist: "+name)
		}
	}
	number := repo.NextIssue
	repo.NextIssue++
	stamp := NowIso()
	issue := &Issue{
		ID:        number,
		Number:    number,
		Title:     input.Title,
		Body:      input.Body,
		State:     "open",
		Labels:    append([]string{}, input.Labels...),
		User:      Viewer,
		CreatedAt: stamp,
		UpdatedAt: stamp,
		Comments:  []*Comment{},
	}
	repo.Issues[number] = issue
	return issue, nil
}

// CreatePullInput is the input to CreatePullInternal.
type CreatePullInput struct {
	Head  string
	Base  string
	Title string
	Body  string
}

// CreatePullInternal creates a pull request. Caller must hold store.Mu
// (write).
func CreatePullInternal(store *Store, repo *Repo, input CreatePullInput) (*Pull, error) {
	EnsureRepoMaps(repo)
	head := StripBranchOwner(input.Head)
	base := StripBranchOwner(input.Base)
	if head == "" || base == "" {
		return nil, Err(422, "head and base are required")
	}
	headSha := BranchTip(repo, head)
	baseSha := BranchTip(repo, base)
	if headSha == "" {
		return nil, Err(422, "head branch does not exist: "+head)
	}
	if baseSha == "" {
		return nil, Err(422, "base branch does not exist: "+base)
	}
	if input.Title == "" {
		return nil, Err(422, "title is required")
	}
	for _, existing := range repo.Pulls {
		if existing.State == "open" && existing.Head.Ref == head && existing.Base.Ref == base {
			return nil, Err(422, "a pull request already exists for this head and base")
		}
	}
	number := repo.NextPull
	repo.NextPull++
	files := DiffFiles(FilesAtCommit(repo, baseSha), FilesAtCommit(repo, headSha))
	stamp := NowIso()
	pull := &Pull{
		ID:        number,
		Number:    number,
		Title:     input.Title,
		Body:      input.Body,
		State:     "open",
		Labels:    []string{},
		User:      Viewer,
		CreatedAt: stamp,
		UpdatedAt: stamp,
		Head:      RefTip{Ref: head, SHA: headSha},
		Base:      RefTip{Ref: base, SHA: baseSha},
		Merged:    false,
		Files:     files,
	}
	repo.Pulls[number] = pull
	return pull, nil
}

// StripBranchOwner strips an "owner:" prefix from a branch ref.
func StripBranchOwner(ref string) string {
	if idx := strings.Index(ref, ":"); idx >= 0 {
		return ref[idx+1:]
	}
	return ref
}

// ConflictPaths returns paths changed differently on both sides vs the
// merge base (sorted, for determinism).
func ConflictPaths(repo *Repo, pull *Pull) []string {
	headSha := pull.Head.SHA
	baseSha := pull.Base.SHA
	if headSha == "" || baseSha == "" {
		return []string{}
	}
	currentBaseSha := BranchTip(repo, pull.Base.Ref)
	if currentBaseSha == "" {
		currentBaseSha = baseSha
	}
	base := MergeBase(repo, headSha, currentBaseSha)
	baseFiles := make(map[string]FileEntry)
	if base != "" {
		baseFiles = FilesAtCommit(repo, base)
	}
	headFiles := FilesAtCommit(repo, headSha)
	tipFiles := FilesAtCommit(repo, currentBaseSha)
	pathSet := make(map[string]bool)
	for p := range baseFiles {
		pathSet[p] = true
	}
	for p := range headFiles {
		pathSet[p] = true
	}
	for p := range tipFiles {
		pathSet[p] = true
	}
	conflicts := []string{}
	for path := range pathSet {
		b := baseFiles[path].OID
		h := headFiles[path].OID
		t := tipFiles[path].OID
		if h != b && t != b && h != t {
			conflicts = append(conflicts, path)
		}
	}
	sort.Strings(conflicts)
	return conflicts
}

// PullMergeable reports whether a pull request is mergeable right now:
// "MERGEABLE", "CONFLICTING", or "UNKNOWN".
func PullMergeable(repo *Repo, pull *Pull) string {
	if pull == nil || pull.State != "open" {
		return "UNKNOWN"
	}
	if len(ConflictPaths(repo, pull)) > 0 {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// MergePullInternal squash-merges a pull request onto its base branch
// (single-parent merge commit, as core.ts). mergeMethod is accepted for
// parity and ignored. Returns the merge commit sha and message. Caller
// must hold store.Mu (write).
func MergePullInternal(repo *Repo, pull *Pull, mergeMethod string) (string, string, error) {
	if pull == nil {
		return "", "", Err(404, "pull request not found")
	}
	if pull.State != "open" || pull.Merged {
		return "", "", Err(405, "pull request is already merged or closed")
	}
	if conflicts := ConflictPaths(repo, pull); len(conflicts) > 0 {
		return "", "", Err(405, "merge conflict in "+strings.Join(conflicts, ", "))
	}
	headSha := pull.Head.SHA
	baseBranch := pull.Base.Ref
	baseTip := BranchTip(repo, baseBranch)
	if baseTip == "" {
		return "", "", Err(422, "base branch does not exist: "+baseBranch)
	}
	base := MergeBase(repo, headSha, baseTip)
	baseFiles := make(map[string]FileEntry)
	if base != "" {
		baseFiles = FilesAtCommit(repo, base)
	}
	headFiles := FilesAtCommit(repo, headSha)
	tipFiles := FilesAtCommit(repo, baseTip)
	// Three-way merge: head wins where only it changed, base-tip wins
	// where only it changed.
	merged := make(map[string][]byte)
	pathSet := make(map[string]bool)
	for p := range baseFiles {
		pathSet[p] = true
	}
	for p := range headFiles {
		pathSet[p] = true
	}
	for p := range tipFiles {
		pathSet[p] = true
	}
	for path := range pathSet {
		b := baseFiles[path].OID
		h, hasH := headFiles[path]
		t, hasT := tipFiles[path]
		var chosen *FileEntry
		switch {
		case h.OID == b:
			if hasT {
				chosen = &t
			}
		case t.OID == b:
			if hasH {
				chosen = &h
			}
		default:
			// h.OID == t.OID, or unreachable (conflicts refused above).
			if hasH {
				chosen = &h
			}
		}
		if chosen != nil {
			merged[path] = chosen.Content
		}
	}
	treeOid := BuildTree(repo, merged)
	message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", pull.Number, pull.Head.Ref, pull.Title)
	mergeSha := CreateCommit(repo, treeOid, []string{baseTip}, message)
	SetRef(repo, BranchRef(baseBranch), mergeSha)
	pull.State = "closed"
	pull.Merged = true
	mergedAt := NowIso()
	pull.MergedAt = &mergedAt
	shaCopy := mergeSha
	pull.MergeCommitSHA = &shaCopy
	pull.UpdatedAt = NowIso()
	return mergeSha, message, nil
}

// AddCommentInternal appends a comment to an issue. Caller must hold
// store.Mu (write).
func AddCommentInternal(repo *Repo, issue *Issue, body string) (*Comment, error) {
	if issue == nil {
		return nil, Err(404, "issue not found")
	}
	if body == "" {
		return nil, Err(422, "body is required")
	}
	comment := &Comment{
		ID:        len(issue.Comments) + 1,
		Body:      body,
		User:      Viewer,
		CreatedAt: NowIso(),
		UpdatedAt: NowIso(),
	}
	issue.Comments = append(issue.Comments, comment)
	return comment, nil
}

// RefreshPullFiles recomputes a pull request's file list from its
// current head/base tips. Mutates pull: caller must hold store.Mu (write).
func RefreshPullFiles(repo *Repo, pull *Pull) {
	headFiles := make(map[string]FileEntry)
	if pull.Head.SHA != "" {
		headFiles = FilesAtCommit(repo, pull.Head.SHA)
	}
	baseFiles := make(map[string]FileEntry)
	if pull.Base.SHA != "" {
		baseFiles = FilesAtCommit(repo, pull.Base.SHA)
	}
	pull.Files = DiffFiles(baseFiles, headFiles)
}
