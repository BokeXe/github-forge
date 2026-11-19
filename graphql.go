package main

// GraphQL v4 surface: POST /api/graphql, JSON {query, variables} (or a batch
// array). Faithful port of graphql.ts: a small hand-written parser/executor
// for the documents gh / github-mcp-server send — viewer, repository (plus
// issues/pullRequests connections, __type introspection, node), createIssue /
// closeIssue / createPullRequest / mergePullRequest / addComment.
//
// Handler signature per CONTRACT.md: HandleGraphql(w, r, store, token).
// Shared helpers from core.go / rest.go (assumed signatures, see report):
// Viewer, HttpError, RequireAuth, SendJSON, SendError, DatabaseID,
// RepoGlobalID, IssueGlobalID, PullGlobalID, CommentGlobalID, DecodeGlobalID,
// CreateIssueInternal, CreatePullInternal, MergePullInternal,
// AddCommentInternal, RefreshPullFiles, PullMergeable.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Introspection field lists, exactly as graphql.ts TYPE_FIELDS.
var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "url", "comments"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "createdAt", "updatedAt", "author", "url"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt", "updatedAt"},
	"SearchType":  {},
	"User":        {"id", "login", "name", "url"},
}

var gqlSearchTypeValues = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "WIKI", "CODE"}

// ---------------------------------------------------------------------------
// Tokenizer + parser (port of graphql.ts tokenize/Parser)
// ---------------------------------------------------------------------------

type gqlTokenKind int

const (
	gqlTokPunct gqlTokenKind = iota
	gqlTokName
	gqlTokString
	gqlTokInt
	gqlTokEOF
)

type gqlToken struct {
	kind     gqlTokenKind
	value    string
	intValue int64
}

func gqlIsDigit(c byte) bool { return c >= '0' && c <= '9' }
func gqlIsNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
func gqlIsNameChar(c byte) bool { return gqlIsNameStart(c) || gqlIsDigit(c) }

func gqlTokenize(src string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, gqlToken{kind: gqlTokPunct, value: "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}()[]:!=$@&|", ch) >= 0 {
			tokens = append(tokens, gqlToken{kind: gqlTokPunct, value: string(ch)})
			i++
			continue
		}
		if ch == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch {
					case esc == 'n':
						out.WriteByte('\n')
					case esc == 't':
						out.WriteByte('\t')
					case esc == 'r':
						out.WriteByte('\r')
					case esc == 'u' && j+5 < len(src):
						n, err := strconv.ParseInt(src[j+2:j+6], 16, 32)
						if err != nil {
							n = 0 // JS: String.fromCharCode(NaN) is '\0'
						}
						out.WriteRune(rune(n))
						j += 4
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: gqlTokString, value: out.String()})
			i = j + 1
			continue
		}
		if (gqlIsDigit(ch) || ch == '-') && (ch != '-' || (i+1 < len(src) && gqlIsDigit(src[i+1]))) {
			j := i
			if src[j] == '-' {
				j++
			}
			for j < len(src) && gqlIsDigit(src[j]) {
				j++
			}
			n, _ := strconv.ParseInt(src[i:j], 10, 64)
			tokens = append(tokens, gqlToken{kind: gqlTokInt, intValue: n})
			i = j
			continue
		}
		if gqlIsNameStart(ch) {
			j := i
			for j < len(src) && gqlIsNameChar(src[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: gqlTokName, value: src[i:j]})
			i = j
			continue
		}
		quoted, _ := json.Marshal(string(ch))
		return nil, fmt.Errorf("unexpected character %s in GraphQL document", quoted)
	}
	tokens = append(tokens, gqlToken{kind: gqlTokEOF})
	return tokens, nil
}

type gqlValueKind int

const (
	gqlValVariable gqlValueKind = iota
	gqlValString
	gqlValInt
	gqlValBool
	gqlValNull
	gqlValEnum
	gqlValList
	gqlValObject
)

type gqlObjectField struct {
	name  string
	value *gqlValueNode
}

type gqlValueNode struct {
	kind   gqlValueKind
	name   string // variable name
	str    string // string / enum value
	num    int64
	b      bool
	list   []*gqlValueNode
	fields []gqlObjectField
}

type gqlArg struct {
	name  string
	value *gqlValueNode
}

type gqlSelectionKind int

const (
	gqlSelField gqlSelectionKind = iota
	gqlSelInline
	gqlSelSpread
)

type gqlSelection struct {
	kind gqlSelectionKind
	// field
	alias           string
	hasAlias        bool
	name            string
	args            []gqlArg
	selectionSet    []*gqlSelection
	hasSelectionSet bool
	// inline fragment
	typeCondition    string
	hasTypeCondition bool
	// fragment spread uses name
}

type gqlVarDef struct {
	name         string
	defaultValue *gqlValueNode
}

type gqlOperation struct {
	operation    string // "query" | "mutation" | "subscription"
	name         string
	hasName      bool
	varDefs      []gqlVarDef
	selectionSet []*gqlSelection
}

