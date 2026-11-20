// MCP over streamable HTTP: JSON-RPC 2.0 at POST /mcp.
//
// The server speaks the modern era (server/discover, 2026-07-28)
// and the legacy handshake (initialize, <=2025-11-25), so the
// client takes whichever path its probe finds. tools/list is
// pre-encoded once so two calls answer byte for byte the same
// list. Tool results carry the object as JSON text and as
// structuredContent; the checks read the first JSON either way.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const mcpRevision = "2026-07-28"

var mcpRevisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// mcpTools is the tool list, in a fixed order.
var mcpTools = []map[string]any{
	{
		"name":        "create_repo",
		"description": "Create a repository owned by the authenticated user.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "private": map[string]any{"type": "boolean"}},
			"required":   []string{"name"},
		},
	},
	{
		"name":        "get_repo",
		"description": "Get a repository by owner and name.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}},
			"required":   []string{"owner", "repo"},
		},
	},
	{
		"name":        "create_issue",
		"description": "Create an issue in a repository.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}},
			"required":   []string{"owner", "repo", "title"},
		},
	},
	{
		"name":        "list_issues",
		"description": "List issues in a repository.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "state": map[string]any{"type": "string"}},
			"required":   []string{"owner", "repo"},
		},
	},
	{
		"name":        "create_pull_request",
		"description": "Create a pull request in a repository.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}, "head": map[string]any{"type": "string"}, "base": map[string]any{"type": "string"}},
			"required":   []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		"name":        "merge_pull_request",
		"description": "Merge a pull request in a repository.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "pull_number": map[string]any{"type": "number"}},
			"required":   []string{"owner", "repo", "pull_number"},
		},
	},
}

var toolsListOnce sync.Once
var toolsListJSON []byte

func toolsListBytes() []byte {
	toolsListOnce.Do(func() {
		toolsListJSON, _ = json.Marshal(map[string]any{"tools": mcpTools})
	})
	return toolsListJSON
}

// ---------------------------------------------------------------- JSON-RPC plumbing

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": result}
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "error": map[string]any{"code": code, "message": message}}
}

// rawID returns the request's id as a JSON value (null when absent).
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(id, &v); err == nil {
		return v
	}
	return nil
}

// isNotification reports whether the request carries no id.
func isNotification(req *rpcRequest) bool {
	return len(bytes.TrimSpace(req.ID)) == 0
}

// ---------------------------------------------------------------- handler

func handleMCP(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeMCPParseError(w, nil)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		writeMCPParseError(w, nil)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeMCPParseError(w, nil)
		return
	}
	if req.Method == "" {
		writeMCPParseError(w, req.ID)
		return
	}
	// A notification (no id) is answered with 202 and no body.
	if isNotification(&req) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "server/discover":
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"resultType":        "server/discover",
			"supportedVersions": mcpRevisions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion": mcpRevision,
				"io.modelcontextprotocol/serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
			},
		}))
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "forge-session-1")
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		}))
	case "tools/list":
		// Byte-identical across calls: the pre-encoded list.
		out := map[string]any{"jsonrpc": "2.0", "id": rawID(req.ID), "result": json.RawMessage(toolsListBytes())}
		writeJSON(w, http.StatusOK, out)
	case "tools/call":
		handleToolCall(w, r, &req)
	case "ping":
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{}))
	default:
		writeJSON(w, http.StatusOK, rpcError(req.ID, -32601, "Method not found: "+req.Method))
	}
}

// writeMCPParseError answers a non-JSON body with HTTP 400 and
// JSON-RPC -32700.
func writeMCPParseError(w http.ResponseWriter, id json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(rpcError(id, -32700, "Parse error"))
}

// ---------------------------------------------------------------- tools/call

func handleToolCall(w http.ResponseWriter, r *http.Request, req *rpcRequest) {
	// The Mcp-Name header is mirrored on tools/call.
	if name := r.Header.Get("Mcp-Name"); name != "" {
		w.Header().Set("Mcp-Name", name)
	}
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	// Match the tool name as a suffix, case-insensitively, so a
	// server prefix (github__create_issue) still matches.
	name := strings.ToLower(params.Name)
	var canonical string
	for _, t := range mcpTools {
		toolName := t["name"].(string)
		if name == toolName || strings.HasSuffix(name, "__"+toolName) || strings.HasSuffix(name, "_"+toolName) {
			canonical = toolName
			break
		}
	}
	if canonical == "" {
		writeJSON(w, http.StatusOK, rpcError(req.ID, -32602, "Unknown tool: "+params.Name))
		return
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}
	// full_name carries owner/repo; split it when owner/repo are
	// missing, as the suite's mapper does.
	if full, ok := args["full_name"].(string); ok {
		if _, hasOwner := args["owner"]; !hasOwner {
			if parts := strings.SplitN(full, "/", 2); len(parts) == 2 {
				args["owner"], args["repo"] = parts[0], parts[1]
			}
		}
	}
	// Required arguments per tool.
	required := map[string][]string{
		"create_repo":         {"name"},
		"get_repo":            {"owner", "repo"},
		"create_issue":        {"owner", "repo", "title"},
		"list_issues":         {"owner", "repo"},
		"create_pull_request": {"owner", "repo", "title", "head", "base"},
		"merge_pull_request":  {"owner", "repo", "pull_number"},
	}
	for _, key := range required[canonical] {
		if _, ok := args[key]; !ok {
			writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "missing required argument: " + key}},
				"isError": true,
			}))
			return
		}
	}
	result, toolErr := runTool(canonical, args)
	if toolErr != "" {
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": toolErr}},
			"isError": true,
		}))
		return
	}
	text, _ := json.Marshal(result)
	writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": result,
		"isError":           false,
	}))
}

