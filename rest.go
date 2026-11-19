package main

// REST v3 surface: POST/GET under /api/v3 — a faithful port of rest.ts.
// Exported handler: HandleRest(w, r, store, token). Plus RepoToJSON /
// IssueToJSON / PullToJSON helpers (also used by MCP).
//
// Concurrency (CONTRACT.md): the request body is read before locking;
// mutations run under store.Mu.Lock(), pure reads under RLock; the full
// response bytes are built under the lock and written after unlocking.

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const scopesValue = "repo, read:org"

func scopesHeaders() map[string]string {
	return map[string]string{"x-oauth-scopes": scopesValue}
}

// ---------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------

// RepoToJSON is the REST repo object. Caller must hold store.Mu; if the
// repo's database id is unassigned the caller must hold the write lock
// (repos created via CreateRepoRecord always have one).
func RepoToJSON(store *Store, repo *Repo) map[string]any {
	created := repo.CreatedAt
	if created == "" {
		created = NowIso()
	}
	updated := repo.UpdatedAt
	if updated == "" {
		updated = created
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return map[string]any{
		"id":                DatabaseID(store, repo),
		"node_id":           RepoGlobalID(repo),
		"name":              repo.Name,
		"full_name":         repo.Owner + "/" + repo.Name,
		"owner":             map[string]any{"login": repo.Owner, "id": 1, "type": "User"},
		"private":           repo.Private,
		"description":       repo.Description,
		"default_branch":    defaultBranch,
		"html_url":          "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		"url":               "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		"created_at":        created,
		"updated_at":        updated,
		"has_issues":        true,
		"has_pull_requests": true,
		"fork":              false,
		"archived":          false,
		"disabled":          false,
		"visibility":        visibility,
	}
}

// IssueToJSON is the REST issue object. Same locking note as RepoToJSON.
func IssueToJSON(store *Store, repo *Repo, issue *Issue) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		labels = append(labels, map[string]any{"name": name, "color": "ededed"})
	}
	userLogin := issue.User
	if userLogin == "" {
		userLogin = Viewer
	}
	created := issue.CreatedAt
	if created == "" {
		created = NowIso()
	}
	updated := issue.UpdatedAt
	if updated == "" {
		updated = NowIso()
	}
	return map[string]any{
		"id":         DatabaseID(store, repo)*1000000 + issue.Number,
		"node_id":    IssueGlobalID(repo, issue.Number),
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": userLogin},
		"created_at": created,
		"updated_at": updated,
		"comments":   len(issue.Comments),
		"html_url":   fmt.Sprintf("https://example.invalid/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number),
		"url":        fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number),
	}
}

// ---------------------------------------------------------------------------
// Fast issue serialisation (the read/write workloads' hot path)
// ---------------------------------------------------------------------------

// needsJSONEscape mirrors rest.ts NEEDS_JSON_ESCAPE (quote, backslash,
// C0 controls, DEL and C1 controls; lone surrogates cannot occur in a
// Go UTF-8 string).
func needsJSONEscape(s string) bool {
	for _, c := range s {
		if c == '"' || c == '\\' || c < 0x20 || (c >= 0x7f && c <= 0x9f) {
			return true
		}
	}
	return false
}

// jsonStr is a JSON string literal for s, without a full marshal when
// nothing needs escaping.
func jsonStr(s string) string {
	if !needsJSONEscape(s) {
		return "\"" + s + "\""
	}
	b, err := MarshalJSON(s)
	if err != nil {
		return "\"\""
	}
	return string(b)
}

