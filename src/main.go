// Command forge is a GitHub-compatible git forge in one process.
//
// It serves git smart HTTP (protocol v2 and v0), the REST v3 API,
// GraphQL v4, and MCP over streamable HTTP from a single in-memory
// store. Its state is a deterministic function of the requests it
// receives: timestamps come from a logical clock seeded by
// MAKEFUN_TRIAL_NOW, never from the wall clock, and nothing random
// is ever generated. Two runs on the same instance therefore answer
// identically.
//
// The lifecycle is the harness's: bind an ephemeral port, register
// it at $MAKEFUN_ENV_URL, serve until the harness reports done, then
// exit 0. With FORGE_PORT set and MAKEFUN_ENV_URL unset it serves
// standalone on that port instead (local testing only).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The scenario's fixed identities. The harness builds its scenario
// from one constant seed, so the viewer, teammate, stranger and org
// logins are the same in every trial, practice or scored.
const (
	viewerLogin   = "adaiversen"
	viewerID      = 1000
	viewerName    = "ada iversen"
	viewerEmail   = "adaiversen@example.invalid"
	teammateLogin = "quinnkeller"
	teammateID    = 1001
	strangerLogin = "olgabauer"
	strangerID    = 1002
	orgLogin      = "faint-meadow-org"
)

const zeroOID = "0000000000000000000000000000000000000000"

// ---------------------------------------------------------------- clock

// clockBase is the only "now" the forge knows. It comes from
// MAKEFUN_TRIAL_NOW (RFC 3339, or epoch milliseconds) and defaults
// to 2025-01-01T00:00:00Z. Every stored timestamp is clockBase plus
// a logical tick, so two runs stamp identical values.
var clockBase time.Time
var clockTick atomic.Int64

func initClock() {
	clockBase = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if raw := os.Getenv("MAKEFUN_TRIAL_NOW"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			clockBase = t
		} else if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			clockBase = time.UnixMilli(ms).UTC()
		}
	}
}

// tickTime returns the next logical instant, one second after the
// previous one. Deterministic: it depends only on call order.
func tickTime() time.Time {
	n := clockTick.Add(1)
	return clockBase.Add(time.Duration(n) * time.Second)
}

func tickUnix() int64 { return tickTime().Unix() }

// isoTime formats t the way GitHub formats timestamps.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ---------------------------------------------------------------- globals

var store *Store
var shopToken string
var reqCounter atomic.Uint64

func main() {
	initClock()
	store = NewStore()
	shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		if port := os.Getenv("FORGE_PORT"); port != "" {
			serveStandalone(port)
			return
		}
		fmt.Fprintln(os.Stderr, "MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	serveHarness(envURL)
}

// serveStandalone listens on a fixed port for local testing.
func serveStandalone(port string) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "forge listening on %s\n", ln.Addr())
	srv := &http.Server{Handler: handler()}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

// serveHarness runs the harness lifecycle: bind an ephemeral port,
// register it, poll /done until true, then exit 0.
func serveHarness(envURL string) {
	envURL = strings.TrimSuffix(envURL, "/")
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: handler()}
	go func() {
		_ = srv.Serve(ln)
	}()

	client := &http.Client{Timeout: 30 * time.Second}
	if !register(client, envURL, port) {
		fmt.Fprintln(os.Stderr, "register failed")
		os.Exit(1)
	}
	waitDone(client, envURL)
	_ = srv.Close()
	os.Exit(0)
}

func register(client *http.Client, envURL string, port int) bool {
	body, _ := json.Marshal(map[string]int{"port": port})
	delay := 250 * time.Millisecond
	for attempt := 0; attempt < 40; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case 200, 409:
				return true
			case 400, 405:
				return false
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
	return false
}

func waitDone(client *http.Client, envURL string) {
	delay := 250 * time.Millisecond
	for {
		req, err := http.NewRequest("GET", envURL+"/done", nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err == nil {
			var done struct {
				Done bool `json:"done"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&done)
			resp.Body.Close()
			if done.Done {
				return
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// ---------------------------------------------------------------- http plumbing

func handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GitHub-Request-Id", "req-"+strconv.FormatUint(reqCounter.Add(1), 10))
		route(w, r)
	})
}

// writeJSON writes v as JSON with the given status. w must already
// carry the request id header.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeRawJSON writes pre-encoded JSON bytes.
func writeRawJSON(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// badJSON is the answer to a malformed JSON request body.
func badJSON(w http.ResponseWriter) { errJSON(w, http.StatusBadRequest, "Problems parsing JSON") }

// readBody reads a request body, capped at 64 MiB.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 64<<20))
}

// authed reports whether the request carries the shop token. The
// health probe needs no auth; every API route does. A request with
// no token is 401; with a wrong token, 401 too. When no shop token
// is configured (local testing) any bearer token is accepted.
func authed(r *http.Request) bool {
	if shopToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+shopToken
}

func needAuth(w http.ResponseWriter, r *http.Request) bool {
	if !authed(r) {
		errJSON(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	return true
}

// route dispatches one request. It is a single switch on path
// prefixes, kept allocation-light on the hot paths.
func route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if path == "/healthz" {
		if r.Method != "GET" {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if path == "/api/v3" || path == "/api/v3/" {
		if !needAuth(w, r) {
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSON(w, http.StatusOK, map[string]string{
			"current_user_url": "/api/v3/user",
			"rate_limit_url":   "/api/v3/rate_limit",
		})
		return
	}

	if strings.HasPrefix(path, "/api/v3/") {
		routeAPI(w, r, strings.TrimPrefix(path, "/api/v3/"))
		return
	}

	if path == "/api/graphql" {
		if r.Method != "POST" {
			notFound(w)
			return
		}
		if !needAuth(w, r) {
			return
		}
		handleGraphQL(w, r)
		return
	}

	if path == "/mcp" {
		if r.Method != "POST" {
			notFound(w)
			return
		}
		if !needAuth(w, r) {
			return
		}
		handleMCP(w, r)
		return
	}

	if strings.HasSuffix(path, ".git/info/refs") || strings.Contains(path, ".git/git-") {
		routeGit(w, r)
		return
	}

	notFound(w)
}

// routeAPI handles everything under /api/v3/.
func routeAPI(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	switch {
	case rest == "meta" && r.Method == "GET":
		if !needAuth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"installed_version": "1.0.0"})
		return
	case rest == "user" && r.Method == "GET":
		if !needAuth(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, userJSON(viewerLogin, viewerID, viewerName, viewerEmail))
		return
	case rest == "user/repos":
		if !needAuth(w, r) {
			return
		}
		switch r.Method {
		case "POST":
			createRepoHandler(w, r)
		case "GET":
			listUserReposHandler(w, r)
		default:
			notFound(w)
		}
		return
	case len(parts) >= 2 && parts[0] == "repos":
		if !needAuth(w, r) {
			return
		}
		routeRepo(w, r, parts[1:])
		return
	}
	notFound(w)
}

// userJSON is the GitHub user object for a login.
func userJSON(login string, id int64, name, email string) map[string]any {
	return map[string]any{
		"login":      login,
		"id":         id,
		"node_id":    "U_" + strconv.FormatInt(id, 10),
		"avatar_url": "https://example.invalid/avatar/" + login,
		"html_url":   "https://example.invalid/" + login,
		"type":       "User",
		"name":       name,
		"email":      email,
	}
}
