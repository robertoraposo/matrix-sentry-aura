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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"matrixsentry/comms"
	"matrixsentry/memory"
	"matrixsentry/mokoblinks"
	"matrixsentry/sentry"
	"matrixsentry/sentry/access"
)

const protocolVersion = "2024-11-05"

// recallEntry is one entry in the in-RAM recall ring used by analyze_recall.
// The ring holds recent-since-process-start recall events; recalls before a
// restart are not counted (acceptable for a recency coverage metric — the
// durable record is still in the journal as EventRecall).
type recallEntry struct {
	tenant sentry.TenantID
	p      memory.RecallPayload
}

const recallRingCap = 500

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
	mu        sync.Mutex // serializes Append-bearing tool calls deterministically
	store     *sentry.Store
	reg       *sentry.Registry // path→id dictionary for real (file-path) accesses
	mem       *memory.Store    // semantic memory (nil when no embedder is configured)
	chat      *comms.Store     // agent communication channel
	oauth     *oauthProvider   // native OAuth AS for claude.ai (nil when not configured)
	moko      *mokoblinks.Client
	tenant    sentry.TenantID
	token     string         // optional bearer auth for HTTP transport
	tokens    *tokenRegistry // secret→tenant; owner built-in, teams from SENTRY_TOKENS_FILE
	logRecall bool           // journal recall queries as EventRecall (SENTRY_RECALL_LOG)

	recallMu   sync.Mutex   // guards recallRing
	recallRing []recallEntry // bounded ring of recent recalls for analyze_recall — O(≤cap), no journal scan
}

