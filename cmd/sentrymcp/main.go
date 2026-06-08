// Command sentrymcp is a minimal, pure-Go (zero-dep) MCP server exposing Matrix
// Sentry's SentryLog to an agent (Claude Code, Cursor, …). It speaks two
// transports:
//
//   - stdio (default): local subprocess, newline-delimited JSON-RPC.
//   - Streamable HTTP (-http :PORT): remote, so Claude Code on another machine
//     connects over an IP. POST {url}/mcp with a JSON-RPC message.
//
// Every tool call is appended to the durable journal AND mirrored to MokoBlinks,
// so the engine's real-world behaviour is watchable live. This is the bridge that
// turns synthetic access streams into REAL ones.
//
//	go build -o sentrymcp ./cmd/sentrymcp
//	# local:  claude mcp add matrix-sentry -- /path/sentrymcp -dir /var/sentry
//	# remote: ./sentrymcp -http 0.0.0.0:8808 -dir /var/sentry   (then --transport http)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"matrixsentry/mokoblinks"
	"matrixsentry/sentry"
	"matrixsentry/sentry/access"
)

const protocolVersion = "2024-11-05"

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type server struct {
	mu     sync.Mutex // serializes Append-bearing tool calls deterministically
	store  *sentry.Store
	reg    *sentry.Registry // path→id dictionary for real (file-path) accesses
	moko   *mokoblinks.Client
	tenant sentry.TenantID
	token  string // optional bearer auth for HTTP transport
}

func main() {
	dir := flag.String("dir", "/var/lib/matrix-sentry", "journal directory")
	tenant := flag.Int("tenant", 1, "default tenant id for this agent")
	httpAddr := flag.String("http", "", "listen address for remote Streamable HTTP (e.g. 0.0.0.0:8808); empty = stdio")
	flag.Parse()

	store, err := sentry.Open(*dir, sentry.Options{FsyncEvery: 75 * time.Millisecond})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: open journal: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	reg, err := sentry.NewRegistry(store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: rebuild path registry: %v\n", err)
		os.Exit(1)
	}

	moko := mokoblinks.FromEnv()
	s := &server{store: store, reg: reg, moko: moko, tenant: sentry.TenantID(*tenant)}

	if *httpAddr != "" {
		moko.Info("sentrymcp listening (http)", map[string]string{"addr": *httpAddr, "dir": *dir, "tenant": fmt.Sprint(*tenant)})
		moko.Flush()
		s.serveHTTP(*httpAddr)
		return
	}
	moko.Info("sentrymcp started (stdio)", map[string]string{"dir": *dir, "tenant": fmt.Sprint(*tenant)})
	moko.Flush()
	s.serveStdio()
	moko.Flush()
}

// --- transports ---