// IssueToJSONText is IssueToJSON as a string, built directly in TS key
// order. Same locking note as RepoToJSON.
func IssueToJSONText(store *Store, repo *Repo, issue *Issue) string {
	var labelsText string
	if len(issue.Labels) == 0 {
		labelsText = "[]"
	} else {
		parts := make([]string, len(issue.Labels))
		for i, name := range issue.Labels {
			parts[i] = "{\"name\":" + jsonStr(name) + ",\"color\":\"ededed\"}"
		}
		labelsText = "[" + strings.Join(parts, ",") + "]"
	}
	created := issue.CreatedAt
	if created == "" {
		created = NowIso()
	}
	updated := issue.UpdatedAt
	if updated == "" {
		updated = NowIso()
	}
	userLogin := issue.User
	if userLogin == "" {
		userLogin = Viewer
	}
	var b strings.Builder
	b.WriteString("{\"id\":")
	b.WriteString(strconv.Itoa(DatabaseID(store, repo)*1000000 + issue.Number))
	b.WriteString(",\"node_id\":")
	b.WriteString(jsonStr(IssueGlobalID(repo, issue.Number)))
	b.WriteString(",\"number\":")
	b.WriteString(strconv.Itoa(issue.Number))
	b.WriteString(",\"title\":")
	b.WriteString(jsonStr(issue.Title))
	b.WriteString(",\"body\":")
	b.WriteString(jsonStr(issue.Body))
	b.WriteString(",\"state\":")
	b.WriteString(jsonStr(issue.State))
	b.WriteString(",\"labels\":")
	b.WriteString(labelsText)
	b.WriteString(",\"user\":{\"login\":")
	b.WriteString(jsonStr(userLogin))
	b.WriteString("},\"created_at\":")
	b.WriteString(jsonStr(created))
	b.WriteString(",\"updated_at\":")
	b.WriteString(jsonStr(updated))
	b.WriteString(",\"comments\":")
	b.WriteString(strconv.Itoa(len(issue.Comments)))
	b.WriteString(",\"html_url\":")
	b.WriteString(jsonStr(fmt.Sprintf("https://example.invalid/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number)))
	b.WriteString(",\"url\":")
	b.WriteString(jsonStr(fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number)))
	b.WriteString("}")
	return b.String()
}

type issueCacheEntry struct {
	buf         []byte
	title       string
	body        string
	state       string
	createdAt   string
	updatedAt   string
	labels      []string
	commentsLen int
	userLogin   string
	dbid        int
}

type issueCache struct {
	entries map[int]*issueCacheEntry
	order   []int // insertion order, for oldest-first eviction
}

// The serialized-issue cache lives outside Repo (store.go's Repo has no
// cache field, as rest.ts hung _issueJsonCache off the repo object).
// Guarded by its own mutex so reads under store.Mu.RLock stay race-free;
// callers must still hold store.Mu (read or write) while calling, and
// the repo's database id must already be assigned on the read path.
var (
	issueCacheMu sync.Mutex
	issueCaches  = make(map[*Repo]*issueCache)
)

func labelsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CachedIssueBuffer returns the serialized GET-one-issue body (with
// trailing newline), cached per issue and rebuilt only when a field the
// JSON carries actually changed — validated by value, so every mutation
// path (PATCH, comments, GraphQL closes) is covered without explicit
// invalidation, exactly as rest.ts cachedIssueBuffer.
func CachedIssueBuffer(store *Store, repo *Repo, issue *Issue) []byte {
	userLogin := issue.User
	if userLogin == "" {
		userLogin = Viewer
	}
	issueCacheMu.Lock()
	cache, ok := issueCaches[repo]
	if !ok {
		cache = &issueCache{entries: make(map[int]*issueCacheEntry)}
		issueCaches[repo] = cache
	}
	if entry, ok := cache.entries[issue.Number]; ok &&
		entry.title == issue.Title &&
		entry.body == issue.Body &&
		entry.state == issue.State &&
		entry.createdAt == issue.CreatedAt &&
		entry.updatedAt == issue.UpdatedAt &&
		labelsEqual(entry.labels, issue.Labels) &&
		entry.commentsLen == len(issue.Comments) &&
		entry.userLogin == userLogin &&
		entry.dbid == repo.DBID {
		buf := entry.buf
		issueCacheMu.Unlock()
		return buf
	}
	issueCacheMu.Unlock()

	buf := []byte(IssueToJSONText(store, repo, issue) + "\n")

	issueCacheMu.Lock()
	if _, exists := cache.entries[issue.Number]; !exists {
		if len(cache.entries) >= 512 && len(cache.order) > 0 {
			oldest := cache.order[0]
			cache.order = cache.order[1:]
			delete(cache.entries, oldest)
		}
		cache.order = append(cache.order, issue.Number)
	}
	cache.entries[issue.Number] = &issueCacheEntry{
		buf:         buf,
		title:       issue.Title,
		body:        issue.Body,
		state:       issue.State,
		createdAt:   issue.CreatedAt,
		updatedAt:   issue.UpdatedAt,
		labels:      append([]string{}, issue.Labels...),
		commentsLen: len(issue.Comments),
		userLogin:   userLogin,
		dbid:        repo.DBID,
	}
	issueCacheMu.Unlock()
	return buf
}

// PullToJSON is the REST pull request object. Same locking note as
// RepoToJSON.
func PullToJSON(store *Store, repo *Repo, pull *Pull) map[string]any {
	var mergedAt any
	if pull.MergedAt != nil {
		mergedAt = *pull.MergedAt
	}
	var mergeSHA any
	if pull.MergeCommitSHA != nil {
		mergeSHA = *pull.MergeCommitSHA
	}
	created := pull.CreatedAt
	if created == "" {
		created = NowIso()
	}
	updated := pull.UpdatedAt
	if updated == "" {
		updated = NowIso()
	}
	return map[string]any{
		"id":               DatabaseID(store, repo)*1000000 + pull.Number,
		"node_id":          PullGlobalID(repo, pull.Number),
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"head":             map[string]any{"ref": pull.Head.Ref, "sha": pull.Head.SHA, "label": repo.Owner + ":" + pull.Head.Ref},
		"base":             map[string]any{"ref": pull.Base.Ref, "sha": pull.Base.SHA, "label": repo.Owner + ":" + pull.Base.Ref},
		"user":             map[string]any{"login": Viewer},
		"created_at":       created,
		"updated_at":       updated,
		"html_url":         fmt.Sprintf("https://example.invalid/%s/%s/pull/%d", repo.Owner, repo.Name, pull.Number),
		"url":              fmt.Sprintf("/api/v3/repos/%s/%s/pulls/%d", repo.Owner, repo.Name, pull.Number),
	}
}

// ---------------------------------------------------------------------------
// Response plumbing: build bytes under the lock, write after unlocking.
// ---------------------------------------------------------------------------

type restResponse struct {
	status  int
	headers map[string]string
	body    []byte // nil for 204
}

func jsonResponse(status int, v any, extra map[string]string) restResponse {
	if status == 204 {
		return restResponse{status: status, headers: extra}
	}
	return restResponse{status: status, headers: extra, body: marshalJSONLine(v)}
}

func jsonTextResponse(status int, text string, extra map[string]string) restResponse {
	return restResponse{status: status, headers: extra, body: []byte(text)}
}

func jsonBufferResponse(status int, buf []byte, extra map[string]string) restResponse {
	return restResponse{status: status, headers: extra, body: buf}
}

// errorResponse mirrors sendError: {"message": ...}, no scopes header.
func errorResponse(status int, message string) restResponse {
	if message == "" {
		message = "error"
	}
	return jsonResponse(status, map[string]any{"message": message}, nil)
}

func errorResponseFromErr(err error) restResponse {
	if he, ok := AsHttpError(err); ok {
		return errorResponse(he.Status, he.Message)
	}
	msg := err.Error()
	if msg == "" {
		msg = "internal error"
	}
	return errorResponse(500, msg)
}

func writeRestResponse(w http.ResponseWriter, resp restResponse) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-Id", RequestID())
	for k, v := range resp.headers {
		h.Set(k, v)
	}
	if resp.status != 204 {
		h.Set("Content-Length", strconv.Itoa(len(resp.body)))
	}
	w.WriteHeader(resp.status)
	if resp.status != 204 && len(resp.body) > 0 {
		_, _ = w.Write(resp.body)
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func decodeSegment(segment string) string {
	if decoded, err := url.PathUnescape(segment); err == nil {
		return decoded
	}
	return segment
}

// pathSegments mirrors rest.ts pathSegments over the raw (escaped) path:
// split on '/', drop empties, percent-decode each segment.
func pathSegments(r *http.Request) []string {
	pathname := r.URL.EscapedPath()
	var out []string
	for _, s := range strings.Split(pathname, "/") {
		if s != "" {
			out = append(out, decodeSegment(s))
		}
	}
	return out
}

func originOf(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return "http://" + host
}

// paginate slices items per ?page/per_page and returns the GitHub-style
// Link header value ("" when no more pages remain).
func paginate[T any](r *http.Request, items []T, query url.Values) ([]T, string) {
	perPage := 30
	if raw, ok := query["per_page"]; ok && len(raw) > 0 {
		if f, ok := parseJSNumber(raw[0]); ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 {
			perPage = int(math.Trunc(f))
			if perPage > 100 {
				perPage = 100
			}
			if perPage < 1 {
				perPage = 30
			}
		}
	}
	page := 1
	if raw, ok := query["page"]; ok && len(raw) > 0 {
		if f, ok := parseJSNumber(raw[0]); ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 {
			page = int(math.Trunc(f))
			if page < 1 {
				page = 1
			}
		}
	}
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start := (page - 1) * perPage
	if start > len(items) {
		start = len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	link := ""
	if page < totalPages {
		basePath := r.URL.EscapedPath()
		origin := originOf(r)
		next := fmt.Sprintf("%s%s?page=%d&per_page=%d", origin, basePath, page+1, perPage)
		last := fmt.Sprintf("%s%s?page=%d&per_page=%d", origin, basePath, totalPages, perPage)
		link = fmt.Sprintf("<%s>; rel=\"next\", <%s>; rel=\"last\"", next, last)
	}
	return slice, link
}

func withLink(headers map[string]string, link string) map[string]string {
	if link != "" {
		headers["link"] = link
	}
	return headers
}

// repoForRead locks store.Mu for reading, looks up the repo, and
// guarantees its database id is assigned (upgrading to the write lock
// for that one assignment). Returns the repo and the unlock func.
func repoForRead(store *Store, owner, name string) (*Repo, func(), error) {
	store.Mu.RLock()
	repo := store.GetRepo(owner, name)
	if repo == nil {
		store.Mu.RUnlock()
		return nil, nil, Err(404, "Not Found")
	}
	if repo.DBID == 0 {
		store.Mu.RUnlock()
		store.Mu.Lock()
		repo = store.GetRepo(owner, name)
		if repo == nil {
			store.Mu.Unlock()
			return nil, nil, Err(404, "Not Found")
		}
		EnsureRepoMaps(repo)
		DatabaseID(store, repo)
		return repo, store.Mu.Unlock, nil
	}
	return repo, store.Mu.RUnlock, nil
}

// repoForWrite locks store.Mu for writing, looks up the repo, ensures
// its maps and database id. Returns the repo and the unlock func.
func repoForWrite(store *Store, owner, name string) (*Repo, func(), error) {
	store.Mu.Lock()
	repo := store.GetRepo(owner, name)
	if repo == nil {
		store.Mu.Unlock()
		return nil, nil, Err(404, "Not Found")
	}
	EnsureRepoMaps(repo)
	DatabaseID(store, repo)
	return repo, store.Mu.Unlock, nil
}

// jsonObjectBody parses raw as a JSON object body. Non-object JSON
// (arrays, scalars) reads as an empty object, as property access on
// them yields undefined in TS.
func jsonObjectBody(raw []byte) (map[string]any, error) {
	v, err := ParseJSONBody(raw)
	if err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{}, nil
}

// bodyString is `typeof body[key] === 'string' ? body[key] : undefined`.
func bodyString(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

func bodyStringOr(m map[string]any, key, def string) string {
	if s, ok := bodyString(m, key); ok {
		return s
	}
	return def
}

// bodyLabels is Array.isArray(body[key]) ? body[key] : []. Non-string
// elements stringify, so the label-exists check rejects them as in TS.
func bodyLabels(m map[string]any, key string) []string {
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(arr))
	for i, el := range arr {
		if s, ok := el.(string); ok {
			out[i] = s
		} else {
			out[i] = fmt.Sprint(el)
		}
	}
	return out
}

// decodeBase64Lenient decodes like Node's Buffer.from(s, 'base64'):
// characters outside the alphabet are ignored and missing padding is
// tolerated. (Node never throws here; the error return is a fallback.)
func decodeBase64Lenient(s string) ([]byte, error) {
	var clean strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/':
			clean.WriteByte(c)
		case c == '-':
			clean.WriteByte('+')
		case c == '_':
			clean.WriteByte('/')
		}
	}
	str := clean.String()
	if m := len(str) % 4; m != 0 {
		str += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(str)
}

// ---------------------------------------------------------------------------
// Contents (git-backed)
// ---------------------------------------------------------------------------

func commitContents(repo *Repo, branch, path string, content []byte, message, expectedSha string, hasExpected bool) (string, string, int, error) {
	tip := BranchTip(repo, branch)
	files := make(map[string]FileEntry)
	if tip != "" {
		files = FilesAtCommit(repo, tip)
	}
	existing, hasExisting := files[path]
	if hasExpected {
		if !hasExisting || !strings.EqualFold(existing.OID, expectedSha) {
			return "", "", 0, Err(409, "sha does not match the current blob")
		}
	}
	blobOid := PutBlob(repo, content)
	next := make(map[string][]byte, len(files)+1)
	for p, entry := range files {
		next[p] = entry.Content
	}
	next[path] = content
	treeOid := BuildTree(repo, next)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitSha := CreateCommit(repo, treeOid, parents, message)
	SetRef(repo, BranchRef(branch), commitSha)
	status := 201
	if hasExisting {
		status = 200
	}
	return blobOid, commitSha, status, nil
}

func contentsFileJSON(path string, entry FileEntry) map[string]any {
	name := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		name = path[idx+1:]
	}
	return map[string]any{
		"type":     "file",
		"encoding": "base64",
		"size":     len(entry.Content),
		"name":     name,
		"path":     path,
		"content":  base64.StdEncoding.EncodeToString(entry.Content),
		"sha":      entry.OID,
	}
}

func contentsAtRef(repo *Repo, ref string) map[string]FileEntry {
	tip := ResolveCommit(repo, ref)
	if tip == "" {
		return make(map[string]FileEntry)
	}
	return FilesAtCommit(repo, tip)
}

// directoryListing lists immediate children of dir ("" = root). It
// builds subtree objects via BuildTree (as rest.ts), so the caller must
// hold the write lock.
func directoryListing(repo *Repo, dir string, files map[string]FileEntry) []any {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seenFiles := make(map[string]FileEntry)
	seenDirs := make(map[string]bool)
	for path, entry := range files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		if rest == "" {
			continue
		}
		if slash := strings.Index(rest, "/"); slash == -1 {
			seenFiles[rest] = entry
		} else {
			seenDirs[rest[:slash]] = true
		}
	}
	out := []any{}
	dirNames := make([]string, 0, len(seenDirs))
	for name := range seenDirs {
		dirNames = append(dirNames, name)
	}
	sort.Strings(dirNames)
	for _, name := range dirNames {
		dirPath := prefix + name
		sub := make(map[string][]byte)
		for path, entry := range files {
			if strings.HasPrefix(path, dirPath+"/") {
				sub[path[len(dirPath)+1:]] = entry.Content
			}
		}
		out = append(out, map[string]any{"type": "dir", "name": name, "path": dirPath, "sha": BuildTree(repo, sub)})
	}
	fileNames := make([]string, 0, len(seenFiles))
	for name := range seenFiles {
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)
	for _, name := range fileNames {
		entry := seenFiles[name]
		out = append(out, map[string]any{"type": "file", "name": name, "path": prefix + name, "sha": entry.OID, "size": len(entry.Content)})
	}
	return out
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// HandleRest serves the REST v3 surface under /api/v3.
func HandleRest(w http.ResponseWriter, r *http.Request, store *Store, token string) {
	if r.Method == http.MethodGet && tryServeCachedIssue(w, r, store, token) {
		return
	}
	if err := RequireAuth(r, token); err != nil {
		writeRestResponse(w, errorResponseFromErr(err))
		return
	}
	// Read the body before locking. GET routes never read a body in
	// rest.ts, so skip the read (and its 413) for them.
	var raw []byte
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		body, err := ReadRawBody(r)
		if err != nil {
			writeRestResponse(w, errorResponseFromErr(err))
			return
		}
		raw = body
	}
	resp, err := routeRest(r, store, raw)
	if err != nil {
		writeRestResponse(w, errorResponseFromErr(err))
		return
	}
	writeRestResponse(w, resp)
}

// tryServeCachedIssue is the fast path for
// GET /api/v3/repos/{owner}/{repo}/issues/{n} with no query string —
// the benchmark's read workload. Returns true when it answered.
func tryServeCachedIssue(w http.ResponseWriter, r *http.Request, store *Store, token string) bool {
	if token == "" {
		return false
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		return false
	}
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		return false
	}
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, "/api/v3/repos/") {
		return false
	}
	parts := strings.Split(path[len("/api/v3/repos/"):], "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return false
	}
	owner, name, numStr := parts[0], parts[1], parts[3]
	if owner == "" || name == "" || !isAllDigits(numStr) {
		return false
	}
	if strings.Contains(owner, "%") || strings.Contains(name, "%") {
		return false
	}
	num, err := strconv.Atoi(numStr)
	if err != nil {
		return false
	}
	store.Mu.RLock()
	repo := store.GetRepo(owner, name)
	if repo == nil || repo.DBID == 0 {
		store.Mu.RUnlock()
		return false
	}
	issue := repo.Issues[num]
	if issue == nil {
		store.Mu.RUnlock()
		return false
	}
	resp := jsonBufferResponse(200, CachedIssueBuffer(store, repo, issue), scopesHeaders())
	store.Mu.RUnlock()
	writeRestResponse(w, resp)
	return true
}

