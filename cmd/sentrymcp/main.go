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
	"strconv"
	"strings"
	"sync"
	"time"

	"matrixsentry/memory"
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
	mem    *memory.Store    // semantic memory (nil when no embedder is configured)
	oauth  *oauthProvider   // native OAuth AS for claude.ai (nil when not configured)
	moko   *mokoblinks.Client
	tenant sentry.TenantID
	token  string          // optional bearer auth for HTTP transport
	tokens *tokenRegistry  // secret→tenant; owner built-in, teams from SENTRY_TOKENS_FILE
}

func main() {
	dir := flag.String("dir", "/var/lib/matrix-sentry", "journal directory")
	tenant := flag.Int("tenant", 1, "default tenant id for this agent")
	httpAddr := flag.String("http", "", "listen address for remote Streamable HTTP (e.g. 0.0.0.0:8808); empty = stdio")
	oauthIssuer := flag.String("oauth-issuer", envOr("SENTRY_OAUTH_ISSUER", ""), "public base URL (e.g. https://mcp.example.com) to enable native OAuth for claude.ai connectors; empty = static bearer only")
	ollamaURL := flag.String("ollama", envOr("SENTRY_OLLAMA_URL", ""), "Ollama base URL for embeddings (enables remember/recall); empty = memory tools disabled")
	embedModel := flag.String("embed-model", envOr("SENTRY_EMBED_MODEL", "nomic-embed-text"), "embedding model name")
	embedDim := flag.Int("embed-dim", 768, "embedding dimension (nomic-embed-text = 768)")
	dedupTau := flag.Float64("dedup-tau", envFloat("SENTRY_DEDUP_TAU", 0), "squared-L2 dedup radius for remember (0 = off); set from Phase-0 calibration")
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

	// Build the token registry from SENTRY_TOKENS_FILE (opt-in multi-tenant).
	// With no file, only the owner entry exists → identical to single-tenant today.
	// Note: s.token is set later in serveHTTP, but SENTRY_MCP_TOKEN is stable here.
	ownerSecret := os.Getenv("SENTRY_MCP_TOKEN")
	var tokensErr error
	s.tokens, tokensErr = loadTokenRegistry(envOr("SENTRY_TOKENS_FILE", ""), ownerSecret, s.tenant)
	if tokensErr != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: %v\n", tokensErr)
		os.Exit(1)
	}

	// Semantic memory is enabled only when an embedder is configured. Without
	// -ollama the journal/telemetry tools still work; remember/recall report
	// that embeddings are not configured rather than failing opaquely.
	if *ollamaURL != "" {
		emb := memory.NewOllamaEmbedder(*ollamaURL, *embedModel, *embedDim)
		mem, err := memory.New(store, emb)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sentrymcp: init semantic memory: %v\n", err)
			os.Exit(1)
		}
		s.mem = mem
		s.mem.DedupThreshold = float32(*dedupTau)
		moko.Info("semantic memory enabled", map[string]string{"ollama": *ollamaURL, "model": *embedModel, "dim": fmt.Sprint(*embedDim)})
	}

	// Native OAuth for claude.ai connectors: enabled when an issuer URL is set.
	// The approval passphrase is the existing SENTRY_MCP_TOKEN, so the owner
	// already holds it. Requires HTTP transport.
	if *oauthIssuer != "" {
		secret := os.Getenv("SENTRY_MCP_TOKEN")
		if secret == "" {
			fmt.Fprintln(os.Stderr, "sentrymcp: -oauth-issuer requires SENTRY_MCP_TOKEN (used as the consent passphrase + signing key)")
			os.Exit(1)
		}
		s.oauth = newOAuth(*oauthIssuer, secret, s.tokens.Tenant)
		moko.Info("native OAuth enabled", map[string]string{"issuer": *oauthIssuer})
	}

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
			if resp, ok := s.dispatch(line, s.tenant); ok {
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
	if s.oauth != nil {
		// OAuth discovery + endpoints (served at the public issuer root).
		mux.HandleFunc("/.well-known/oauth-protected-resource", s.oauth.handleProtectedResource)
		mux.HandleFunc("/.well-known/oauth-authorization-server", s.oauth.handleAuthServerMeta)
		// Some MCP clients append the resource path to the discovery URL.
		mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.oauth.handleProtectedResource)
		mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", s.oauth.handleAuthServerMeta)
		mux.HandleFunc("/register", s.oauth.handleRegister)
		mux.HandleFunc("/authorize", s.oauth.handleAuthorize)
		mux.HandleFunc("/token", s.oauth.handleToken)
	}
	mux.HandleFunc("/", s.handleHTTP)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp http: %v\n", err)
		os.Exit(1)
	}
}