func (s *server) serveStdio() {
	out := bufio.NewWriter(os.Stdout)
	r := bufio.NewReaderSize(bufio.NewReader(os.Stdin), 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if resp, ok := s.dispatch(line); ok {
				b, _ := json.Marshal(resp)
				out.Write(b)
				out.WriteByte('\n')
				out.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

func (s *server) serveHTTP(addr string) {
	s.token = os.Getenv("SENTRY_MCP_TOKEN") // optional bearer auth
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHTTP)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp http: %v\n", err)
		os.Exit(1)
	}
}

func (s *server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Ship any buffered MokoBlinks lines after every request so the live log
	// mirror stays current under low volume (batch flush alone would not fire).
	defer func() { go s.moko.Flush() }()

	if r.Method == http.MethodGet && r.URL.Path == "/" {
		fmt.Fprintln(w, "matrix-sentry mcp ok")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST /mcp", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	resp, ok := s.dispatch(body)
	if !ok {
		w.WriteHeader(http.StatusAccepted) // notification: no response body
		return
	}
	if resp.ID != nil && resp.Result != nil {
		// Streamable HTTP: hand the client a session id on initialize.
		if mInit(body) {
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("ms-%d", time.Now().UnixNano()))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func mInit(body []byte) bool {
	var r rpcReq
	return json.Unmarshal(body, &r) == nil && r.Method == "initialize"
}

// --- JSON-RPC dispatch (shared by both transports) ---

// dispatch parses one JSON-RPC message and returns the response (ok=false for
// notifications, which get no response).
func (s *server) dispatch(line []byte) (rpcResp, bool) {
	var req rpcReq
	if err := json.Unmarshal(line, &req); err != nil {
		return rpcResp{}, false
	}
	notification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		return s.ok(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "matrix-sentry", "version": "0.1.0"},
		}), true
	case "notifications/initialized", "notifications/cancelled":
		return rpcResp{}, false
	case "ping":
		return s.ok(req.ID, map[string]any{}), true
	case "tools/list":
		return s.ok(req.ID, map[string]any{"tools": toolList()}), true
	case "tools/call":
		return s.callTool(req), true
	default:
		if notification {
			return rpcResp{}, false
		}
		return s.fail(req.ID, -32601, "method not found: "+req.Method), true
	}
}

func toolList() []map[string]any {
	return []map[string]any{
		{
			"name":        "record_access",
			"description": "Record that the agent accessed a memory/decision item. Builds the real access stream Matrix Sentry's predictive allocation learns from. Provide one of: 'item' (a stable integer id, for synthetic streams), 'path' (a file path, mapped to a stable sequential id server-side), or 'paths' (a batch of file paths from one tool use). 'src' tags the originating tool.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"item":  map[string]any{"type": "integer", "description": "stable id of the accessed memory item"},
					"path":  map[string]any{"type": "string", "description": "file path accessed; mapped to a stable sequential id"},
					"paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "batch of file paths accessed in one tool use"},
					"src":   map[string]any{"type": "string", "description": "originating tool (e.g. Read, Edit, Bash)"},
				},
			},
		},
		{
			"name":        "analyze_access",
			"description": "Measure the predictability (lift = Markov vs marginal next-access hit-rate) of this agent's access stream. lift>0 means access has sequential structure predictive bit-allocation can exploit.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "stats",
			"description": "Return how many events are stored in the journal.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func (s *server) callTool(req rpcReq) rpcResp {
	var p struct {
		Name string         `json:"name"`
		Args map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return s.fail(req.ID, -32602, "invalid params")
	}
	switch p.Name {
	case "record_access":
		src, _ := strArg(p.Args, "src")
		paths := pathArgs(p.Args)
		if len(paths) > 0 {
			s.mu.Lock()
			var ids []uint64
			for _, path := range paths {
				id, _, err := s.reg.Record(s.tenant, path, src)
				if err != nil {
					s.mu.Unlock()
					return s.toolErr(req.ID, "append failed: "+err.Error())
				}
				ids = append(ids, id)
			}
			s.mu.Unlock()
			s.moko.Info("record_access", map[string]string{
				"tenant": fmt.Sprint(s.tenant), "src": src,
				"paths": fmt.Sprint(len(paths)), "items": fmt.Sprint(ids),
			})
			return s.toolText(req.ID, fmt.Sprintf("recorded %d access(es) src=%q items=%v", len(paths), src, ids))
		}
		item, okArg := numArg(p.Args, "item")
		if !okArg {
			return s.toolErr(req.ID, "provide one of 'item', 'path', or 'paths'")
		}
		s.mu.Lock()
		seq, err := s.store.Append(s.tenant, sentry.EventAccess, sentry.AccessPayload{ItemID: uint64(item), Source: src})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "append failed: "+err.Error())
		}
		s.moko.Info("record_access", map[string]string{"tenant": fmt.Sprint(s.tenant), "item": fmt.Sprint(uint64(item)), "seq": fmt.Sprint(seq)})
		return s.toolText(req.ID, fmt.Sprintf("recorded access item=%d as seq=%d", uint64(item), seq))
	case "analyze_access":
		rep, err := access.Analyze(s.store, s.tenant)
		if err != nil {
			return s.toolErr(req.ID, "analyze failed: "+err.Error())
		}
		s.moko.Info("analyze_access", map[string]string{
			"tenant": fmt.Sprint(s.tenant), "total": fmt.Sprint(rep.Total),
			"lift": fmt.Sprintf("%.4f", rep.Lift), "markov": fmt.Sprintf("%.4f", rep.MarkovHit),
			"marginal": fmt.Sprintf("%.4f", rep.MarginalHit), "coverage": fmt.Sprintf("%.4f", rep.Coverage),
		})
		return s.toolText(req.ID, fmt.Sprintf(
			"access analysis (tenant %d): total=%d  markovHit=%.1f%%  marginalHit=%.1f%%  LIFT=%.1f%%  coverage=%.1f%%\n%s",
			s.tenant, rep.Total, rep.MarkovHit*100, rep.MarginalHit*100, rep.Lift*100, rep.Coverage*100, liftVerdict(rep.Lift)))
	case "stats":
		return s.toolText(req.ID, fmt.Sprintf("journal holds %d events", s.store.ReadNextSeq()-1))
	default:
		return s.toolErr(req.ID, "unknown tool: "+p.Name)
	}
}

func liftVerdict(lift float64) string {
	if lift > 0.05 {
		return "→ access has exploitable sequential structure (predictive allocation pays off)."
	}
	return "→ access looks ~memoryless so far (predictive allocation ≈ marginal); collect more or expect flat gains."
}

func numArg(args map[string]any, key string) (float64, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

func strArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// pathArgs collects file paths from either the single 'path' arg or the 'paths'
// array, preserving order. Empty/non-string entries are ignored.
func pathArgs(args map[string]any) []string {
	var out []string
	if p, ok := strArg(args, "path"); ok && p != "" {
		out = append(out, p)
	}
	if raw, ok := args["paths"].([]any); ok {
		for _, v := range raw {
			if p, ok := v.(string); ok && p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (s *server) ok(id json.RawMessage, result any) rpcResp {
	return rpcResp{JSONRPC: "2.0", ID: id, Result: result}
}
func (s *server) fail(id json.RawMessage, code int, msg string) rpcResp {
	return rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}}
}
func (s *server) toolText(id json.RawMessage, text string) rpcResp {
	return s.ok(id, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
}
func (s *server) toolErr(id json.RawMessage, text string) rpcResp {
	return s.ok(id, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true})
}