func routeRest(r *http.Request, store *Store, raw []byte) (restResponse, error) {
	method := r.Method
	segments := pathSegments(r)
	query := r.URL.Query()

	// /api/v3 (root)
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "v3" {
		if method != http.MethodGet {
			return restResponse{}, Err(404, "Not Found")
		}
		return jsonResponse(200, map[string]any{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		}, scopesHeaders()), nil
	}
	if len(segments) < 3 || segments[0] != "api" || segments[1] != "v3" {
		return restResponse{}, Err(404, "Not Found")
	}
	rest := segments[2:]

	// GET /meta
	if len(rest) == 1 && rest[0] == "meta" {
		if method != http.MethodGet {
			return restResponse{}, Err(404, "Not Found")
		}
		return jsonResponse(200, map[string]any{
			"installed_version":                  "1.0.0",
			"verifiable_password_authentication": true,
		}, scopesHeaders()), nil
	}

	// GET /user
	if len(rest) == 1 && rest[0] == "user" {
		if method != http.MethodGet {
			return restResponse{}, Err(404, "Not Found")
		}
		return jsonResponse(200, map[string]any{
			"login":    Viewer,
			"id":       1,
			"type":     "User",
			"name":     Viewer,
			"html_url": "https://example.invalid/" + Viewer,
		}, scopesHeaders()), nil
	}

	// /user/repos
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		return routeUserRepos(r, store, raw, query, method)
	}

	// /repos/{owner}/{name}[...]
	if len(rest) >= 3 && rest[0] == "repos" {
		owner, name := rest[1], rest[2]
		tail := rest[3:]
		return routeRepo(r, store, raw, query, method, owner, name, tail)
	}

	return restResponse{}, Err(404, "Not Found")
}

