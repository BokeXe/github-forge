package main

// Shared in-memory store for the Go forge — a faithful port of the Node
// entry's store.ts. One Store holds every repository; the git smart-HTTP
// module, REST v3, GraphQL and MCP all share one instance. No disk I/O.
//
// Concurrency: the Node original is single-threaded. In Go, handlers run
// concurrently, so every access goes through Store.Mu. Read-only work
// (pack building for clone, issue reads) takes RLock so the 50 concurrent
// clones can build packs in parallel; mutations take Lock.

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitObject is one git object: its type and its exact bytes (no loose header).
type GitObject struct {
	Type string
	Data []byte
}

// Label is an issue label; Color is 6-hex without '#'.
type Label struct {
	Name        string
	Color       string
	Description string
}

// Comment is an issue comment.
type Comment struct {
	ID        int
	Body      string
	User      string // login
	CreatedAt string
	UpdatedAt string
}

// Issue mirrors store.ts Issue plus the comments array core.ts adds.
type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	User      string // login
	CreatedAt string
	UpdatedAt string
	Comments  []*Comment
}

// RefTip is a pull head/base: branch ref name + sha.
type RefTip struct {
	Ref string
	SHA string
}

// FileDiff is one changed file in a pull request.
type FileDiff struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
}

// Pull mirrors store.ts Pull plus merged_at/files from core.ts.
type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Labels         []string
	User           string // login
	CreatedAt      string
	UpdatedAt      string
	Head           RefTip
	Base           RefTip
	Merged         bool
	MergedAt       *string
	MergeCommitSHA *string
	Files          []FileDiff
}

// Repo mirrors store.ts Repo.
type Repo struct {
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	HeadTarget    string // e.g. "refs/heads/main"
	Refs          map[string]string
	RefsVersion   int // bumped on every Refs/HeadTarget change; keys the advertisement caches
	Objects       map[string]*GitObject
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	Labels        map[string]*Label
	NextIssue     int
	NextPull      int
	DBID          int // 0 = unassigned; core.databaseId assigns from 10000

	// Pack cache for clone/fetch: key -> built pack bytes. Guarded by Store.Mu.
	PackCache map[string][]byte
}

// Store is the whole forge state.
type Store struct {
	Mu       sync.RWMutex
	Repos    map[string]*Repo
	NextDBID int

	// In-flight fetch-response generations, for stampede coalescing:
	// 50 concurrent identical clones share one pack generation.
	FetchMu    sync.Mutex
	FetchCalls map[string]*FetchCall
}

// FetchCall is one in-flight cached-response generation.
type FetchCall struct {
	Done chan struct{}
}

// RepoKey joins owner and name unambiguously.
func RepoKey(owner, name string) string { return owner + "/" + name }

// NewStore builds an empty store.
func NewStore() *Store {
	return &Store{Repos: make(map[string]*Repo), NextDBID: 10000, FetchCalls: make(map[string]*FetchCall)}
}

// GetRepo returns the repo or nil. Caller must hold Mu (read or write).
func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[RepoKey(owner, name)]
}

// CreateRepo creates a repository. Idempotent: an existing name under the
// same owner returns the existing repo. Caller must hold Mu (write).
func (s *Store) CreateRepo(owner, name, description string, isPrivate bool) *Repo {
	if existing := s.GetRepo(owner, name); existing != nil {
		return existing
	}
	createdAt := NowIso()
	repo := &Repo{
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       isPrivate,
		DefaultBranch: "main",
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
		HeadTarget:    "refs/heads/main",
		Refs:          make(map[string]string),
		Objects:       make(map[string]*GitObject),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		Labels:        make(map[string]*Label),
		NextIssue:     1,
		NextPull:      1,
		PackCache:     make(map[string][]byte),
	}
	for _, l := range DefaultLabels() {
		label := l
		repo.Labels[label.Name] = &label
	}
	s.Repos[RepoKey(owner, name)] = repo
	return repo
}

// ListRepos returns every repo, optionally only one owner's.
// Caller must hold Mu (read or write). Order is unspecified; callers that
// need determinism must sort.
func (s *Store) ListRepos(owner string, filterOwner bool) []*Repo {
	out := make([]*Repo, 0, len(s.Repos))
	for _, r := range s.Repos {
		if filterOwner && r.Owner != owner {
			continue
		}
		out = append(out, r)
	}
	return out
}

// DefaultLabels are the labels GitHub puts on a fresh repository.
func DefaultLabels() []Label {
	return []Label{
		{Name: "bug", Color: "d73a4a", Description: "Something isn't working"},
		{Name: "documentation", Color: "0075ca", Description: "Improvements or additions to documentation"},
		{Name: "duplicate", Color: "cfd3d7", Description: "This issue or pull request already exists"},
		{Name: "enhancement", Color: "a2eeef", Description: "New feature or request"},
		{Name: "good first issue", Color: "7057ff", Description: "Good for newcomers"},
		{Name: "help wanted", Color: "008672", Description: "Extra attention is needed"},
		{Name: "invalid", Color: "e4e669", Description: "This does not seem right"},
		{Name: "question", Color: "d876e3", Description: "Further information is requested"},
		{Name: "wontfix", Color: "ffffff", Description: "This will not be worked on"},
	}
}

// --- trial-fixed clock -----------------------------------------------------

var (
	trialNowOnce sync.Once
	trialNowISO  string
	trialNowSet  bool
)

// TrialNowIso returns the trial's fixed "now" (MAKEFUN_TRIAL_NOW as an
// RFC 3339 instant or epoch milliseconds), parsed once.
func TrialNowIso() (string, bool) {
	trialNowOnce.Do(func() {
		text := strings.TrimSpace(os.Getenv("MAKEFUN_TRIAL_NOW"))
		if text == "" {
			return
		}
		var ms int64
		if isAllDigits(text) && len(text) <= 16 {
			v, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return
			}
			ms = v
		} else {
			t, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				// Node's Date parser also accepts a few other forms; try the
				// common ones before giving up.
				t, err = time.Parse("2006-01-02T15:04:05.000Z", text)
				if err != nil {
					return
				}
			}
			ms = t.UnixMilli()
		}
		trialNowISO = formatISO(time.UnixMilli(ms).UTC())
		trialNowSet = true
	})
	return trialNowISO, trialNowSet
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// formatISO renders t exactly like JS Date.toISOString(): millisecond
// precision, trailing Z.
func formatISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// NowIso is the timestamp a new record carries: the trial's fixed now when
// set, else the wall clock.
func NowIso() string {
	if iso, ok := TrialNowIso(); ok {
		return iso
	}
	return formatISO(time.Now())
}

// --- git objects -----------------------------------------------------------

// HashObject is git's object id: SHA-1 over "<type> <size>\0" + data.
func HashObject(objType string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(objType))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// PutObject stores an object and returns its oid. Caller holds Mu (write).
func PutObject(repo *Repo, objType string, data []byte) string {
	oid := HashObject(objType, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: objType, Data: data}
	}
	return oid
}

// GetObject returns the object for an oid or nil. Caller holds Mu.
func GetObject(repo *Repo, oid string) *GitObject {
	return repo.Objects[oid]
}