// resolveTenant maps an HTTP request to its tenant via the credential: a static
// bearer secret in the registry, or an OAuth access token's tnt claim. Returns
// (_, false) → 401. Open/local mode (no static token and no OAuth) → default.
func (s *server) resolveTenant(r *http.Request) (sentry.TenantID, bool) {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		secret := strings.TrimPrefix(auth, "Bearer ")
		if t, ok := s.tokens.Tenant(secret); ok {
			return t, true
		}
		if s.oauth != nil {
			if cl, ok := s.oauth.verifyToken(secret, "access"); ok {
				if cl.Tnt != 0 {
					return cl.Tnt, true
				}
				return s.tenant, true // tnt-less (legacy) token → default tenant
			}
		}
	}
	if s.token == "" && s.oauth == nil && len(s.tokens.entries) == 0 {
		return s.tenant, true // open/local mode — no auth configured at all
	}
	return 0, false
}

func (s *server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Ship any buffered MokoBlinks lines after every request so the live log
	// mirror stays current under low volume (batch flush alone would not fire).
	defer func() { go s.moko.Flush() }()

	// CORS for the browser-based claude.ai connector (also answers preflight).
	if s.oauth != nil && s.oauth.cors(w, r) {
		return
	}

	if r.Method == http.MethodGet && r.URL.Path == "/" {
		fmt.Fprintln(w, "matrix-sentry mcp ok")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "use POST /mcp", http.StatusMethodNotAllowed)
		return
	}
	tenant, ok := s.resolveTenant(r)
	if !ok {
		if s.oauth != nil {
			w.Header().Set("WWW-Authenticate", s.oauth.wwwAuthenticate())
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	resp, ok := s.dispatch(body, tenant)
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
func (s *server) dispatch(line []byte, tenant sentry.TenantID) (rpcResp, bool) {
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
		return s.callTool(req, tenant), true
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
			"name":        "remember",
			"description": "Store a durable memory so future sessions (this agent or another model) can recall it semantically. Use for decisions, conventions, gotchas, and context worth surviving a context reset. The text is embedded and indexed; recall finds it by meaning, not keywords.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":       map[string]any{"type": "string", "description": "the memory to store (a fact, decision, or piece of context)"},
					"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "optional labels for grouping/filtering"},
					"src":        map[string]any{"type": "string", "description": "optional originating tool or context"},
					"supersedes": map[string]any{"type": "integer", "description": "optional id of an existing memory this fact updates or corrects; replaces it instead of storing a contradicting duplicate"},
					"force": map[string]any{"type": "boolean", "description": "store even if a near-duplicate already exists; use only when your fact is genuinely distinct from what recall/remember reports, not a restatement"},
				},
				"required": []any{"text"},
			},
		},
		{
			"name":        "recall",
			"description": "Retrieve the memories most semantically relevant to a query, so the agent can recover context it stored earlier instead of starting from amnesia. Returns the closest matches ranked by similarity.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "what you want to remember about"},
					"k":     map[string]any{"type": "integer", "description": "max results to return (default 5)"},
				},
				"required": []any{"query"},
			},
		},
		{
			"name":        "forget",
			"description": "Remove a memory from recall by id (e.g. an accidental duplicate or a wrong fact). The record stays in the journal history; it just stops being returned by recall. Use deliberately — there is no automatic un-forget.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "integer", "description": "the memory id to forget (as shown by recall, e.g. the #N)"},
				},
				"required": []any{"id"},
			},
		},
		{
			"name":        "stats",
			"description": "Return how many events are stored in the journal.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func (s *server) callTool(req rpcReq, tenant sentry.TenantID) rpcResp {
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
				id, _, err := s.reg.Record(tenant, path, src)
				if err != nil {
					s.mu.Unlock()
					return s.toolErr(req.ID, "append failed: "+err.Error())
				}
				ids = append(ids, id)
			}
			s.mu.Unlock()
			s.moko.Info("record_access", map[string]string{
				"tenant": fmt.Sprint(tenant), "src": src,
				"paths": fmt.Sprint(len(paths)), "items": fmt.Sprint(ids),
			})
			return s.toolText(req.ID, fmt.Sprintf("recorded %d access(es) src=%q items=%v", len(paths), src, ids))
		}
		item, okArg := numArg(p.Args, "item")
		if !okArg {
			return s.toolErr(req.ID, "provide one of 'item', 'path', or 'paths'")
		}
		s.mu.Lock()
		seq, err := s.store.Append(tenant, sentry.EventAccess, sentry.AccessPayload{ItemID: uint64(item), Source: src})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "append failed: "+err.Error())
		}
		s.moko.Info("record_access", map[string]string{"tenant": fmt.Sprint(tenant), "item": fmt.Sprint(uint64(item)), "seq": fmt.Sprint(seq)})
		return s.toolText(req.ID, fmt.Sprintf("recorded access item=%d as seq=%d", uint64(item), seq))
	case "analyze_access":
		rep, err := access.Analyze(s.store, tenant)
		if err != nil {
			return s.toolErr(req.ID, "analyze failed: "+err.Error())
		}
		s.moko.Info("analyze_access", map[string]string{
			"tenant": fmt.Sprint(tenant), "total": fmt.Sprint(rep.Total),
			"lift": fmt.Sprintf("%.4f", rep.Lift), "markov": fmt.Sprintf("%.4f", rep.MarkovHit),
			"marginal": fmt.Sprintf("%.4f", rep.MarginalHit), "coverage": fmt.Sprintf("%.4f", rep.Coverage),
		})
		return s.toolText(req.ID, fmt.Sprintf(
			"access analysis (tenant %d): total=%d  markovHit=%.1f%%  marginalHit=%.1f%%  LIFT=%.1f%%  coverage=%.1f%%\n%s",
			tenant, rep.Total, rep.MarkovHit*100, rep.MarginalHit*100, rep.Lift*100, rep.Coverage*100, liftVerdict(rep.Lift)))
	case "remember":
		if s.mem == nil {
			return s.toolErr(req.ID, "semantic memory disabled: no embedder configured (start sentrymcp with -ollama URL)")
		}
		text, _ := strArg(p.Args, "text")
		if text == "" {
			return s.toolErr(req.ID, "provide 'text' to remember")
		}
		src, _ := strArg(p.Args, "src")
		tags := stringsArg(p.Args, "tags")
		supersedes := uintArg(p.Args, "supersedes")
		force := boolArg(p.Args, "force")
		s.mu.Lock()
		id, deduped, superseded, err := s.mem.Remember(tenant, text, memory.RememberOpts{Tags: tags, Src: src, Supersedes: supersedes, Force: force})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "remember failed: "+err.Error())
		}
		s.moko.Info("remember", map[string]string{"tenant": fmt.Sprint(tenant), "id": fmt.Sprint(id), "tags": fmt.Sprint(tags), "len": fmt.Sprint(len(text)), "deduped": fmt.Sprint(deduped), "superseded": fmt.Sprint(superseded)})
		switch {
		case superseded != 0:
			return s.toolText(req.ID, fmt.Sprintf("remembered as memory #%d, superseding #%d", id, superseded))
		case deduped:
			return s.toolText(req.ID, fmt.Sprintf("already known as memory #%d (deduped, not stored again)", id))
		case supersedes != 0:
			return s.toolText(req.ID, fmt.Sprintf("superseded id #%d not found for this tenant; remembered as memory #%d", supersedes, id))
		default:
			return s.toolText(req.ID, fmt.Sprintf("remembered as memory #%d", id))
		}
	case "recall":
		if s.mem == nil {
			return s.toolErr(req.ID, "semantic memory disabled: no embedder configured (start sentrymcp with -ollama URL)")
		}
		query, _ := strArg(p.Args, "query")
		if query == "" {
			return s.toolErr(req.ID, "provide 'query' to recall")
		}
		k := 5
		if v, ok := numArg(p.Args, "k"); ok && int(v) > 0 {
			k = int(v)
		}
		hits, err := s.mem.Recall(tenant, query, k)
		if err != nil {
			return s.toolErr(req.ID, "recall failed: "+err.Error())
		}
		s.moko.Info("recall", map[string]string{"tenant": fmt.Sprint(tenant), "k": fmt.Sprint(k), "hits": fmt.Sprint(len(hits))})
		return s.toolText(req.ID, formatRecall(query, hits))
	case "forget":
		if s.mem == nil {
			return s.toolErr(req.ID, "semantic memory disabled: no embedder configured (start sentrymcp with -ollama URL)")
		}
		id := uintArg(p.Args, "id")
		if id == 0 {
			return s.toolErr(req.ID, "provide an 'id' to forget")
		}
		s.mu.Lock()
		forgotten, err := s.mem.Forget(tenant, id)
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "forget failed: "+err.Error())
		}
		s.moko.Info("forget", map[string]string{"tenant": fmt.Sprint(tenant), "id": fmt.Sprint(id), "forgotten": fmt.Sprint(forgotten)})
		if forgotten {
			return s.toolText(req.ID, fmt.Sprintf("forgot memory #%d (removed from recall; still in the journal history)", id))
		}
		return s.toolText(req.ID, fmt.Sprintf("memory #%d not found for this tenant", id))
	case "stats":
		return s.toolText(req.ID, fmt.Sprintf("journal holds %d events", s.store.ReadNextSeq()-1))
	default:
		return s.toolErr(req.ID, "unknown tool: "+p.Name)
	}
}

// formatRecall renders recall hits as readable, model-friendly text.
func formatRecall(query string, hits []memory.Memory) string {
	if len(hits) == 0 {
		return fmt.Sprintf("no memories found for %q (0 stored matches)", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d relevant memor%s for %q (closest first):\n", len(hits), plural(len(hits)), query)
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. [#%d", i+1, h.ID)
		if len(h.Tags) > 0 {
			fmt.Fprintf(&b, " %v", h.Tags)
		}
		fmt.Fprintf(&b, " dist=%.3f] %s\n", h.Score, h.Text)
	}
	return strings.TrimRight(b.String(), "\n")
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// stringsArg pulls a []string from a JSON array argument (ignoring non-strings).
func stringsArg(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		fmt.Fprintf(os.Stderr, "sentrymcp: %s=%q: not a valid float, using default %v\n", key, v, def)
	}
	return def
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

// boolArg reads a boolean arg; missing or non-bool yields false.
func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// uintArg reads a non-negative integer arg. JSON numbers arrive as float64 in a
// map[string]any; a json.Number is handled defensively. Missing/invalid -> 0.
func uintArg(args map[string]any, key string) uint64 {
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return uint64(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return uint64(n)
		}
	}
	return 0
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