func main() {
	dir := flag.String("dir", "/var/lib/matrix-sentry", "journal directory")
	tenant := flag.Int("tenant", 1, "default tenant id for this agent")
	httpAddr := flag.String("http", "", "listen address for remote Streamable HTTP (e.g. 0.0.0.0:8808); empty = stdio")
	oauthIssuer := flag.String("oauth-issuer", envOr("SENTRY_OAUTH_ISSUER", ""), "public base URL (e.g. https://mcp.example.com) to enable native OAuth for claude.ai connectors; empty = static bearer only")
	ollamaURL := flag.String("ollama", envOr("SENTRY_OLLAMA_URL", ""), "Ollama base URL for embeddings (enables remember/recall); empty = memory tools disabled")
	embedModel := flag.String("embed-model", envOr("SENTRY_EMBED_MODEL", "nomic-embed-text"), "embedding model name")
	embedDim := flag.Int("embed-dim", 768, "embedding dimension (nomic-embed-text = 768)")
	embedProvider := flag.String("embed-provider", envOr("SENTRY_EMBED_PROVIDER", "ollama"), "embedding provider: ollama | mistral")
	dedupTau := flag.Float64("dedup-tau", envFloat("SENTRY_DEDUP_TAU", 0), "squared-L2 dedup radius for remember (0 = off); set from Phase-0 calibration")
	recallGap := flag.Float64("recall-gap", envFloat("SENTRY_RECALL_GAP", 1.20), "truncate recall at the first distance cliff of this ratio (0 = off, plain top-k); 1.20 = calibrated over 20 real queries (F1 peak 1.15-1.20, dominates 1.25)")
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
	s := &server{store: store, reg: reg, moko: moko, tenant: sentry.TenantID(*tenant), logRecall: envBool("SENTRY_RECALL_LOG", true)}

	s.chat, err = comms.New(store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: init comms: %v\n", err)
		os.Exit(1)
	}
	s.chat.SetRetention(envInt("SENTRY_COMMS_RETAIN_N", 2000), time.Duration(envInt("SENTRY_COMMS_RETAIN_DAYS", 14))*24*time.Hour)

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

	// Resolve the embedder for the configured provider. dim defaults to 768 for
	// ollama; mistral-embed is 1024, so when the operator left -embed-dim at its
	// ollama default we bump it to 1024 for mistral (override with -embed-dim).
	model := *embedModel
	dim := *embedDim
	if *embedProvider == "mistral" {
		if model == "nomic-embed-text" { // the ollama default — pick the mistral default instead
			model = "mistral-embed"
		}
		if dim == 768 { // the ollama default
			dim = 1024
		}
	}
	emb, err := resolveEmbedder(*embedProvider, *ollamaURL, model, dim, os.Getenv("SENTRY_MISTRAL_API_KEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: %v\n", err)
		os.Exit(1)
	}
	if emb != nil {
		mem, err := memory.New(store, emb)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sentrymcp: init semantic memory: %v\n", err)
			os.Exit(1)
		}
		s.mem = mem
		s.mem.DedupThreshold = float32(*dedupTau)
		s.mem.RecallGap = float32(*recallGap)
		moko.Info("semantic memory enabled", map[string]string{"provider": *embedProvider, "model": model, "dim": fmt.Sprint(dim)})
	}

	// Native OAuth for claude.ai connectors: enabled when an issuer URL is set.
	// The approval passphrase is the existing SENTRY_MCP_TOKEN, so the owner
	// already holds it. Requires HTTP transport.
	if *oauthIssuer != "" {
		secret := oauthSigningKey(os.Getenv("SENTRY_OAUTH_KEY"), os.Getenv("SENTRY_MCP_TOKEN"))
		if secret == "" {
			fmt.Fprintln(os.Stderr, "sentrymcp: -oauth-issuer requires SENTRY_OAUTH_KEY (or SENTRY_MCP_TOKEN) as the JWT signing key")
			os.Exit(1)
		}
		s.oauth = newOAuth(*oauthIssuer, secret, s.tokens.Tenant)
		extraHosts := envOr("SENTRY_OAUTH_EXTRA_REDIRECT_HOSTS", "")
		s.oauth.setExtraRedirectHosts(extraHosts)
		moko.Info("native OAuth enabled", map[string]string{"issuer": *oauthIssuer, "extra_redirect_hosts": extraHosts})
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
	mux.HandleFunc("/admin/corpus", s.handleAdminCorpus)
	mux.HandleFunc("/admin/journal", s.handleAdminJournal)
	mux.HandleFunc("/admin/comms", s.handleAdminComms)
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

// handleAdminCorpus serves a tenant's full memory corpus (with vectors) for the
// admin dashboard's galaxy view. Auth + tenant come from resolveTenant (same as
// the MCP). Server-to-server only (the admin proxy calls it); no CORS.
func (s *server) handleAdminCorpus(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.mem == nil {
		http.Error(w, "semantic memory disabled (no embedder)", http.StatusServiceUnavailable)
		return
	}
	mems := s.mem.List(tenant)
	type item struct {
		ID   uint64    `json:"id"`
		Text string    `json:"text"`
		Tags []string  `json:"tags,omitempty"`
		Src  string    `json:"src,omitempty"`
		Vec  []float32 `json:"vec"`
	}
	out := struct {
		Tenant   int    `json:"tenant"`
		Dim      int    `json:"dim"`
		Count    int    `json:"count"`
		Memories []item `json:"memories"`
	}{Tenant: int(tenant), Count: len(mems), Memories: make([]item, 0, len(mems))}
	for _, m := range mems {
		out.Dim = len(m.Vector)
		out.Memories = append(out.Memories, item{ID: m.ID, Text: m.Text, Tags: m.Tags, Src: m.Source, Vec: m.Vector})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleAdminJournal serves a tenant's recent SEMANTIC journal events
// (memory writes, tombstones, agent messages) for the dashboard's journal panel.
// Access/PathMap records are excluded (bulk telemetry, no cheap id→path). Auth +
// tenant via resolveTenant. Server-to-server only.
func (s *server) handleAdminJournal(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	limit := 60
	if q := r.URL.Query().Get("limit"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 200 {
		limit = 200
	}
	type ev struct {
		Seq  uint64 `json:"seq"`
		TS   int64  `json:"ts"`
		Type string `json:"type"`
		Text string `json:"text"`
	}
	events := make([]ev, 0, limit)
	t := tenant
	s.store.ScanReverse(sentry.Filter{Tenant: &t}, func(rec sentry.Record) bool {
		var e ev
		e.Seq = uint64(rec.Seq)
		e.TS = rec.Tstamp / 1e6
		switch rec.Type {
		case memory.EventMemory:
			var p memory.MemoryPayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil {
				return true
			}
			e.Type = "Memory"
			e.Text = fmt.Sprintf("#%d %s", p.ID, truncRunes(p.Text, 80))
		case memory.EventForget:
			var p memory.ForgetPayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil {
				return true
			}
			e.Type = "Forget"
			e.Text = fmt.Sprintf("tombstone #%d", p.ID)
		case comms.EventMessage:
			var p comms.MessagePayload
			if sentry.UnmarshalPayload(rec.Payload, &p) != nil {
				return true
			}
			e.Type = "Message"
			tgt := ""
			if p.Target != "" {
				tgt = " → " + p.Target
			}
			e.Text = fmt.Sprintf("%s%s @%s: %s", p.From, tgt, p.Area, truncRunes(p.Text, 60))
		default:
			return true
		}
		events = append(events, e)
		return len(events) < limit
	})
	// ScanReverse yielded newest-first; reverse to chronological for display
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Events []ev `json:"events"`
	}{Events: events})
}

// handleAdminComms serves a tenant's recent channel messages (EventMessage) for
// the dashboard's comms kanban. Auth + tenant via resolveTenant. Server-to-server.
func (s *server) handleAdminComms(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 300 {
		limit = 300
	}
	type msg struct {
		Seq    uint64 `json:"seq"`
		TS     int64  `json:"ts"`
		Area   string `json:"area"`
		From   string `json:"from"`
		Kind   string `json:"kind"`
		Text   string `json:"text"`
		Target string `json:"target,omitempty"`
		Ref    uint64 `json:"ref,omitempty"`
	}
	recent := s.chat.Recent(tenant, limit)
	msgs := make([]msg, 0, len(recent))
	for _, m := range recent {
		msgs = append(msgs, msg{Seq: m.Seq, TS: m.TS / 1e6, Area: m.Area, From: m.From, Kind: m.Kind, Text: m.Text, Target: m.Target, Ref: m.Ref})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Messages []msg `json:"messages"`
	}{Messages: msgs})
}

// truncRunes shortens s to n runes, appending an ellipsis when cut.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
			"name":        "analyze_recall",
			"description": "Measure recall coverage for this tenant: how many recalls have run, the top-hit distance distribution (a high top-distance means recall found nothing relevant — a coverage gap), and the most recent real queries.",
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
			"name":        "post",
			"description": "Post a message to a shared agent channel ('area') so other agents working the same project see it. Use kind=question to ask, kind=answer to reply (set ref to the question's #), kind=info to share, target to direct it at a specific agent (else broadcast).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":   map[string]any{"type": "string", "description": "channel name, e.g. 'projX/backend' (agents agree on names)"},
					"from":   map[string]any{"type": "string", "description": "your agent label, e.g. 'backend' or '01-core'"},
					"text":   map[string]any{"type": "string", "description": "the message"},
					"kind":   map[string]any{"type": "string", "description": "question | answer | info | note (default note)"},
					"target": map[string]any{"type": "string", "description": "optional agent label to direct this at; empty = broadcast"},
					"ref":    map[string]any{"type": "integer", "description": "optional message # this replies to"},
				},
				"required": []any{"area", "from", "text"},
			},
		},
		{
			"name":        "read",
			"description": "Read new messages in an area since a cursor. Pass since=<the last # you saw> to get only newer messages; the response ends with the latest # to use as your next cursor. Poll this to coordinate in near-real-time.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":   map[string]any{"type": "string", "description": "channel name"},
					"since":  map[string]any{"type": "integer", "description": "return only messages with # greater than this (default 0 = all)"},
					"target": map[string]any{"type": "string", "description": "optional: only messages directed at this label (plus broadcasts)"},
				},
				"required": []any{"area"},
			},
		},
		{
			"name":        "comms_clear",
			"description": "Sweep a FINISHED coordination area: drops its messages from the live channel (the durable journal is retained for audit). Use when an area's work is done; promote anything worth keeping to memory first. Tenant-scoped.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"area": map[string]any{"type": "string", "description": "the area to clear"}},
				"required":   []any{"area"},
			},
		},
		{
			"name":        "inbox",
			"description": "Fetch all messages directed at YOU (by target) across every area, in one call — so you never miss a directed message by polling the wrong area. Pass target=<your agent label> and since=<the last # you saw> for only-newer. The response ends with the latest # to use as your next cursor. Reply with post(area=…, kind=\"answer\", ref=<that #>, target=<sender>).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{"type": "string", "description": "your agent label — messages whose target is this are returned"},
					"since":  map[string]any{"type": "integer", "description": "return only messages with # greater than this (default 0 = all)"},
				},
				"required": []any{"target"},
			},
		},
		{
			"name":        "promote",
			"description": "Promote a channel message to durable semantic memory (remember), e.g. a decision or an answer worth keeping. The message stays in the channel; a memory is also created.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area": map[string]any{"type": "string", "description": "channel name"},
					"seq":  map[string]any{"type": "integer", "description": "the message # to promote"},
					"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "optional tags for the memory"},
				},
				"required": []any{"area", "seq"},
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
	case "analyze_recall":
		// Read the in-RAM ring (O(≤recallRingCap)) instead of scanning the
		// journal.  EventRecall is sparse (~116 in a 31k-event journal), so
		// ScanReverse never hit the match cap and scanned everything (~350ms,
		// growing with the journal).  The ring holds recent-since-process-start
		// recalls; historical ones before a restart are not counted — acceptable
		// for a recency coverage metric; the durable record is in the journal.
		s.recallMu.Lock()
		var rps []memory.RecallPayload
		for _, e := range s.recallRing {
			if e.tenant == tenant {
				rps = append(rps, e.p)
			}
		}
		s.recallMu.Unlock()
		var tops []float64
		empty, total := 0, 0
		var recent []string
		// Walk newest-first for the "recent" display.
		for i := len(rps) - 1; i >= 0; i-- {
			p := rps[i]
			total++
			if len(p.Hits) == 0 {
				empty++
			} else {
				tops = append(tops, float64(p.Hits[0].Dist))
			}
			if len(recent) < 8 {
				recent = append(recent, truncRunes(p.Query, 40))
			}
		}
		if total == 0 {
			return s.toolText(req.ID, fmt.Sprintf("recall coverage (tenant %d): total=0 — no recalls logged yet", tenant))
		}
		sort.Float64s(tops)
		pct := func(f float64) float64 {
			if len(tops) == 0 {
				return 0
			}
			i := int(f * float64(len(tops)))
			if i >= len(tops) {
				i = len(tops) - 1
			}
			return tops[i]
		}
		maxTop := 0.0
		if len(tops) > 0 {
			maxTop = tops[len(tops)-1]
		}
		return s.toolText(req.ID, fmt.Sprintf(
			"recall coverage (tenant %d): total=%d empty=%d  topDist min=%.3f p50=%.3f p90=%.3f max=%.3f\nrecent: %q",
			tenant, total, empty, pct(0), pct(0.5), pct(0.9), maxTop, recent))
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
		if s.logRecall {
			rp := memory.RecallPayload{Query: query, K: k, Hits: make([]memory.RecallHit, len(hits))}
			for i, h := range hits {
				rp.Hits[i] = memory.RecallHit{ID: h.ID, Dist: h.Score}
			}
			if _, aerr := s.store.Append(tenant, memory.EventRecall, rp); aerr != nil {
				s.moko.Info("recall-log failed", map[string]string{"tenant": fmt.Sprint(tenant), "err": aerr.Error()})
			}
			s.recallMu.Lock()
			s.recallRing = append(s.recallRing, recallEntry{tenant: tenant, p: rp})
			if len(s.recallRing) > recallRingCap {
				s.recallRing = s.recallRing[len(s.recallRing)-recallRingCap:]
			}
			s.recallMu.Unlock()
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
	case "post":
		area, _ := strArg(p.Args, "area")
		from, _ := strArg(p.Args, "from")
		text, _ := strArg(p.Args, "text")
		if area == "" || from == "" || text == "" {
			return s.toolErr(req.ID, "provide 'area', 'from' and 'text' to post")
		}
		kind, _ := strArg(p.Args, "kind")
		target, _ := strArg(p.Args, "target")
		ref := uintArg(p.Args, "ref")
		s.mu.Lock()
		seq, err := s.chat.Post(tenant, comms.MessagePayload{Area: area, From: from, Kind: kind, Text: text, Target: target, Ref: ref})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "post failed: "+err.Error())
		}
		s.moko.Info("post", map[string]string{"tenant": fmt.Sprint(tenant), "area": area, "from": from, "seq": fmt.Sprint(seq)})
		return s.toolText(req.ID, fmt.Sprintf("posted message #%d in %s", seq, area))
	case "read":
		area, _ := strArg(p.Args, "area")
		if area == "" {
			return s.toolErr(req.ID, "provide 'area' to read")
		}
		since := uintArg(p.Args, "since")
		target, _ := strArg(p.Args, "target")
		msgs := s.chat.Read(tenant, area, since)
		const readCap = 100
		if len(msgs) > readCap {
			msgs = msgs[len(msgs)-readCap:]
		}
		var b strings.Builder
		var last uint64 = since
		n := 0
		for _, m := range msgs {
			if target != "" && m.Target != "" && m.Target != target {
				continue // filter: keep broadcasts + those addressed to target
			}
			to := m.Target
			if to == "" {
				to = "all"
			}
			fmt.Fprintf(&b, "#%d [%s] %s→%s: %s\n", m.Seq, m.Kind, m.From, to, m.Text)
			if m.Seq > last {
				last = m.Seq
			}
			n++
		}
		if n == 0 {
			return s.toolText(req.ID, fmt.Sprintf("no new messages in %s since #%d", area, since))
		}
		fmt.Fprintf(&b, "(cursor: #%d)", last)
		return s.toolText(req.ID, b.String())
	case "comms_clear":
		area, _ := strArg(p.Args, "area")
		if area == "" {
			return s.toolErr(req.ID, "provide 'area' to clear")
		}
		n, err := s.chat.Clear(tenant, area)
		if err != nil {
			return s.toolErr(req.ID, "comms_clear failed: "+err.Error())
		}
		return s.toolText(req.ID, fmt.Sprintf("cleared %d message(s) from area %q (journal retained)", n, area))
	case "inbox":
		target, _ := strArg(p.Args, "target")
		if target == "" {
			return s.toolErr(req.ID, "provide 'target' (your agent label) to read your inbox")
		}
		var inboxSince uint64
		if v, ok := numArg(p.Args, "since"); ok && v > 0 {
			inboxSince = uint64(v)
		}
		msgs := s.chat.Inbox(tenant, target, inboxSince)
		var b strings.Builder
		if len(msgs) == 0 {
			fmt.Fprintf(&b, "inbox empty for %q (since #%d)", target, inboxSince)
		} else {
			fmt.Fprintf(&b, "%d message(s) for %q:\n", len(msgs), target)
			cursor := inboxSince
			for _, m := range msgs {
				tgt := m.Target
				if tgt == "" {
					tgt = "all"
				}
				fmt.Fprintf(&b, "#%d [%s] %s→%s @%s: %s\n", m.Seq, m.Kind, m.From, tgt, m.Area, m.Text)
				if m.Seq > cursor {
					cursor = m.Seq
				}
			}
			fmt.Fprintf(&b, "(cursor: #%d)", cursor)
		}
		return s.toolText(req.ID, b.String())
	case "promote":
		if s.mem == nil {
			return s.toolErr(req.ID, "semantic memory disabled: no embedder configured (start sentrymcp with -ollama URL)")
		}
		area, _ := strArg(p.Args, "area")
		seq := uintArg(p.Args, "seq")
		if area == "" || seq == 0 {
			return s.toolErr(req.ID, "provide 'area' and 'seq' to promote")
		}
		m, ok := s.chat.Get(tenant, area, seq)
		if !ok {
			return s.toolErr(req.ID, fmt.Sprintf("message #%d not found in %s", seq, area))
		}
		userTags := stringsArg(p.Args, "tags")
		tags := make([]string, len(userTags)+1)
		copy(tags, userTags)
		tags[len(userTags)] = "promoted"
		s.mu.Lock()
		id, _, _, err := s.mem.Remember(tenant, fmt.Sprintf("[%s %s#%d] %s", m.From, area, seq, m.Text), memory.RememberOpts{Tags: tags, Src: "promote"})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "promote failed: "+err.Error())
		}
		s.moko.Info("promote", map[string]string{"tenant": fmt.Sprint(tenant), "area": area, "seq": fmt.Sprint(seq), "memid": fmt.Sprint(id)})
		return s.toolText(req.ID, fmt.Sprintf("promoted message #%d in %s → memory #%d", seq, area, id))
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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
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

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v != "0" && strings.ToLower(v) != "false"
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