func routeUserRepos(r *http.Request, store *Store, raw []byte, query url.Values, method string) (restResponse, error) {
	switch method {
	case http.MethodPost:
		body, err := jsonObjectBody(raw)
		if err != nil {
			return restResponse{}, err
		}
		name := ""
		if s, ok := bodyString(body, "name"); ok {
			name = strings.TrimSpace(s)
		}
		if name == "" {
			return restResponse{}, Err(422, "name is required")
		}
		store.Mu.Lock()
		defer store.Mu.Unlock()
		if store.GetRepo(Viewer, name) != nil {
			return restResponse{}, Err(422, "Repository creation failed: name already exists")
		}
		isPrivate := body["private"] == true // TS: body.private === true
		repo := CreateRepoRecord(store, Viewer, name, bodyStringOr(body, "description", ""), isPrivate)
		return jsonResponse(201, RepoToJSON(store, repo), scopesHeaders()), nil
	case http.MethodGet:
		store.Mu.RLock()
		all := store.ListRepos(Viewer, true)
		// RepoToJSON needs database ids; assign any missing under the
		// write lock (repos created via CreateRepoRecord have one).
		needIDs := false
		for _, repo := range all {
			if repo.DBID == 0 {
				needIDs = true
				break
			}
		}
		if needIDs {
			store.Mu.RUnlock()
			store.Mu.Lock()
			defer store.Mu.Unlock()
			all = store.ListRepos(Viewer, true)
			for _, repo := range all {
				EnsureRepoMaps(repo)
				DatabaseID(store, repo)
			}
		} else {
			defer store.Mu.RUnlock()
		}
		sort.Slice(all, func(i, j int) bool {
			return RepoKey(all[i].Owner, all[i].Name) < RepoKey(all[j].Owner, all[j].Name)
		})
		page, link := paginate(r, all, query)
		out := make([]any, 0, len(page))
		for _, repo := range page {
			out = append(out, RepoToJSON(store, repo))
		}
		return jsonResponse(200, out, withLink(scopesHeaders(), link)), nil
	}
	return restResponse{}, Err(404, "Not Found")
}