type gqlFragment struct {
	name          string
	typeCondition string
	selectionSet  []*gqlSelection
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken {
	if p.pos >= len(p.tokens) {
		return gqlToken{kind: gqlTokEOF}
	}
	return p.tokens[p.pos]
}

func (p *gqlParser) next() gqlToken {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *gqlParser) peekPunct(v string) bool {
	t := p.peek()
	return t.kind == gqlTokPunct && t.value == v
}

func (p *gqlParser) expectPunct(v string) error {
	tok := p.next()
	if tok.kind != gqlTokPunct || tok.value != v {
		return fmt.Errorf("expected %q in GraphQL document", v)
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	tok := p.next()
	if tok.kind != gqlTokName {
		return "", errors.New("expected a name in GraphQL document")
	}
	return tok.value, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peekPunct("@") {
		p.next()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.peekPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, map[string]*gqlFragment, error) {
	var ops []*gqlOperation
	frags := make(map[string]*gqlFragment)
	for p.peek().kind != gqlTokEOF {
		op, frag, err := p.parseDefinition()
		if err != nil {
			return nil, nil, err
		}
		if frag != nil {
			frags[frag.name] = frag
		} else {
			ops = append(ops, op)
		}
	}
	return ops, frags, nil
}

func (p *gqlParser) parseDefinition() (*gqlOperation, *gqlFragment, error) {
	tok := p.peek()
	if tok.kind == gqlTokPunct && tok.value == "{" {
		ss, err := p.parseSelectionSet()
		if err != nil {
			return nil, nil, err
		}
		return &gqlOperation{operation: "query", selectionSet: ss}, nil, nil
	}
	if tok.kind == gqlTokName && (tok.value == "query" || tok.value == "mutation" || tok.value == "subscription") {
		operation := p.next().value
		op := &gqlOperation{operation: operation}
		if p.peek().kind == gqlTokName {
			op.name = p.next().value
			op.hasName = true
		}
		defs, err := p.parseVariableDefinitions()
		if err != nil {
			return nil, nil, err
		}
		op.varDefs = defs
		if err := p.skipDirectives(); err != nil {
			return nil, nil, err
		}
		ss, err := p.parseSelectionSet()
		if err != nil {
			return nil, nil, err
		}
		op.selectionSet = ss
		return op, nil, nil
	}
	if tok.kind == gqlTokName && tok.value == "fragment" {
		p.next()
		name, err := p.expectName()
		if err != nil {
			return nil, nil, err
		}
		on, err := p.expectName()
		if err != nil {
			return nil, nil, err
		}
		if on != "on" {
			return nil, nil, errors.New("expected \"on\" in fragment definition")
		}
		typeCondition, err := p.expectName()
		if err != nil {
			return nil, nil, err
		}
		if err := p.skipDirectives(); err != nil {
			return nil, nil, err
		}
		ss, err := p.parseSelectionSet()
		if err != nil {
			return nil, nil, err
		}
		return nil, &gqlFragment{name: name, typeCondition: typeCondition, selectionSet: ss}, nil
	}
	return nil, nil, errors.New("unexpected definition in GraphQL document")
}

func (p *gqlParser) parseVariableDefinitions() ([]gqlVarDef, error) {
	var defs []gqlVarDef
	if !p.peekPunct("(") {
		return defs, nil
	}
	p.next()
	for !p.peekPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		var defaultValue *gqlValueNode
		if p.peekPunct("=") {
			p.next()
			dv, err := p.parseValue(true)
			if err != nil {
				return nil, err
			}
			defaultValue = dv
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		defs = append(defs, gqlVarDef{name: name, defaultValue: defaultValue})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return defs, nil
}

func (p *gqlParser) parseType() error {
	if p.peekPunct("[") {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	if p.peekPunct("!") {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	selections := []*gqlSelection{}
	for !p.peekPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	tok := p.peek()
	if tok.kind == gqlTokPunct && tok.value == "..." {
		p.next()
		nextTok := p.peek()
		if nextTok.kind == gqlTokName && nextTok.value == "on" {
			p.next()
			typeCondition, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			ss, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: gqlSelInline, typeCondition: typeCondition, hasTypeCondition: true, selectionSet: ss, hasSelectionSet: true}, nil
		}
		if nextTok.kind == gqlTokName {
			name := p.next().value
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			return &gqlSelection{kind: gqlSelSpread, name: name}, nil
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		ss, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{kind: gqlSelInline, selectionSet: ss, hasSelectionSet: true}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	sel := &gqlSelection{kind: gqlSelField, name: first}
	if p.peekPunct(":") {
		p.next()
		sel.alias = first
		sel.hasAlias = true
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		sel.name = name
	}
	args, err := p.parseArguments()
	if err != nil {
		return nil, err
	}
	sel.args = args
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if p.peekPunct("{") {
		ss, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selectionSet = ss
		sel.hasSelectionSet = true
	}
	return sel, nil
}

func (p *gqlParser) parseArguments() ([]gqlArg, error) {
	var args []gqlArg
	if !p.peekPunct("(") {
		return args, nil
	}
	p.next()
	for !p.peekPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue(false)
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArg{name: name, value: v})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue(isConst bool) (*gqlValueNode, error) {
	tok := p.next()
	if tok.kind == gqlTokPunct && tok.value == "$" {
		if isConst {
			return nil, errors.New("variable in constant value")
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: gqlValVariable, name: name}, nil
	}
	if tok.kind == gqlTokPunct && tok.value == "[" {
		values := []*gqlValueNode{}
		for !p.peekPunct("]") {
			v, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: gqlValList, list: values}, nil
	}
	if tok.kind == gqlTokPunct && tok.value == "{" {
		fields := []gqlObjectField{}
		for !p.peekPunct("}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			fields = append(fields, gqlObjectField{name: name, value: v})
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: gqlValObject, fields: fields}, nil
	}
	if tok.kind == gqlTokString {
		return &gqlValueNode{kind: gqlValString, str: tok.value}, nil
	}
	if tok.kind == gqlTokInt {
		return &gqlValueNode{kind: gqlValInt, num: tok.intValue}, nil
	}
	if tok.kind == gqlTokName {
		switch tok.value {
		case "true":
			return &gqlValueNode{kind: gqlValBool, b: true}, nil
		case "false":
			return &gqlValueNode{kind: gqlValBool, b: false}, nil
		case "null":
			return &gqlValueNode{kind: gqlValNull}, nil
		}
		return &gqlValueNode{kind: gqlValEnum, str: tok.value}, nil
	}
	return nil, errors.New("unexpected value in GraphQL document")
}

// ---------------------------------------------------------------------------
// Execution context and value coercion helpers
// ---------------------------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
}

type gqlContext struct {
	store     *Store
	errors    []gqlError
	fragments map[string]*gqlFragment
}

// gqlObject is a domain object: a __typename plus plain fields resolvable by
// name, mirroring the TS objects that carry a hidden TYPENAME symbol and
// __repo/__issue/__pull/__comment back-references.
type gqlObject struct {
	typ     string
	fields  map[string]any
	repo    *Repo
	issue   *Issue
	pull    *Pull
	comment *Comment
}

func gqlResolveValue(node *gqlValueNode, variables map[string]any) any {
	switch node.kind {
	case gqlValVariable:
		return variables[node.name]
	case gqlValString:
		return node.str
	case gqlValInt:
		return node.num
	case gqlValBool:
		return node.b
	case gqlValNull:
		return nil
	case gqlValEnum:
		return node.str
	case gqlValList:
		out := make([]any, 0, len(node.list))
		for _, v := range node.list {
			out = append(out, gqlResolveValue(v, variables))
		}
		return out
	case gqlValObject:
		out := make(map[string]any, len(node.fields))
		for _, f := range node.fields {
			out[f.name] = gqlResolveValue(f.value, variables)
		}
		return out
	}
	return nil
}

func gqlArgsOf(field *gqlSelection, variables map[string]any) map[string]any {
	out := make(map[string]any, len(field.args))
	for _, arg := range field.args {
		out[arg.name] = gqlResolveValue(arg.value, variables)
	}
	return out
}

func gqlTypeOf(value any) string {
	if obj, ok := value.(*gqlObject); ok && obj.typ != "" {
		return obj.typ
	}
	return "Unknown"
}

// gqlStrip converts a value selected without a selection set into plain JSON
// data (the TS stripTypename pass). Internal back-references never leak.
func gqlStrip(value any) any {
	switch t := value.(type) {
	case *gqlObject:
		out := make(map[string]any, len(t.fields))
		for k, v := range t.fields {
			out[k] = gqlStrip(v)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = gqlStrip(e)
		}
		return out
	default:
		return value
	}
}

// gqlStringValue mirrors JS String(v ?? ”) for the scalar shapes the suite
// sends: nil -> "", strings pass through, numbers/bools stringify.
func gqlStringValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// gqlStringOrEmpty mirrors `typeof v === 'string' ? v : ”`.
func gqlStringOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// gqlToInt mirrors JS Number(v) truncated toward zero. ok=false is NaN.
func gqlToInt(v any) (int, bool) {
	switch t := v.(type) {
	case nil:
		return 0, true // Number(null) === 0; Number(undefined) is NaN, but both miss every lookup
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if math.IsNaN(t) {
			return 0, false
		}
		return int(t), true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return int(f), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return int(f), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// gqlNumberValue mirrors `typeof v === 'number'` plus the numeric value.
func gqlNumberValue(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func gqlInputMap(args map[string]any) map[string]any {
	if m, ok := args["input"].(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// ---------------------------------------------------------------------------
// Domain objects (port of userObj/refObj/repoObj/issueObj/pullObj/commentObj)
// ---------------------------------------------------------------------------

func gqlUserObj(login string) *gqlObject {
	return &gqlObject{
		typ: "User",
		fields: map[string]any{
			"id":    "U_" + base64.RawURLEncoding.EncodeToString([]byte(login)),
			"login": login,
			"name":  login,
			"url":   "https://example.invalid/" + login,
		},
	}
}

func gqlRefObj(name string) *gqlObject {
	return &gqlObject{
		typ:    "Ref",
		fields: map[string]any{"name": name, "prefix": "refs/heads/"},
	}
}

func gqlRepoObj(store *Store, repo *Repo) *gqlObject {
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	createdAt := repo.CreatedAt
	if createdAt == "" {
		createdAt = NowIso()
	}
	updatedAt := repo.UpdatedAt
	if updatedAt == "" {
		updatedAt = NowIso()
	}
	return &gqlObject{
		typ:  "Repository",
		repo: repo,
		fields: map[string]any{
			"id":               RepoGlobalID(repo),
			"databaseId":       DatabaseID(store, repo),
			"name":             repo.Name,
			"nameWithOwner":    repo.Owner + "/" + repo.Name,
			"owner":            gqlUserObj(repo.Owner),
			"defaultBranchRef": gqlRefObj(defaultBranch),
			"viewerPermission": "ADMIN",
			"hasIssuesEnabled": true,
			"description":      repo.Description,
			"private":          repo.Private,
			"url":              "https://example.invalid/" + repo.Owner + "/" + repo.Name,
			"createdAt":        createdAt,
			"updatedAt":        updatedAt,
		},
	}
}

func gqlIssueObj(repo *Repo, issue *Issue) *gqlObject {
	state := "OPEN"
	if issue.State == "closed" {
		state = "CLOSED"
	}
	author := issue.User
	if author == "" {
		author = Viewer
	}
	createdAt := issue.CreatedAt
	if createdAt == "" {
		createdAt = NowIso()
	}
	updatedAt := issue.UpdatedAt
	if updatedAt == "" {
		updatedAt = NowIso()
	}
	return &gqlObject{
		typ:   "Issue",
		repo:  repo,
		issue: issue,
		fields: map[string]any{
			"id":        IssueGlobalID(repo, issue.Number),
			"number":    issue.Number,
			"title":     issue.Title,
			"state":     state,
			"body":      issue.Body,
			"createdAt": createdAt,
			"updatedAt": updatedAt,
			"author":    gqlUserObj(author),
			"url":       fmt.Sprintf("https://example.invalid/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number),
		},
	}
}

func gqlPullObj(repo *Repo, pull *Pull) *gqlObject {
	state := "OPEN"
	if pull.Merged {
		state = "MERGED"
	} else if pull.State == "closed" {
		state = "CLOSED"
	}
	createdAt := pull.CreatedAt
	if createdAt == "" {
		createdAt = NowIso()
	}
	updatedAt := pull.UpdatedAt
	if updatedAt == "" {
		updatedAt = NowIso()
	}
	return &gqlObject{
		typ:  "PullRequest",
		repo: repo,
		pull: pull,
		fields: map[string]any{
			"id":          PullGlobalID(repo, pull.Number),
			"number":      pull.Number,
			"title":       pull.Title,
			"body":        pull.Body,
			"state":       state,
			"merged":      pull.Merged,
			"mergeable":   PullMergeable(repo, pull),
			"headRefName": pull.Head.Ref,
			"baseRefName": pull.Base.Ref,
			"headRefOid":  pull.Head.SHA,
			"baseRefOid":  pull.Base.SHA,
			"createdAt":   createdAt,
			"updatedAt":   updatedAt,
			"author":      gqlUserObj(Viewer),
			"url":         fmt.Sprintf("https://example.invalid/%s/%s/pull/%d", repo.Owner, repo.Name, pull.Number),
		},
	}
}

func gqlCommentObj(repo *Repo, issueNumber int, comment *Comment) *gqlObject {
	author := comment.User
	if author == "" {
		author = Viewer
	}
	createdAt := comment.CreatedAt
	if createdAt == "" {
		createdAt = NowIso()
	}
	return &gqlObject{
		typ:     "IssueComment",
		repo:    repo,
		comment: comment,
		fields: map[string]any{
			"id":        CommentGlobalID(repo, issueNumber, comment.ID),
			"body":      comment.Body,
			"createdAt": createdAt,
			"author":    gqlUserObj(author),
		},
	}
}

func gqlPageInfo(hasNextPage bool, endCursor any) *gqlObject {
	return &gqlObject{
		typ: "PageInfo",
		fields: map[string]any{
			"hasNextPage":     hasNextPage,
			"hasPreviousPage": false,
			"startCursor":     endCursor,
			"endCursor":       endCursor,
		},
	}
}

func gqlEncodeCursor(index int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(index)))
}

func gqlDecodeCursor(cursor any) int {
	s, ok := cursor.(string)
	if !ok || s == "" {
		return -1
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return -1
		}
	}
	text := string(decoded)
	if !strings.HasPrefix(text, "cursor:") {
		return -1
	}
	digits := text[len("cursor:"):]
	if digits == "" {
		return -1
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return -1
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return -1
	}
	return n
}

// ---------------------------------------------------------------------------
// Connections (port of connectionNodes/issueConnection/pullConnection/
// pullFilesConnection)
// ---------------------------------------------------------------------------

type gqlPage struct {
	nodes       []*gqlObject
	hasNextPage bool
	endCursor   any // string or nil
}

func gqlConnectionNodes(items []*gqlObject, args map[string]any) gqlPage {
	first := len(items)
	if f, ok := gqlNumberValue(args["first"]); ok {
		if math.IsNaN(f) || f < 0 {
			first = 0
		} else if f > float64(len(items)) {
			first = len(items)
		} else {
			first = int(f)
		}
	}
	afterIndex := gqlDecodeCursor(args["after"])
	start := afterIndex + 1
	if start < 0 {
		start = 0
	}
	var slice []*gqlObject
	if start < len(items) {
		end := start + first
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	} else {
		slice = []*gqlObject{}
	}
	endIndex := start + len(slice) - 1
	page := gqlPage{
		nodes:       slice,
		hasNextPage: endIndex+1 < len(items),
	}
	if len(slice) > 0 {
		page.endCursor = gqlEncodeCursor(endIndex)
	}
	return page
}

func gqlConnectionObject(typ, edgeTyp string, page gqlPage, totalCount int) *gqlObject {
	nodes := make([]any, 0, len(page.nodes))
	edges := make([]any, 0, len(page.nodes))
	for idx, node := range page.nodes {
		nodes = append(nodes, node)
		edges = append(edges, &gqlObject{
			typ: edgeTyp,
			fields: map[string]any{
				"node":   node,
				"cursor": gqlEncodeCursor(idx), // TS uses the in-page index, faithfully kept
			},
		})
	}
	return &gqlObject{
		typ: typ,
		fields: map[string]any{
			"nodes":      nodes,
			"edges":      edges,
			"pageInfo":   gqlPageInfo(page.hasNextPage, page.endCursor),
			"totalCount": totalCount,
		},
	}
}

func gqlIssueConnection(repo *Repo, args map[string]any) *gqlObject {
	list := make([]*Issue, 0, len(repo.Issues))
	for _, issue := range repo.Issues {
		list = append(list, issue)
	}
	if statesRaw, ok := args["states"].([]any); ok {
		states := make(map[string]bool, len(statesRaw))
		for _, s := range statesRaw {
			states[strings.ToUpper(gqlStringValue(s))] = true
		}
		filtered := list[:0]
		for _, issue := range list {
			st := "OPEN"
			if issue.State == "closed" {
				st = "CLOSED"
			}
			if states[st] {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	direction := 1
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		if d, _ := orderBy["direction"].(string); d == "DESC" {
			direction = -1
		}
	}
	sort.Slice(list, func(a, b int) bool {
		return direction*(list[a].Number-list[b].Number) < 0
	})
	objs := make([]*gqlObject, 0, len(list))
	for _, issue := range list {
		objs = append(objs, gqlIssueObj(repo, issue))
	}
	page := gqlConnectionNodes(objs, args)
	return gqlConnectionObject("IssueConnection", "IssueEdge", page, len(list))
}

func gqlPullConnection(repo *Repo, args map[string]any) *gqlObject {
	list := make([]*Pull, 0, len(repo.Pulls))
	for _, pull := range repo.Pulls {
		list = append(list, pull)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Number < list[b].Number })
	objs := make([]*gqlObject, 0, len(list))
	for _, pull := range list {
		objs = append(objs, gqlPullObj(repo, pull))
	}
	page := gqlConnectionNodes(objs, args)
	return gqlConnectionObject("PullRequestConnection", "PullRequestEdge", page, len(list))
}

func gqlPullFilesConnection(repo *Repo, pull *Pull, args map[string]any) *gqlObject {
	RefreshPullFiles(repo, pull)
	files := make([]*gqlObject, 0, len(pull.Files))
	for _, f := range pull.Files {
		files = append(files, &gqlObject{
			typ: "PullRequestChangedFile",
			fields: map[string]any{
				"path":      f.Filename,
				"additions": f.Additions,
				"deletions": f.Deletions,
				"changes":   f.Changes,
			},
		})
	}
	page := gqlConnectionNodes(files, args)
	return gqlConnectionObject("PullRequestChangedFileConnection", "PullRequestChangedFileEdge", page, len(files))
}

// ---------------------------------------------------------------------------
// Field resolvers (port of RESOLVERS in graphql.ts)
// ---------------------------------------------------------------------------

type gqlResolver func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error)

var gqlResolvers = map[string]map[string]gqlResolver{
	"Query": {
		"viewer": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			return gqlUserObj(Viewer), nil
		},
		"repository": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			repo := ctx.store.GetRepo(gqlStringValue(args["owner"]), gqlStringValue(args["name"]))
			if repo == nil {
				return nil, nil
			}
			return gqlRepoObj(ctx.store, repo), nil
		},
		"node": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			decoded, ok := DecodeGlobalID(gqlStringValue(args["id"]))
			if !ok {
				return nil, nil
			}
			repo := ctx.store.GetRepo(decoded.Owner, decoded.Name)
			if repo == nil {
				return nil, nil
			}
			switch decoded.Kind {
			case "repo":
				return gqlRepoObj(ctx.store, repo), nil
			case "issue":
				if issue := repo.Issues[decoded.Number]; issue != nil {
					return gqlIssueObj(repo, issue), nil
				}
				return nil, nil
			case "pull":
				if pull := repo.Pulls[decoded.Number]; pull != nil {
					return gqlPullObj(repo, pull), nil
				}
				return nil, nil
			}
			return nil, nil
		},
		"__type": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			name := gqlStringValue(args["name"])
			fields, known := gqlTypeFields[name]
			if !known && name != "SearchType" {
				return nil, nil
			}
			kind := "OBJECT"
			if name == "SearchType" {
				kind = "ENUM"
			}
			fieldObjs := make([]any, 0, len(fields))
			for _, f := range fields {
				fieldObjs = append(fieldObjs, &gqlObject{
					typ: "__Field",
					fields: map[string]any{
						"name":        f,
						"description": nil,
						"args":        []any{},
					},
				})
			}
			enumValues := []any{}
			if name == "SearchType" {
				for _, v := range gqlSearchTypeValues {
					enumValues = append(enumValues, &gqlObject{
						typ: "__EnumValue",
						fields: map[string]any{
							"name":        v,
							"description": nil,
						},
					})
				}
			}
			return &gqlObject{
				typ: "__Type",
				fields: map[string]any{
					"name":        name,
					"kind":        kind,
					"fields":      fieldObjs,
					"enumValues":  enumValues,
					"description": nil,
					"__typeName":  name,
				},
			}, nil
		},
		"__schema": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			return &gqlObject{
				typ: "__Schema",
				fields: map[string]any{
					"queryType":    &gqlObject{fields: map[string]any{"name": "Query"}},
					"mutationType": &gqlObject{fields: map[string]any{"name": "Mutation"}},
					"types":        []any{},
				},
			}, nil
		},
	},
	"Mutation": {
		"createIssue": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			input := gqlInputMap(args)
			decoded, ok := DecodeGlobalID(gqlStringValue(input["repositoryId"]))
			if !ok || decoded.Kind != "repo" {
				return nil, errors.New("repositoryId is not a known repository")
			}
			repo := ctx.store.GetRepo(decoded.Owner, decoded.Name)
			if repo == nil {
				return nil, errors.New("repository not found")
			}
			issue, err := CreateIssueInternal(ctx.store, repo, CreateIssueInput{Title: gqlStringOrEmpty(input["title"]), Body: gqlStringOrEmpty(input["body"])})
			if err != nil {
				return nil, err
			}
			return &gqlObject{
				typ: "CreateIssuePayload",
				fields: map[string]any{
					"issue":            gqlIssueObj(repo, issue),
					"clientMutationId": input["clientMutationId"],
				},
			}, nil
		},
		"closeIssue": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			input := gqlInputMap(args)
			decoded, ok := DecodeGlobalID(gqlStringValue(input["issueId"]))
			if !ok || decoded.Kind != "issue" {
				return nil, errors.New("issueId is not a known issue")
			}
			repo := ctx.store.GetRepo(decoded.Owner, decoded.Name)
			var issue *Issue
			if repo != nil {
				issue = repo.Issues[decoded.Number]
			}
			if repo == nil || issue == nil {
				return nil, errors.New("issue not found")
			}
			issue.State = "closed"
			issue.UpdatedAt = NowIso()
			return &gqlObject{
				typ: "CloseIssuePayload",
				fields: map[string]any{
					"issue":            gqlIssueObj(repo, issue),
					"clientMutationId": input["clientMutationId"],
				},
			}, nil
		},
		"createPullRequest": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			input := gqlInputMap(args)
			decoded, ok := DecodeGlobalID(gqlStringValue(input["repositoryId"]))
			if !ok || decoded.Kind != "repo" {
				return nil, errors.New("repositoryId is not a known repository")
			}
			repo := ctx.store.GetRepo(decoded.Owner, decoded.Name)
			if repo == nil {
				return nil, errors.New("repository not found")
			}
			pull, err := CreatePullInternal(ctx.store, repo, CreatePullInput{
				Head:  gqlStringOrEmpty(input["headRefName"]),
				Base:  gqlStringOrEmpty(input["baseRefName"]),
				Title: gqlStringOrEmpty(input["title"]),
				Body:  gqlStringOrEmpty(input["body"]),
			})
			if err != nil {
				return nil, err
			}
			return &gqlObject{
				typ: "CreatePullRequestPayload",
				fields: map[string]any{
					"pullRequest":      gqlPullObj(repo, pull),
					"clientMutationId": input["clientMutationId"],
				},
			}, nil
		},
		"mergePullRequest": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			input := gqlInputMap(args)
			decoded, ok := DecodeGlobalID(gqlStringValue(input["pullRequestId"]))
			if !ok || decoded.Kind != "pull" {
				return nil, errors.New("pullRequestId is not a known pull request")
			}
			repo := ctx.store.GetRepo(decoded.Owner, decoded.Name)
			var pull *Pull
			if repo != nil {
				pull = repo.Pulls[decoded.Number]
			}
			if repo == nil || pull == nil {
				return nil, errors.New("pull request not found")
			}
			if _, _, err := MergePullInternal(repo, pull, gqlStringOrEmpty(input["mergeMethod"])); err != nil {
				return nil, err
			}
			return &gqlObject{
				typ: "MergePullRequestPayload",
				fields: map[string]any{
					"pullRequest":      gqlPullObj(repo, pull),
					"clientMutationId": input["clientMutationId"],
				},
			}, nil
		},
		"addComment": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			input := gqlInputMap(args)
			decoded, ok := DecodeGlobalID(gqlStringValue(input["subjectId"]))
			if !ok || decoded.Kind != "issue" {
				return nil, errors.New("subjectId is not a known issue")
			}
			repo := ctx.store.GetRepo(decoded.Owner, decoded.Name)
			var issue *Issue
			if repo != nil {
				issue = repo.Issues[decoded.Number]
			}
			if repo == nil || issue == nil {
				return nil, errors.New("issue not found")
			}
			comment, err := AddCommentInternal(repo, issue, gqlStringOrEmpty(input["body"]))
			if err != nil {
				return nil, err
			}
			edge := &gqlObject{
				typ: "IssueCommentEdge",
				fields: map[string]any{
					"node":   gqlCommentObj(repo, decoded.Number, comment),
					"cursor": gqlEncodeCursor(0),
				},
			}
			return &gqlObject{
				typ: "AddCommentPayload",
				fields: map[string]any{
					"commentEdge":      edge,
					"clientMutationId": input["clientMutationId"],
				},
			}, nil
		},
	},
	"Repository": {
		"issue": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			n, ok := gqlToInt(args["number"])
			if !ok {
				return nil, nil
			}
			if issue := source.repo.Issues[n]; issue != nil {
				return gqlIssueObj(source.repo, issue), nil
			}
			return nil, nil
		},
		"issues": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			return gqlIssueConnection(source.repo, args), nil
		},
		"pullRequest": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			n, ok := gqlToInt(args["number"])
			if !ok {
				return nil, nil
			}
			if pull := source.repo.Pulls[n]; pull != nil {
				return gqlPullObj(source.repo, pull), nil
			}
			return nil, nil
		},
		"pullRequests": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			return gqlPullConnection(source.repo, args), nil
		},
	},
	"PullRequest": {
		"files": func(ctx *gqlContext, source *gqlObject, args map[string]any) (any, error) {
			return gqlPullFilesConnection(source.repo, source.pull, args), nil
		},
	},
}

