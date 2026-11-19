# Go port contract — github-forge-go

Port of ~/workspace/makefun/github-forge/ (Node TS, 120/120) to Go 1.23,
single package `main`, stdlib only. Toolchain: ~/workspace/tools/go1.23.4/bin/go
Build: CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
Binary MUST be <= 8 MiB (kit entry limit) and the whole repo tree <= 16 MiB.

## Files (one owner each — do not edit another owner's file)
- main.go, store.go — DONE by coordinator. Do not edit.
- core.go + rest.go — port of core.ts + rest.ts
- git.go — port of git.ts
- graphql.go + mcp.go + app.go — port of graphql.ts + mcp.ts + app.ts

## Shared types (store.go, already written)
Store{Mu sync.RWMutex, Repos map[string]*Repo, NextDBID int}
Repo{Owner,Name,Description,Private,DefaultBranch,CreatedAt,UpdatedAt,HeadTarget,
  Refs map[string]string, Objects map[string]*GitObject, Issues map[int]*Issue,
  Pulls map[int]*Pull, Labels map[string]*Label, NextIssue,NextPull,DBID int,
  PackCache map[string][]byte}
GitObject{Type string, Data []byte}
Label{Name,Color,Description string}
Comment{ID int, Body, User, CreatedAt, UpdatedAt string}
Issue{ID,Number int, Title,Body,State string, Labels []string, User string,
  CreatedAt,UpdatedAt string, Comments []*Comment}
RefTip{Ref,SHA string}
FileDiff{Filename,Status string, Additions,Deletions,Changes int}
Pull{ID,Number int, Title,Body,State string, Labels []string, User string,
  CreatedAt,UpdatedAt string, Head,Base RefTip, Merged bool, MergedAt *string,
  MergeCommitSHA *string, Files []FileDiff}
Funcs: NewStore(), (*Store).GetRepo/CreateRepo/ListRepos, RepoKey, NowIso(),
TrialNowIso() (string,bool), HashObject(type,data) string,
PutObject(repo,type,data) string, GetObject(repo,oid) *GitObject, DefaultLabels().

## Concurrency rule
Every handler must hold store.Mu while touching store state:
- mutations (create/update/delete/merge/push): store.Mu.Lock()
- pure reads (GET issue/repo, clone pack build): store.Mu.RLock()
Read the request body BEFORE locking. Write the response AFTER unlocking
(build the full response bytes under the lock, then write). This keeps the
50 concurrent clones parallel while staying race-free. Run `go vet` and,
if possible, `go build -race` for a smoke test.

## Handler signatures (app.go routes to these)
- HandleRest(w http.ResponseWriter, r *http.Request, store *Store, token string)
- HandleGraphql(w, r, store, token)
- HandleMcp(w, r, store, token)
- HandleGit(w, r, store, token) bool   // false = not a git route, fall through
- NewApp(store *Store) http.Handler     // in app.go; token from MAKEFUN_SHOP_TOKEN

Routing order in app.ts (post-improvement): GET /healthz -> 200 {"ok":true};
path == /api/v3 or prefix /api/v3/ -> REST; path == /api/graphql -> GraphQL;
HandleGit; path == /mcp or prefix /mcp/ -> MCP; else 404 {"message":"Not Found"}.
Path = r.URL.Path (Go already strips query; %-escapes: use r.URL.EscapedPath()
semantics where the TS used raw path — check TS pathOf: it slices req.url at '?').

## Determinism (replay must be byte-identical)
- All timestamps via NowIso() (MAKEFUN_TRIAL_NOW). Commit author/committer
  epoch fixed 1735689600 as in core.ts.
- X-GitHub-Request-Id: per-process counter "req-<base36>", as core.ts requestId().
- MCP session id: deterministic counter, as mcp.ts.
- No map-iteration order in any output: sort refs, repos, issues, files,
  labels wherever the TS sorts or insertion order mattered. Go maps are
  unordered — sort explicitly. Repo list order: TS used Map insertion order;
  Go must track insertion order if the suite checks it — check rest.ts
  user/repos handling and match (sort by created order via DBID if needed).
- JSON: JS JSON.stringify emits keys in object-insertion order. Go
  encoding/json on maps sorts keys alphabetically; on structs uses field
  order. The suite parses JSON, so key order should not matter, BUT
  tools/list must be byte-identical across two calls — build it once, cache
  the bytes. Issue read hot path: cache serialized bytes per issue like
  rest.ts cachedIssueBuffer (invalidate on mutation).

## Behaviour
Port faithfully: same status codes, same error messages, same JSON field
names, same edge cases as the TS. The TS is the spec — where RULES.txt and
TS disagree, TS wins (it passes 120/120). Do not "improve" semantics.
Viewer login is "adaiversen" (core.ts VIEWER). Server name "github-forge",
version "1.0.0".

## Perf notes
Hot paths: read = 2000x GET issue (keep-alive), write = 200x create issue,
push = 200x 1KiB push, clone = 50 concurrent shallow clones of 200-commit repo.
Cache packs per (repo, wants, haves, depth) in Repo.PackCache like git.ts does.
Avoid re-serializing issues per read (cache bytes). Use flate level matching
git.ts writePack (check its zlib level). Set Content-Length explicitly.
