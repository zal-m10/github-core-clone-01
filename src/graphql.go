// GraphQL v4: a hand-rolled parser and executor for the documents
// gh and github-mcp-server send. No GraphQL library is used
// client-side, so the documents are fixed strings; the parser
// covers operations, variables, aliases, arguments (including
// enums, lists and input objects), and inline fragments, which is
// everything those documents use.
//
// Rules the executor follows:
//   - an unknown field is HTTP 200 with an errors array naming it;
//   - an unknown repository is data.repository = null, no errors;
//   - ids are the same opaque global ids REST and MCP use;
//   - issues order CREATED_AT DESC with ties by number DESC, and
//     cursor pagination through pageInfo.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- lexer

type gqlToken struct {
	kind string // name, string, int, float, punct, var, eof
	val  string
}

func gqlLex(src string) ([]gqlToken, error) {
	var out []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@' || c == '$' || c == '&' || c == '|':
			out = append(out, gqlToken{kind: "punct", val: string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			out = append(out, gqlToken{kind: "punct", val: "..."})
			i += 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(src[j])
					}
					j++
					continue
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			out = append(out, gqlToken{kind: "string", val: sb.String()})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			out = append(out, gqlToken{kind: kind, val: src[i:j]})
			i = j
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
			j := i
			for j < len(src) && ((src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			out = append(out, gqlToken{kind: "name", val: src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	out = append(out, gqlToken{kind: "eof"})
	return out, nil
}

// ---------------------------------------------------------------- AST

type gqlValue struct {
	kind string // var, string, int, float, bool, null, enum, list, object
	str  string
	num  int64
	list []gqlValue
	obj  map[string]gqlValue
}

type gqlField struct {
	alias     string
	name      string
	args      map[string]gqlValue
	selection []*gqlField // nil when a leaf
	fragment  string      // inline fragment type condition, "" when a plain field
}

type gqlOperation struct {
	kind    string // query | mutation
	name    string
	varDefs map[string]gqlValue // variable defaults (unused here)
	fields  []*gqlField
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() gqlToken  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlToken  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(val string) error {
	t := p.next()
	if t.val != val {
		return fmt.Errorf("expected %q, got %q", val, t.val)
	}
	return nil
}

func parseGraphQL(src string) (*gqlOperation, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	op, err := p.parseOperation()
	if err != nil {
		return nil, err
	}
	return op, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query", varDefs: map[string]gqlValue{}}
	t := p.peek()
	if t.kind == "name" && (t.val == "query" || t.val == "mutation") {
		op.kind = p.next().val
		if p.peek().kind == "name" {
			op.name = p.next().val
		}
	}
	if p.peek().val == "(" {
		if err := p.parseVarDefs(op); err != nil {
			return nil, err
		}
	}
	fields, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.fields = fields
	return op, nil
}

func (p *gqlParser) parseVarDefs(op *gqlOperation) error {
	if err := p.expect("("); err != nil {
		return err
	}
	for p.peek().val != ")" {
		if p.peek().val != "$" {
			return fmt.Errorf("expected $ in variable definition")
		}
		p.next()
		name := p.next().val
		if err := p.expect(":"); err != nil {
			return err
		}
		if err := p.parseType(); err != nil {
			return err
		}
		if p.peek().val == "=" {
			p.next()
			v, err := p.parseValue()
			if err != nil {
				return err
			}
			op.varDefs[name] = v
		}
	}
	return p.expect(")")
}

func (p *gqlParser) parseType() error {
	if p.peek().val == "[" {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expect("]"); err != nil {
			return err
		}
	} else {
		if p.peek().kind != "name" {
			return fmt.Errorf("expected type name")
		}
		p.next()
	}
	if p.peek().val == "!" {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var fields []*gqlField
	for p.peek().val != "}" {
		if p.peek().kind == "eof" {
			return nil, fmt.Errorf("unterminated selection set")
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
	if err := p.expect("}"); err != nil {
		return nil, err
	}
	return fields, nil
}

func (p *gqlParser) parseField() (*gqlField, error) {
	// Inline fragment: ... on Type { ... }
	if p.peek().val == "..." {
		p.next()
		f := &gqlField{}
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			f.fragment = p.next().val
		}
		// Directives are skipped.
		for p.peek().val == "@" {
			p.next()
			p.next()
			if p.peek().val == "(" {
				if _, err := p.parseArgs(); err != nil {
					return nil, err
				}
			}
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.selection = sel
		return f, nil
	}
	t := p.next()
	if t.kind != "name" {
		return nil, fmt.Errorf("expected field name, got %q", t.val)
	}
	f := &gqlField{name: t.val, alias: t.val}
	if p.peek().val == ":" {
		p.next()
		alias := p.next()
		if alias.kind != "name" {
			return nil, fmt.Errorf("expected field name after alias")
		}
		f.alias, f.name = t.val, alias.val
	}
	if p.peek().val == "(" {
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	for p.peek().val == "@" {
		p.next()
		p.next()
		if p.peek().val == "(" {
			if _, err := p.parseArgs(); err != nil {
				return nil, err
			}
		}
	}
	if p.peek().val == "{" {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.selection = sel
	}
	return f, nil
}

func (p *gqlParser) parseArgs() (map[string]gqlValue, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	args := map[string]gqlValue{}
	for p.peek().val != ")" {
		name := p.next()
		if name.kind != "name" {
			return nil, fmt.Errorf("expected argument name")
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name.val] = v
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	t := p.next()
	switch t.kind {
	case "string":
		return gqlValue{kind: "string", str: t.val}, nil
	case "int":
		n, _ := strconv.ParseInt(t.val, 10, 64)
		return gqlValue{kind: "int", num: n}, nil
	case "float":
		return gqlValue{kind: "float", str: t.val}, nil
	case "name":
		switch t.val {
		case "true":
			return gqlValue{kind: "bool", str: "true"}, nil
		case "false":
			return gqlValue{kind: "bool", str: "false"}, nil
		case "null":
			return gqlValue{kind: "null"}, nil
		default:
			return gqlValue{kind: "enum", str: t.val}, nil
		}
	case "punct":
		switch t.val {
		case "$":
			name := p.next()
			return gqlValue{kind: "var", str: name.val}, nil
		case "[":
			var list []gqlValue
			for p.peek().val != "]" {
				v, err := p.parseValue()
				if err != nil {
					return gqlValue{}, err
				}
				list = append(list, v)
			}
			p.next()
			return gqlValue{kind: "list", list: list}, nil
		case "{":
			obj := map[string]gqlValue{}
			for p.peek().val != "}" {
				name := p.next()
				if err := p.expect(":"); err != nil {
					return gqlValue{}, err
				}
				v, err := p.parseValue()
				if err != nil {
					return gqlValue{}, err
				}
				obj[name.val] = v
			}
			p.next()
			return gqlValue{kind: "object", obj: obj}, nil
		}
	}
	return gqlValue{}, fmt.Errorf("unexpected value token %q", t.val)
}

// ---------------------------------------------------------------- value resolution

// resolveValue turns a parsed value into a plain Go value using
// the operation's variables.
func resolveValue(v gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		if x, ok := vars[v.str]; ok {
			return x
		}
		return nil
	case "string", "enum":
		return v.str
	case "int":
		return v.num
	case "float":
		f, _ := strconv.ParseFloat(v.str, 64)
		return f
	case "bool":
		return v.str == "true"
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, e := range v.list {
			out = append(out, resolveValue(e, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, e := range v.obj {
			out[k] = resolveValue(e, vars)
		}
		return out
	}
	return nil
}

func argString(f *gqlField, name string, vars map[string]any) string {
	v, ok := f.args[name]
	if !ok {
		return ""
	}
	if s, ok := resolveValue(v, vars).(string); ok {
		return s
	}
	return ""
}

func argInt(f *gqlField, name string, vars map[string]any) int64 {
	v, ok := f.args[name]
	if !ok {
		return 0
	}
	switch x := resolveValue(v, vars).(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

// ---------------------------------------------------------------- schema tables

// typeFields lists the fields each type answers. A field outside
// the list is an error naming it.
var typeFields = map[string]map[string]bool{
	"Query": {
		"viewer": true, "repository": true, "node": true, "__type": true,
		"createIssue": false, "closeIssue": false, "createPullRequest": false,
		"mergePullRequest": false, "addComment": false,
	},
	"Mutation": {
		"createIssue": true, "closeIssue": true, "createPullRequest": true,
		"mergePullRequest": true, "addComment": true,
	},
	"User":    {"login": true, "id": true, "name": true},
	"Repository": {
		"id": true, "databaseId": true, "name": true, "nameWithOwner": true,
		"owner": true, "defaultBranchRef": true, "viewerPermission": true,
		"hasIssuesEnabled": true, "issue": true, "issues": true,
		"pullRequests": true, "pullRequest": true,
	},
	"Issue":       {"id": true, "number": true, "title": true, "state": true, "body": true},
	"IssueConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"PageInfo":    {"hasNextPage": true, "endCursor": true, "hasPreviousPage": true, "startCursor": true},
	"PullRequest": {
		"id": true, "number": true, "state": true, "merged": true,
		"headRefName": true, "baseRefName": true, "mergeable": true, "files": true,
		"title": true, "body": true,
	},
	"PullRequestConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"PullRequestChangedFileConnection": {"nodes": true, "pageInfo": true},
	"PullRequestChangedFile":           {"path": true},
	"Ref":                              {"name": true},
	"IssueCommentEdge":                 {"node": true},
	"IssueComment":                     {"id": true, "body": true},
	"CreateIssuePayload":               {"issue": true, "clientMutationId": true},
	"CloseIssuePayload":                {"issue": true, "clientMutationId": true},
	"CreatePullRequestPayload":         {"pullRequest": true, "clientMutationId": true},
	"MergePullRequestPayload":          {"pullRequest": true, "clientMutationId": true},
	"AddCommentPayload":                {"commentEdge": true, "clientMutationId": true},
	"__Type":                           {"name": true, "fields": true, "enumValues": true},
	"__Field":                          {"name": true},
	"__EnumValue":                      {"name": true},
}

// typeFieldNames returns the field names a __type query lists.
var typeFieldNames = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
}

var searchTypeEnums = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"}

// ---------------------------------------------------------------- executor

type gqlExecutor struct {
	vars   map[string]any
	errors []map[string]any
}

func (e *gqlExecutor) errf(format string, args ...any) {
	e.errors = append(e.errors, map[string]any{"message": fmt.Sprintf(format, args...)})
}

// execute runs one operation and returns its data object.
func (e *gqlExecutor) execute(op *gqlOperation) map[string]any {
	data := map[string]any{}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	for _, f := range op.fields {
		v := e.resolveRoot(rootType, f)
		data[f.alias] = v
	}
	return data
}

// resolveRoot resolves a top-level field.
func (e *gqlExecutor) resolveRoot(rootType string, f *gqlField) any {
	if f.fragment != "" {
		return e.resolveFragment(rootType, nil, f)
	}
	if !typeFields[rootType][f.name] {
		e.errf("Cannot query field %q on type %q.", f.name, rootType)
		return nil
	}
	switch rootType {
	case "Query":
		return e.queryField(f)
	default:
		return e.mutationField(f)
	}
}

func (e *gqlExecutor) queryField(f *gqlField) any {
	switch f.name {
	case "viewer":
		return e.selectFields("User", map[string]any{"login": viewerLogin, "id": viewerID, "name": viewerName}, f.selection)
	case "repository":
		owner := argString(f, "owner", e.vars)
		name := argString(f, "name", e.vars)
		store.mu.RLock()
		repo := store.getRepo(owner, name)
		store.mu.RUnlock()
		if repo == nil {
			return nil
		}
		return e.selectFields("Repository", repo, f.selection)
	case "node":
		id := argString(f, "id", e.vars)
		kind, a, _, ok := parseGlobalID(id)
		if !ok {
			return nil
		}
		if kind == "R" {
			store.mu.RLock()
			repo := store.byID[a]
			store.mu.RUnlock()
			if repo == nil {
				return nil
			}
			return e.selectFields("Repository", repo, f.selection)
		}
		return nil
	case "__type":
		name := argString(f, "name", e.vars)
		return e.selectFields("__Type", name, f.selection)
	}
	return nil
}

func (e *gqlExecutor) mutationField(f *gqlField) any {
	input, _ := resolveValue(f.args["input"], e.vars).(map[string]any)
	if input == nil {
		e.errf("Mutation %q needs an input object.", f.name)
		return nil
	}
	switch f.name {
	case "createIssue":
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		kind, a, _, ok := parseGlobalID(repoID)
		if !ok || kind != "R" {
			e.errf("createIssue: unknown repository id %q.", repoID)
			return nil
		}
		store.mu.Lock()
		repo := store.byID[a]
		if repo == nil {
			store.mu.Unlock()
			e.errf("createIssue: unknown repository id %q.", repoID)
			return nil
		}
		now := tickTime()
		iss := &Issue{ID: store.nextID, Number: repo.nextNumber, Title: title, Body: body, State: "open", CreatedAt: now, UpdatedAt: now}
		store.nextID++
		repo.nextNumber++
		repo.Issues = append(repo.Issues, iss)
		iss.rebuildCache(repo)
		store.mu.Unlock()
		return e.selectFields("CreateIssuePayload", map[string]any{"issue": iss, "repo": repo}, f.selection)
	case "closeIssue":
		issueID, _ := input["issueId"].(string)
		kind, a, b, ok := parseGlobalID(issueID)
		if !ok || kind != "I" {
			e.errf("closeIssue: unknown issue id %q.", issueID)
			return nil
		}
		store.mu.Lock()
		repo := store.byID[a]
		var iss *Issue
		if repo != nil {
			iss = store.findIssue(repo, b)
		}
		if iss == nil {
			store.mu.Unlock()
			e.errf("closeIssue: unknown issue id %q.", issueID)
			return nil
		}
		iss.State = "closed"
		iss.UpdatedAt = tickTime()
		iss.rebuildCache(repo)
		store.mu.Unlock()
		return e.selectFields("CloseIssuePayload", map[string]any{"issue": iss, "repo": repo}, f.selection)
	case "createPullRequest":
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		baseRef, _ := input["baseRefName"].(string)
		headRef, _ := input["headRefName"].(string)
		kind, a, _, ok := parseGlobalID(repoID)
		if !ok || kind != "R" {
			e.errf("createPullRequest: unknown repository id %q.", repoID)
			return nil
		}
		store.mu.Lock()
		repo := store.byID[a]
		if repo == nil {
			store.mu.Unlock()
			e.errf("createPullRequest: unknown repository id %q.", repoID)
			return nil
		}
		headSHA, ok1 := repo.Refs["refs/heads/"+headRef]
		baseSHA, ok2 := repo.Refs["refs/heads/"+baseRef]
		if !ok1 || !ok2 {
			store.mu.Unlock()
			e.errf("createPullRequest: head or base branch does not exist.")
			return nil
		}
		now := tickTime()
		pr := &Pull{ID: store.nextID, Number: repo.nextNumber, Title: title, Body: body, HeadRef: headRef, BaseRef: baseRef, HeadSHA: headSHA, BaseSHA: baseSHA, State: "open", CreatedAt: now, UpdatedAt: now}
		store.nextID++
		repo.nextNumber++
		repo.Pulls = append(repo.Pulls, pr)
		store.mu.Unlock()
		return e.selectFields("CreatePullRequestPayload", map[string]any{"pullRequest": pr, "repo": repo}, f.selection)
	case "mergePullRequest":
		prID, _ := input["pullRequestId"].(string)
		kind, a, b, ok := parseGlobalID(prID)
		if !ok || kind != "P" {
			e.errf("mergePullRequest: unknown pull request id %q.", prID)
			return nil
		}
		store.mu.Lock()
		repo := store.byID[a]
		var pr *Pull
		if repo != nil {
			pr = store.findPull(repo, b)
		}
		if pr == nil {
			store.mu.Unlock()
			e.errf("mergePullRequest: unknown pull request id %q.", prID)
			return nil
		}
		if pr.Merged || pr.State != "open" || !repo.prMergeable(pr) {
			store.mu.Unlock()
			e.errf("mergePullRequest: pull request cannot be merged.")
			return nil
		}
		repo.doMerge(pr)
		store.mu.Unlock()
		return e.selectFields("MergePullRequestPayload", map[string]any{"pullRequest": pr, "repo": repo}, f.selection)
	case "addComment":
		subjectID, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)
		kind, a, b, ok := parseGlobalID(subjectID)
		if !ok || (kind != "I" && kind != "P") {
			e.errf("addComment: unknown subject id %q.", subjectID)
			return nil
		}
		store.mu.Lock()
		repo := store.byID[a]
		var iss *Issue
		if repo != nil && kind == "I" {
			iss = store.findIssue(repo, b)
		}
		if iss == nil {
			store.mu.Unlock()
			e.errf("addComment: unknown subject id %q.", subjectID)
			return nil
		}
		c := &Comment{ID: store.nextID, Body: body, CreatedAt: tickTime()}
		store.nextID++
		iss.Comments = append(iss.Comments, c)
		iss.UpdatedAt = tickTime()
		iss.rebuildCache(repo)
		store.mu.Unlock()
		return e.selectFields("AddCommentPayload", map[string]any{"comment": c, "repo": repo}, f.selection)
	}
	return nil
}

// resolveFragment resolves an inline fragment against a parent
// value: it applies only when the parent's type matches.
func (e *gqlExecutor) resolveFragment(parentType string, parent any, f *gqlField) any {
	if f.fragment != "" && f.fragment != parentType {
		return nil
	}
	return e.selectFields(parentType, parent, f.selection)
}

// selectFields resolves a selection set against a parent value of
// the given type, returning a JSON object.
func (e *gqlExecutor) selectFields(typeName string, parent any, fields []*gqlField) any {
	if parent == nil {
		return nil
	}
	out := map[string]any{}
	for _, f := range fields {
		if f.fragment != "" {
			// An inline fragment merges its fields when the type
			// matches; otherwise it contributes nothing.
			if f.fragment == typeName {
				sub := e.selectFields(typeName, parent, f.selection)
				if m, ok := sub.(map[string]any); ok {
					for k, v := range m {
						out[k] = v
					}
				}
			}
			continue
		}
		if !typeFields[typeName][f.name] {
			e.errf("Cannot query field %q on type %q.", f.name, typeName)
			out[f.alias] = nil
			continue
		}
		out[f.alias] = e.resolveField(typeName, parent, f)
	}
	return out
}

// resolveField resolves one field of a parent value.
func (e *gqlExecutor) resolveField(typeName string, parent any, f *gqlField) any {
	switch typeName {
	case "User":
		m := parent.(map[string]any)
		return m[f.name]
	case "Repository":
		return e.repositoryField(parent.(*Repo), f)
	case "Issue":
		iss, repo := unwrapIssue(parent)
		switch f.name {
		case "id":
			return issueGlobalID(repo.ID, iss.Number)
		case "number":
			return iss.Number
		case "title":
			return iss.Title
		case "state":
			if iss.State == "closed" {
				return "CLOSED"
			}
			return "OPEN"
		case "body":
			return iss.Body
		}
	case "IssueComment":
		m := parent.(map[string]any)
		return m[f.name]
	case "IssueConnection":
		conn := parent.(*issueConn)
		switch f.name {
		case "nodes":
			return e.issueNodes(conn, f)
		case "pageInfo":
			return e.selectFields("PageInfo", conn, f.selection)
		case "totalCount":
			return int64(len(conn.items))
		}
	case "PageInfo":
		conn := parent.(*issueConn)
		switch f.name {
		case "hasNextPage":
			return conn.hasNext
		case "endCursor":
			return conn.endCursor
		case "hasPreviousPage":
			return false
		case "startCursor":
			return conn.startCursor
		}
	case "PullRequest":
		pr, repo := unwrapPull(parent)
		switch f.name {
		case "id":
			return pullGlobalID(repo.ID, pr.Number)
		case "number":
			return pr.Number
		case "state":
			if pr.Merged {
				return "MERGED"
			}
			if pr.State == "closed" {
				return "CLOSED"
			}
			return "OPEN"
		case "merged":
			return pr.Merged
		case "headRefName":
			return pr.HeadRef
		case "baseRefName":
			return pr.BaseRef
		case "mergeable":
			if repo.prMergeable(pr) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "title":
			return pr.Title
		case "body":
			return pr.Body
		case "files":
			return e.selectFields("PullRequestChangedFileConnection", &fileConn{repo: repo, pr: pr, first: argInt(f, "first", e.vars)}, f.selection)
		}
	case "PullRequestConnection":
		conn := parent.(*pullConn)
		switch f.name {
		case "nodes":
			return e.pullNodes(conn, f)
		case "pageInfo":
			return e.selectFields("PageInfo", &issueConn{}, f.selection)
		case "totalCount":
			return int64(len(conn.items))
		}
	case "PullRequestChangedFileConnection":
		conn := parent.(*fileConn)
		switch f.name {
		case "nodes":
			return e.fileNodes(conn, f)
		case "pageInfo":
			return e.selectFields("PageInfo", &issueConn{}, f.selection)
		}
	case "PullRequestChangedFile":
		return parent.(map[string]any)[f.name]
	case "Ref":
		m := parent.(map[string]any)
		return m[f.name]
	case "__Type":
		name := parent.(string)
		switch f.name {
		case "name":
			return name
		case "fields":
			return e.typeFieldsValue(name, f)
		case "enumValues":
			return e.typeEnumValues(name, f)
		}
	case "__Field", "__EnumValue":
		m := parent.(map[string]any)
		return m[f.name]
	case "CreateIssuePayload", "CloseIssuePayload":
		m := parent.(map[string]any)
		if f.name == "issue" {
			iss := m["issue"].(*Issue)
			repo := m["repo"].(*Repo)
			return e.selectFields("Issue", &issueWithRepo{iss: iss, repo: repo}, f.selection)
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		m := parent.(map[string]any)
		if f.name == "pullRequest" {
			pr := m["pullRequest"].(*Pull)
			repo := m["repo"].(*Repo)
			return e.selectFields("PullRequest", &pullWithRepo{pr: pr, repo: repo}, f.selection)
		}
		return nil
	case "AddCommentPayload":
		m := parent.(map[string]any)
		if f.name == "commentEdge" {
			return e.selectFields("IssueCommentEdge", map[string]any{"node": m["comment"]}, f.selection)
		}
		return nil
	case "IssueCommentEdge":
		m := parent.(map[string]any)
		if f.name == "node" {
			c := m["node"].(*Comment)
			return e.selectFields("IssueComment", map[string]any{"id": commentGlobalID(c.ID), "body": c.Body}, f.selection)
		}
		return nil
	}
	return nil
}

// issueWithRepo and pullWithRepo carry a repo alongside an issue
// or pull so global ids resolve. They are handled in resolveField
// through the repoOf helpers below.
type issueWithRepo struct {
	iss  *Issue
	repo *Repo
}

type pullWithRepo struct {
	pr   *Pull
	repo *Repo
}

// unwrapIssue returns the issue and its repository from a parent
// value, which is either an *Issue (repo looked up by number is
// impossible, so callers always pass issueWithRepo) or an
// *issueWithRepo.
func unwrapIssue(parent any) (*Issue, *Repo) {
	if w, ok := parent.(*issueWithRepo); ok {
		return w.iss, w.repo
	}
	return parent.(*Issue), nil
}

func unwrapPull(parent any) (*Pull, *Repo) {
	if w, ok := parent.(*pullWithRepo); ok {
		return w.pr, w.repo
	}
	return parent.(*Pull), nil
}

func (e *gqlExecutor) repositoryField(repo *Repo, f *gqlField) any {
	switch f.name {
	case "id":
		return repoGlobalID(repo.ID)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "owner":
		return e.selectFields("User", map[string]any{"login": repo.Owner, "id": viewerID, "name": viewerName}, f.selection)
	case "defaultBranchRef":
		tip := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if tip == "" {
			return nil
		}
		return e.selectFields("Ref", map[string]any{"name": repo.DefaultBranch}, f.selection)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		n := argInt(f, "number", e.vars)
		iss := store.findIssue(repo, n)
		if iss == nil {
			return nil
		}
		return e.selectFields("Issue", &issueWithRepo{iss: iss, repo: repo}, f.selection)
	case "issues":
		return e.selectFields("IssueConnection", e.issuesConn(repo, f), f.selection)
	case "pullRequests":
		return e.selectFields("PullRequestConnection", e.pullsConn(repo, f), f.selection)
	case "pullRequest":
		n := argInt(f, "number", e.vars)
		pr := store.findPull(repo, n)
		if pr == nil {
			return nil
		}
		return e.selectFields("PullRequest", &pullWithRepo{pr: pr, repo: repo}, f.selection)
	}
	return nil
}

// ---------------------------------------------------------------- connections

type issueConn struct {
	items      []*Issue
	repo       *Repo
	hasNext    bool
	endCursor  any
	startCursor any
}

type pullConn struct {
	items []*Pull
	repo  *Repo
}

type fileConn struct {
	repo  *Repo
	pr    *Pull
	first int64
}

// issuesConn builds the issue connection for a repository field,
// applying states, orderBy and cursor pagination.
func (e *gqlExecutor) issuesConn(repo *Repo, f *gqlField) *issueConn {
	// states: [OPEN] etc.
	var states map[string]bool
	if v, ok := f.args["states"]; ok {
		states = map[string]bool{}
		for _, x := range resolveValue(v, e.vars).([]any) {
			if s, ok := x.(string); ok {
				states[s] = true
			}
		}
	}
	// orderBy: {field: CREATED_AT, direction: DESC}
	direction := "DESC"
	if v, ok := f.args["orderBy"]; ok {
		if m, ok := resolveValue(v, e.vars).(map[string]any); ok {
			if d, ok := m["direction"].(string); ok && d != "" {
				direction = d
			}
		}
	}
	var items []*Issue
	for _, iss := range repo.Issues {
		if states != nil {
			state := "OPEN"
			if iss.State == "closed" {
				state = "CLOSED"
			}
			if !states[state] {
				continue
			}
		}
		items = append(items, iss)
	}
	// CREATED_AT DESC, ties by number DESC; ASC reverses both.
	sort.Slice(items, func(i, j int) bool {
		if direction == "ASC" {
			return items[i].Number < items[j].Number
		}
		return items[i].Number > items[j].Number
	})
	// after cursor: only items after the cursor's number.
	if after := argString(f, "after", e.vars); after != "" {
		if n, ok := decodeCursor(after); ok {
			var kept []*Issue
			for _, iss := range items {
				if direction == "ASC" {
					if iss.Number > n {
						kept = append(kept, iss)
					}
				} else {
					if iss.Number < n {
						kept = append(kept, iss)
					}
				}
			}
			items = kept
		}
	}
	first := argInt(f, "first", e.vars)
	conn := &issueConn{repo: repo}
	if first > 0 && int64(len(items)) > first {
		conn.hasNext = true
		items = items[:first]
	}
	conn.items = items
	if len(items) > 0 {
		conn.endCursor = encodeCursor(items[len(items)-1].Number)
		conn.startCursor = encodeCursor(items[0].Number)
	}
	return conn
}

func (e *gqlExecutor) pullsConn(repo *Repo, f *gqlField) *pullConn {
	first := argInt(f, "first", e.vars)
	items := append([]*Pull{}, repo.Pulls...)
	if first > 0 && int64(len(items)) > first {
		items = items[:first]
	}
	return &pullConn{items: items, repo: repo}
}

func (e *gqlExecutor) issueNodes(conn *issueConn, f *gqlField) any {
	var out []any
	for _, iss := range conn.items {
		out = append(out, e.selectFields("Issue", &issueWithRepo{iss: iss, repo: conn.repo}, f.selection))
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func (e *gqlExecutor) pullNodes(conn *pullConn, f *gqlField) any {
	var out []any
	for _, pr := range conn.items {
		out = append(out, e.selectFields("PullRequest", &pullWithRepo{pr: pr, repo: conn.repo}, f.selection))
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func (e *gqlExecutor) fileNodes(conn *fileConn, f *gqlField) any {
	changes := conn.repo.diffFiles(conn.pr.BaseSHA, conn.pr.HeadSHA)
	var out []any
	for i, c := range changes {
		if conn.first > 0 && int64(i) >= conn.first {
			break
		}
		out = append(out, e.selectFields("PullRequestChangedFile", map[string]any{"path": c.Filename}, f.selection))
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func (e *gqlExecutor) typeFieldsValue(name string, f *gqlField) any {
	names, ok := typeFieldNames[name]
	if !ok {
		return nil
	}
	var out []any
	for _, n := range names {
		out = append(out, e.selectFields("__Field", map[string]any{"name": n}, f.selection))
	}
	return out
}

func (e *gqlExecutor) typeEnumValues(name string, f *gqlField) any {
	if name != "SearchType" {
		return nil
	}
	var out []any
	for _, n := range searchTypeEnums {
		out = append(out, e.selectFields("__EnumValue", map[string]any{"name": n}, f.selection))
	}
	return out
}

func encodeCursor(n int64) string {
	return base64.StdEncoding.EncodeToString([]byte("issue:" + strconv.FormatInt(n, 10)))
}

func decodeCursor(s string) (int64, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	v := strings.TrimPrefix(string(b), "issue:")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------- handler

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		badJSON(w)
		return
	}
	// A body may be one operation document or an array of them.
	var docs []map[string]any
	if trimmed[0] == '[' {
		if err := json.Unmarshal(body, &docs); err != nil {
			badJSON(w)
			return
		}
	} else {
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			badJSON(w)
			return
		}
		docs = []map[string]any{doc}
	}
	results := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		results = append(results, runGraphQLDoc(doc))
	}
	if len(docs) == 1 {
		writeJSON(w, http.StatusOK, results[0])
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func runGraphQLDoc(doc map[string]any) map[string]any {
	query, _ := doc["query"].(string)
	if query == "" {
		return map[string]any{"errors": []map[string]any{{"message": "Must provide query string."}}}
	}
	vars := map[string]any{}
	if v, ok := doc["variables"].(map[string]any); ok {
		vars = v
	}
	op, err := parseGraphQL(query)
	if err != nil {
		return map[string]any{"errors": []map[string]any{{"message": err.Error()}}}
	}
	// operationName selects among several operations; the documents
	// here carry one, so it is only a sanity check.
	if name, _ := doc["operationName"].(string); name != "" && op.name != "" && op.name != name {
		return map[string]any{"errors": []map[string]any{{"message": "Unknown operation named " + name}}}
	}
	e := &gqlExecutor{vars: vars}
	data := e.execute(op)
	out := map[string]any{"data": data}
	if len(e.errors) > 0 {
		out["errors"] = e.errors
	}
	return out
}