// restNeedsWriteLock reports whether a repo route mutates store state.
// GET contents (directoryListing stores subtree objects) and GET pull
// files (refreshPullFiles) mutate, as in rest.ts.
func restNeedsWriteLock(method string, tail []string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	if len(tail) >= 1 && tail[0] == "contents" {
		return true
	}
	if len(tail) == 3 && tail[0] == "pulls" && tail[2] == "files" {
		return true
	}
	return false
}

func routeRepo(r *http.Request, store *Store, raw []byte, query url.Values, method, owner, name string, tail []string) (restResponse, error) {
	var repo *Repo
	var unlock func()
	var err error
	if restNeedsWriteLock(method, tail) {
		repo, unlock, err = repoForWrite(store, owner, name)
	} else {
		repo, unlock, err = repoForRead(store, owner, name)
	}
	if err != nil {
		return restResponse{}, err
	}
	defer unlock()

	// /repos/{o}/{r}
	if len(tail) == 0 {
		switch method {
		case http.MethodGet:
			return jsonResponse(200, RepoToJSON(store, repo), scopesHeaders()), nil
		case http.MethodPatch:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			if s, ok := bodyString(body, "description"); ok {
				repo.Description = s
			}
			if b, ok := body["private"].(bool); ok {
				repo.Private = b
			}
			repo.UpdatedAt = NowIso()
			return jsonResponse(200, RepoToJSON(store, repo), scopesHeaders()), nil
		case http.MethodDelete:
			delete(store.Repos, RepoKey(owner, name))
			return restResponse{status: 204, headers: scopesHeaders()}, nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	head := tail[0]

	// POST /git/refs
	if head == "git" && len(tail) == 2 && tail[1] == "refs" {
		if method != http.MethodPost {
			return restResponse{}, Err(404, "Not Found")
		}
		body, err := jsonObjectBody(raw)
		if err != nil {
			return restResponse{}, err
		}
		ref := bodyStringOr(body, "ref", "")
		sha := bodyStringOr(body, "sha", "")
		if !strings.HasPrefix(ref, "refs/heads/") || !isHex40(sha) {
			return restResponse{}, Err(422, "ref and sha are required")
		}
		if _, exists := repo.Refs[ref]; exists {
			return restResponse{}, Err(422, "Reference already exists")
		}
		if _, exists := repo.Objects[sha]; !exists {
			return restResponse{}, Err(422, "sha does not exist")
		}
		repo.Refs[ref] = sha
		return jsonResponse(201, map[string]any{
			"ref":    ref,
			"object": map[string]any{"sha": sha, "type": "commit"},
		}, scopesHeaders()), nil
	}

	// GET /git/refs/heads/{branch...}
	if head == "git" && len(tail) >= 4 && tail[1] == "refs" && tail[2] == "heads" {
		if method != http.MethodGet {
			return restResponse{}, Err(404, "Not Found")
		}
		branch := strings.Join(tail[3:], "/")
		sha := BranchTip(repo, branch)
		if sha == "" {
			return restResponse{}, Err(404, "Not Found")
		}
		return jsonResponse(200, map[string]any{
			"ref":    "refs/heads/" + branch,
			"object": map[string]any{"sha": sha, "type": "commit"},
		}, scopesHeaders()), nil
	}

	// GET /branches and /branches/{branch...}
	if head == "branches" {
		if method != http.MethodGet {
			return restResponse{}, Err(404, "Not Found")
		}
		if len(tail) == 1 {
			var branches []string
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					branches = append(branches, ref[len("refs/heads/"):])
				}
			}
			sort.Strings(branches)
			out := make([]any, 0, len(branches))
			for _, branch := range branches {
				out = append(out, map[string]any{
					"name":   branch,
					"commit": map[string]any{"sha": repo.Refs[BranchRef(branch)]},
				})
			}
			return jsonResponse(200, out, scopesHeaders()), nil
		}
		branch := strings.Join(tail[1:], "/")
		sha := BranchTip(repo, branch)
		if sha == "" {
			return restResponse{}, Err(404, "Not Found")
		}
		return jsonResponse(200, map[string]any{
			"name":      branch,
			"commit":    map[string]any{"sha": sha},
			"protected": false,
		}, scopesHeaders()), nil
	}

	// GET /commits[?sha=<branch>]
	if head == "commits" && len(tail) == 1 {
		if method != http.MethodGet {
			return restResponse{}, Err(404, "Not Found")
		}
		shaParam := ""
		if vals, ok := query["sha"]; ok && len(vals) > 0 {
			shaParam = vals[0]
		}
		tip := ResolveCommit(repo, shaParam)
		var commits []WalkedCommit
		if tip != "" {
			commits = WalkCommits(repo, tip)
		}
		out := make([]any, 0, len(commits))
		for _, c := range commits {
			parents := make([]any, 0, len(c.Parents))
			for _, p := range c.Parents {
				parents = append(parents, map[string]any{"sha": p})
			}
			out = append(out, map[string]any{
				"sha": c.SHA,
				"commit": map[string]any{
					"message":   c.Message,
					"author":    map[string]any{"name": Viewer, "email": Viewer + "@example.invalid", "date": NowIso()},
					"committer": map[string]any{"name": Viewer, "email": Viewer + "@example.invalid", "date": NowIso()},
					"tree":      map[string]any{"sha": c.Tree},
				},
				"parents": parents,
			})
		}
		return jsonResponse(200, out, scopesHeaders()), nil
	}

	// /contents/{path...}
	if head == "contents" {
		path := strings.Join(tail[1:], "/")
		switch method {
		case http.MethodPut:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			message := bodyStringOr(body, "message", "update")
			contentB64 := bodyStringOr(body, "content", "")
			if contentB64 == "" {
				return restResponse{}, Err(422, "content is required")
			}
			content, err := decodeBase64Lenient(contentB64)
			if err != nil {
				return restResponse{}, Err(422, "content is not valid base64")
			}
			branch := repo.DefaultBranch
			if s, ok := bodyString(body, "branch"); ok && s != "" {
				branch = s
			}
			expectedSha, hasExpected := bodyString(body, "sha")
			contentSha, commitSha, status, err := commitContents(repo, branch, path, content, message, expectedSha, hasExpected)
			if err != nil {
				return restResponse{}, err
			}
			fileName := path
			if idx := strings.LastIndex(path, "/"); idx >= 0 {
				fileName = path[idx+1:]
			}
			return jsonResponse(status, map[string]any{
				"content": map[string]any{
					"name": fileName, "path": path, "sha": contentSha,
					"size": len(content), "encoding": "base64", "type": "file",
				},
				"commit": map[string]any{"sha": commitSha, "message": message},
			}, scopesHeaders()), nil
		case http.MethodGet:
			refParam := ""
			if vals, ok := query["ref"]; ok && len(vals) > 0 {
				refParam = vals[0]
			}
			files := contentsAtRef(repo, refParam)
			if path == "" {
				return jsonResponse(200, directoryListing(repo, "", files), scopesHeaders()), nil
			}
			if file, ok := files[path]; ok {
				return jsonResponse(200, contentsFileJSON(path, file), scopesHeaders()), nil
			}
			prefix := path + "/"
			isDir := false
			for p := range files {
				if strings.HasPrefix(p, prefix) {
					isDir = true
					break
				}
			}
			if isDir {
				return jsonResponse(200, directoryListing(repo, path, files), scopesHeaders()), nil
			}
			return restResponse{}, Err(404, "Not Found")
		case http.MethodDelete:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			message := bodyStringOr(body, "message", "delete")
			branch := repo.DefaultBranch
			if s, ok := bodyString(body, "branch"); ok && s != "" {
				branch = s
			}
			tip := BranchTip(repo, branch)
			if tip == "" {
				return restResponse{}, Err(404, "Not Found")
			}
			files := FilesAtCommit(repo, tip)
			if _, ok := files[path]; !ok {
				return restResponse{}, Err(404, "Not Found")
			}
			next := make(map[string][]byte, len(files))
			for p, entry := range files {
				if p != path {
					next[p] = entry.Content
				}
			}
			treeOid := BuildTree(repo, next)
			commitSha := CreateCommit(repo, treeOid, []string{tip}, message)
			SetRef(repo, BranchRef(branch), commitSha)
			return jsonResponse(200, map[string]any{
				"commit": map[string]any{"sha": commitSha, "message": message},
			}, scopesHeaders()), nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	// /labels
	if head == "labels" {
		switch method {
		case http.MethodPost:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			labelName := ""
			if s, ok := bodyString(body, "name"); ok {
				labelName = strings.TrimSpace(s)
			}
			if labelName == "" {
				return restResponse{}, Err(422, "name is required")
			}
			label := &Label{
				Name:        labelName,
				Color:       bodyStringOr(body, "color", "ededed"),
				Description: bodyStringOr(body, "description", ""),
			}
			repo.Labels[labelName] = label
			return jsonResponse(201, labelJSON(label), scopesHeaders()), nil
		case http.MethodGet:
			// rest.ts returns Map insertion order; Go maps are unordered,
			// so sort by name (the default labels are already in name
			// order, so a fresh repo's listing matches TS exactly).
			names := make([]string, 0, len(repo.Labels))
			for name := range repo.Labels {
				names = append(names, name)
			}
			sort.Strings(names)
			out := make([]any, 0, len(names))
			for _, name := range names {
				out = append(out, labelJSON(repo.Labels[name]))
			}
			return jsonResponse(200, out, scopesHeaders()), nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	// /issues
	if head == "issues" {
		return routeIssues(r, store, repo, raw, query, method, tail)
	}

	// /pulls
	if head == "pulls" {
		return routePulls(r, store, repo, raw, query, method, tail)
	}

	return restResponse{}, Err(404, "Not Found")
}

func labelJSON(label *Label) map[string]any {
	return map[string]any{
		"name":        label.Name,
		"color":       label.Color,
		"description": label.Description,
	}
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func routeIssues(r *http.Request, store *Store, repo *Repo, raw []byte, query url.Values, method string, tail []string) (restResponse, error) {
	if len(tail) == 1 {
		switch method {
		case http.MethodPost:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			issue, err := CreateIssueInternal(store, repo, CreateIssueInput{
				Title:  bodyStringOr(body, "title", ""),
				Body:   bodyStringOr(body, "body", ""),
				Labels: bodyLabels(body, "labels"),
			})
			if err != nil {
				return restResponse{}, err
			}
			return jsonTextResponse(201, IssueToJSONText(store, repo, issue)+"\n", scopesHeaders()), nil
		case http.MethodGet:
			state := "open"
			if vals, ok := query["state"]; ok && len(vals) > 0 {
				state = strings.ToLower(vals[0])
			}
			direction := "asc"
			if vals, ok := query["direction"]; ok && len(vals) > 0 {
				direction = strings.ToLower(vals[0])
			}
			sortKey := "created"
			if vals, ok := query["sort"]; ok && len(vals) > 0 {
				sortKey = strings.ToLower(vals[0])
			}
			var list []*Issue
			for _, issue := range repo.Issues {
				if state == "all" || issue.State == state {
					list = append(list, issue)
				}
			}
			// TS insertion order is number-ascending; sort by number,
			// reversing only for sort=created&direction=desc.
			sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
			if sortKey == "created" && direction == "desc" {
				sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
			}
			page, link := paginate(r, list, query)
			parts := make([]string, len(page))
			for i, issue := range page {
				parts[i] = IssueToJSONText(store, repo, issue)
			}
			return jsonTextResponse(200, "["+strings.Join(parts, ",")+"]\n", withLink(scopesHeaders(), link)), nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	issueNumber, ok := parseJSInt(tail[1])
	if !ok {
		return restResponse{}, Err(404, "Not Found")
	}
	issue := repo.Issues[issueNumber]

	if len(tail) == 2 {
		if issue == nil {
			return restResponse{}, Err(404, "Not Found")
		}
		switch method {
		case http.MethodGet:
			return jsonBufferResponse(200, CachedIssueBuffer(store, repo, issue), scopesHeaders()), nil
		case http.MethodPatch:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			if s, ok := bodyString(body, "state"); ok && (s == "open" || s == "closed") {
				issue.State = s
			}
			if s, ok := bodyString(body, "title"); ok {
				issue.Title = s
			}
			if s, ok := bodyString(body, "body"); ok {
				issue.Body = s
			}
			issue.UpdatedAt = NowIso()
			return jsonBufferResponse(200, CachedIssueBuffer(store, repo, issue), scopesHeaders()), nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	if len(tail) == 3 && tail[2] == "comments" {
		if issue == nil {
			return restResponse{}, Err(404, "Not Found")
		}
		switch method {
		case http.MethodPost:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			comment, err := AddCommentInternal(repo, issue, bodyStringOr(body, "body", ""))
			if err != nil {
				return restResponse{}, err
			}
			return jsonResponse(201, commentJSON(repo, issueNumber, comment), scopesHeaders()), nil
		case http.MethodGet:
			out := make([]any, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				out = append(out, commentJSON(repo, issueNumber, c))
			}
			return jsonResponse(200, out, scopesHeaders()), nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	return restResponse{}, Err(404, "Not Found")
}

func commentJSON(repo *Repo, issueNumber int, c *Comment) map[string]any {
	userLogin := c.User
	if userLogin == "" {
		userLogin = Viewer
	}
	return map[string]any{
		"id":         c.ID,
		"node_id":    CommentGlobalID(repo, issueNumber, c.ID),
		"body":       c.Body,
		"user":       map[string]any{"login": userLogin},
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
	}
}

func routePulls(r *http.Request, store *Store, repo *Repo, raw []byte, query url.Values, method string, tail []string) (restResponse, error) {
	if len(tail) == 1 {
		switch method {
		case http.MethodPost:
			body, err := jsonObjectBody(raw)
			if err != nil {
				return restResponse{}, err
			}
			pull, err := CreatePullInternal(store, repo, CreatePullInput{
				Head:  bodyStringOr(body, "head", ""),
				Base:  bodyStringOr(body, "base", ""),
				Title: bodyStringOr(body, "title", ""),
				Body:  bodyStringOr(body, "body", ""),
			})
			if err != nil {
				return restResponse{}, err
			}
			return jsonResponse(201, PullToJSON(store, repo, pull), scopesHeaders()), nil
		case http.MethodGet:
			state := "open"
			if vals, ok := query["state"]; ok && len(vals) > 0 {
				state = strings.ToLower(vals[0])
			}
			var list []*Pull
			for _, pull := range repo.Pulls {
				if state == "all" || pull.State == state {
					list = append(list, pull)
				}
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
			page, link := paginate(r, list, query)
			out := make([]any, 0, len(page))
			for _, pull := range page {
				out = append(out, PullToJSON(store, repo, pull))
			}
			return jsonResponse(200, out, withLink(scopesHeaders(), link)), nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	pullNumber, ok := parseJSInt(tail[1])
	if !ok {
		return restResponse{}, Err(404, "Not Found")
	}
	pull := repo.Pulls[pullNumber]

	if len(tail) == 2 {
		if pull == nil {
			return restResponse{}, Err(404, "Not Found")
		}
		if method == http.MethodGet {
			return jsonResponse(200, PullToJSON(store, repo, pull), scopesHeaders()), nil
		}
		return restResponse{}, Err(404, "Not Found")
	}

	if len(tail) == 3 && tail[2] == "files" {
		if pull == nil {
			return restResponse{}, Err(404, "Not Found")
		}
		if method != http.MethodGet {
			return restResponse{}, Err(404, "Not Found")
		}
		RefreshPullFiles(repo, pull)
		out := make([]any, 0, len(pull.Files))
		for _, f := range pull.Files {
			out = append(out, map[string]any{
				"filename":  f.Filename,
				"status":    f.Status,
				"additions": f.Additions,
				"deletions": f.Deletions,
				"changes":   f.Changes,
			})
		}
		return jsonResponse(200, out, scopesHeaders()), nil
	}

	if len(tail) == 3 && tail[2] == "merge" {
		if method != http.MethodPut {
			return restResponse{}, Err(404, "Not Found")
		}
		if pull == nil {
			return restResponse{}, Err(404, "Not Found")
		}
		// rest.ts: readJsonBody().catch(() => ({})) — malformed tolerated.
		body, _ := jsonObjectBody(raw)
		mergeMethod := ""
		if body != nil {
			mergeMethod = bodyStringOr(body, "merge_method", "")
		}
		sha, message, err := MergePullInternal(repo, pull, mergeMethod)
		if err != nil {
			return restResponse{}, err
		}
		return jsonResponse(200, map[string]any{
			"merged":  true,
			"sha":     sha,
			"message": message,
		}, scopesHeaders()), nil
	}

	return restResponse{}, Err(404, "Not Found")
}