func argStr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argNum(args map[string]any, key string) int64 {
	switch v := args[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// runTool executes a tool against the store. It returns the result
// object, or a tool error message.
func runTool(name string, args map[string]any) (any, string) {
	switch name {
	case "create_repo":
		repoName := argStr(args, "name")
		desc := argStr(args, "description")
		private := true
		if b, ok := args["private"].(bool); ok {
			private = b
		}
		repo, err := store.createRepo(viewerLogin, repoName, private, desc)
		if err != nil {
			return nil, "repository already exists: " + repoName
		}
		return repoJSON(repo), ""
	case "get_repo":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner != viewerLogin {
			return nil, "repository not found: " + owner + "/" + repoName
		}
		store.mu.RLock()
		repo := store.getRepo(owner, repoName)
		store.mu.RUnlock()
		if repo == nil {
			return nil, "repository not found: " + owner + "/" + repoName
		}
		return repoJSON(repo), ""
	case "create_issue":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner != viewerLogin {
			return nil, "repository not found: " + owner + "/" + repoName
		}
		store.mu.Lock()
		repo := store.getRepo(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			return nil, "repository not found: " + owner + "/" + repoName
		}
		now := tickTime()
		iss := &Issue{ID: store.nextID, Number: repo.nextNumber, Title: argStr(args, "title"), Body: argStr(args, "body"), State: "open", CreatedAt: now, UpdatedAt: now}
		store.nextID++
		repo.nextNumber++
		repo.Issues = append(repo.Issues, iss)
		iss.rebuildCache(repo)
		out := issueJSON(repo, iss)
		store.mu.Unlock()
		return out, ""
	case "list_issues":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner != viewerLogin {
			return nil, "repository not found: " + owner + "/" + repoName
		}
		state := argStr(args, "state")
		if state == "" {
			state = "open"
		}
		store.mu.RLock()
		repo := store.getRepo(owner, repoName)
		if repo == nil {
			store.mu.RUnlock()
			return nil, "repository not found: " + owner + "/" + repoName
		}
		var out []map[string]any
		for _, iss := range repo.Issues {
			if state != "all" && iss.State != state {
				continue
			}
			out = append(out, issueJSON(repo, iss))
		}
		store.mu.RUnlock()
		if out == nil {
			out = []map[string]any{}
		}
		return out, ""
	case "create_pull_request":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner != viewerLogin {
			return nil, "repository not found: " + owner + "/" + repoName
		}
		store.mu.Lock()
		repo := store.getRepo(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			return nil, "repository not found: " + owner + "/" + repoName
		}
		headSHA, ok1 := repo.Refs["refs/heads/"+argStr(args, "head")]
		baseSHA, ok2 := repo.Refs["refs/heads/"+argStr(args, "base")]
		if !ok1 || !ok2 {
			store.mu.Unlock()
			return nil, "head or base branch does not exist"
		}
		now := tickTime()
		pr := &Pull{ID: store.nextID, Number: repo.nextNumber, Title: argStr(args, "title"), Body: argStr(args, "body"), HeadRef: argStr(args, "head"), BaseRef: argStr(args, "base"), HeadSHA: headSHA, BaseSHA: baseSHA, State: "open", CreatedAt: now, UpdatedAt: now}
		store.nextID++
		repo.nextNumber++
		repo.Pulls = append(repo.Pulls, pr)
		out := pullJSON(repo, pr)
		store.mu.Unlock()
		return out, ""
	case "merge_pull_request":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner != viewerLogin {
			return nil, "repository not found: " + owner + "/" + repoName
		}
		store.mu.Lock()
		repo := store.getRepo(owner, repoName)
		if repo == nil {
			store.mu.Unlock()
			return nil, "repository not found: " + owner + "/" + repoName
		}
		pr := store.findPull(repo, argNum(args, "pull_number"))
		if pr == nil {
			store.mu.Unlock()
			return nil, "pull request not found"
		}
		if pr.Merged || pr.State != "open" || !repo.prMergeable(pr) {
			store.mu.Unlock()
			return nil, "pull request cannot be merged"
		}
		repo.doMerge(pr)
		out := pullJSON(repo, pr)
		store.mu.Unlock()
		return out, ""
	}
	return nil, "unknown tool"
}