// Scalar / pass-through fields resolve from the source object's own property,
// exactly as executeSelectionSet in graphql.ts does.
func gqlExecuteSelectionSet(ctx *gqlContext, typeName string, source *gqlObject, selections []*gqlSelection, variables map[string]any) map[string]any {
	out := make(map[string]any)
	for _, selection := range selections {
		if selection.kind == gqlSelSpread {
			frag, ok := ctx.fragments[selection.name]
			if !ok {
				ctx.errors = append(ctx.errors, gqlError{Message: fmt.Sprintf("Unknown fragment %q.", selection.name)})
				continue
			}
			nested := gqlExecuteSelectionSet(ctx, typeName, source, frag.selectionSet, variables)
			for k, v := range nested {
				out[k] = v
			}
			continue
		}
		if selection.kind == gqlSelInline {
			if selection.hasTypeCondition && selection.typeCondition != typeName {
				continue
			}
			nestedType := typeName
			if selection.hasTypeCondition {
				nestedType = selection.typeCondition
			}
			nested := gqlExecuteSelectionSet(ctx, nestedType, source, selection.selectionSet, variables)
			for k, v := range nested {
				out[k] = v
			}
			continue
		}
		field := selection
		outputKey := field.name
		if field.hasAlias {
			outputKey = field.alias
		}
		var value any
		resolver := gqlResolvers[typeName][field.name]
		switch {
		case resolver != nil:
			v, err := resolver(ctx, source, gqlArgsOf(field, variables))
			if err != nil {
				ctx.errors = append(ctx.errors, gqlError{Message: err.Error()})
				out[outputKey] = nil
				continue
			}
			value = v
		case field.name == "__typename":
			value = typeName
		case source != nil && fieldInSource(source, field.name):
			value = source.fields[field.name]
		case field.name == "id" && source != nil:
			// Fallback id for connection/edge objects that carry one.
			value = nil
		default:
			ctx.errors = append(ctx.errors, gqlError{Message: fmt.Sprintf("Cannot query field %q on type %q.", field.name, typeName)})
			continue
		}
		if value == nil {
			out[outputKey] = nil
			continue
		}
		if field.hasSelectionSet {
			switch t := value.(type) {
			case []any:
				items := make([]any, 0, len(t))
				for _, item := range t {
					if _, isObj := item.(*gqlObject); isObj {
						items = append(items, gqlExecuteSelectionSet(ctx, gqlTypeOf(item), item.(*gqlObject), field.selectionSet, variables))
					} else {
						items = append(items, item)
					}
				}
				out[outputKey] = items
				continue
			case *gqlObject:
				out[outputKey] = gqlExecuteSelectionSet(ctx, gqlTypeOf(t), t, field.selectionSet, variables)
				continue
			}
		}
		out[outputKey] = gqlStrip(value)
	}
	return out
}

func fieldInSource(source *gqlObject, name string) bool {
	_, ok := source.fields[name]
	return ok
}

func gqlExecuteOperation(operation *gqlOperation, fragments map[string]*gqlFragment, store *Store, variables map[string]any) (map[string]any, []gqlError) {
	ctx := &gqlContext{store: store, fragments: fragments}
	rootType := "Query"
	if operation.operation == "mutation" {
		rootType = "Mutation"
	}
	mergedVariables := make(map[string]any, len(variables))
	for k, v := range variables {
		mergedVariables[k] = v
	}
	for _, def := range operation.varDefs {
		if _, present := mergedVariables[def.name]; !present && def.defaultValue != nil {
			mergedVariables[def.name] = gqlResolveValue(def.defaultValue, mergedVariables)
		}
	}
	root := &gqlObject{typ: rootType, fields: map[string]any{}}
	data := gqlExecuteSelectionSet(ctx, rootType, root, operation.selectionSet, mergedVariables)
	return data, ctx.errors
}

// ---------------------------------------------------------------------------
// Handler (port of handleGraphql/runOneOperation)
// ---------------------------------------------------------------------------

func gqlRunOneOperation(operation any, store *Store) map[string]any {
	opMap, _ := operation.(map[string]any)
	query := gqlStringOrEmpty(opMap["query"])
	variables := map[string]any{}
	if v, ok := opMap["variables"].(map[string]any); ok && v != nil {
		variables = v
	}
	operationName, hasOperationName := opMap["operationName"].(string)
	if strings.TrimSpace(query) == "" {
		return map[string]any{"data": map[string]any{}, "errors": []gqlError{{Message: "Must provide query string."}}}
	}
	tokens, err := gqlTokenize(query)
	if err != nil {
		return map[string]any{"data": map[string]any{}, "errors": []gqlError{{Message: err.Error()}}}
	}
	operations, fragments, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		return map[string]any{"data": map[string]any{}, "errors": []gqlError{{Message: err.Error()}}}
	}
	if len(operations) == 0 {
		return map[string]any{"data": map[string]any{}, "errors": []gqlError{{Message: "Must provide an operation."}}}
	}
	var chosen *gqlOperation
	if hasOperationName {
		for _, op := range operations {
			if op.hasName && op.name == operationName {
				chosen = op
				break
			}
		}
	}
	if chosen == nil {
		chosen = operations[0]
	}
	// Execution touches store state (mutations included): hold the write lock
	// while building the full result, write after unlocking (CONTRACT.md).
	store.Mu.Lock()
	data, errs := gqlExecuteOperation(chosen, fragments, store, variables)
	store.Mu.Unlock()
	result := map[string]any{"data": data}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	return result
}

func HandleGraphql(w http.ResponseWriter, r *http.Request, store *Store, token string) {
	if err := RequireAuth(r, token); err != nil {
		status := 401
		message := err.Error()
		if he, ok := err.(*HttpError); ok {
			status = he.Status
			message = he.Message
		}
		SendError(w, status, message)
		return
	}
	if r.Method != http.MethodPost {
		SendError(w, 405, "method not allowed")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<20)+1))
	if err != nil {
		SendError(w, 500, err.Error())
		return
	}
	if len(raw) > 64<<20 {
		SendError(w, 413, "request body too large")
		return
	}
	var body any = map[string]any{}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil {
			SendError(w, 400, "malformed JSON body")
			return
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			SendError(w, 400, "malformed JSON body")
			return
		}
	}
	isBatch := false
	var operations []any
	if arr, ok := body.([]any); ok {
		isBatch = true
		operations = arr
	} else {
		operations = []any{body}
	}
	results := make([]any, 0, len(operations))
	for _, operation := range operations {
		results = append(results, gqlRunOneOperation(operation, store))
	}
	var out any
	if isBatch {
		out = results
	} else {
		out = results[0]
	}
	SendJSON(w, 200, out, map[string]string{"x-oauth-scopes": "repo, read:org"})
}
