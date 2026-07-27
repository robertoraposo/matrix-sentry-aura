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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"matrixsentry/blob"
	"matrixsentry/comms"
	"matrixsentry/memory"
	"matrixsentry/mokoblinks"
	"matrixsentry/providerbroker"
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
	mu             sync.Mutex // serializes Append-bearing tool calls deterministically
	store          *sentry.Store
	reg            *sentry.Registry // path→id dictionary for real (file-path) accesses
	mem            *memory.Store    // semantic memory (nil when no embedder is configured)
	chat           *comms.Store     // agent communication channel
	blobs          *blob.Store      // content-addressed image bytes (comms image transfer)
	oauth          *oauthProvider   // native OAuth AS for claude.ai (nil when not configured)
	moko           *mokoblinks.Client
	providers      *providerbroker.Registry
	providerDaemon *providerDaemonClient
	ollamaClient   *http.Client
	ollamaURL      string
	tenant         sentry.TenantID
	token          string         // optional bearer auth for HTTP transport
	tokens         *tokenRegistry // secret→tenant; owner built-in, teams from SENTRY_TOKENS_FILE
	logRecall      bool           // journal recall queries as EventRecall (SENTRY_RECALL_LOG)

	recallMu   sync.Mutex    // guards recallRing
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
	s := &server{
		store:     store,
		reg:       reg,
		moko:      moko,
		providers: defaultProviderRegistry(),
		providerDaemon: newProviderDaemonClient(
			envOr("SENTRY_PROVIDERD_URL", ""),
			os.Getenv("SENTRY_PROVIDERD_TOKEN"),
		),
		ollamaClient: &http.Client{Timeout: 2 * time.Minute},
		ollamaURL:    *ollamaURL,
		tenant:       sentry.TenantID(*tenant),
		logRecall:    envBool("SENTRY_RECALL_LOG", true),
	}

	s.chat, err = comms.New(store)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: init comms: %v\n", err)
		os.Exit(1)
	}
	s.chat.SetRetention(envInt("SENTRY_COMMS_RETAIN_N", 2000), time.Duration(envInt("SENTRY_COMMS_RETAIN_DAYS", 14))*24*time.Hour)
	// Lifecycle-v2: claim lease length, presence staleness, owner override label,
	// and the eager sweeper (all defaulted; each knob is backward-compatible).
	s.chat.SetLeaseTTL(time.Duration(envInt("SENTRY_COMMS_LEASE_MIN", 15)) * time.Minute)
	s.chat.SetPresenceStale(time.Duration(envInt("SENTRY_COMMS_PRESENCE_STALE_SEC", 900)) * time.Second)
	s.chat.SetOwnerLabel(envOr("SENTRY_COMMS_OWNER_LABEL", ""))
	// Sweeper on the process lifecycle context: eager lease/deadline/TTL/presence
	// expiry even in a quiet channel. Nudges publish AFTER s.mu is released (Start
	// preserves the lock-order invariant). Cancelled when main returns.
	sweepCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()
	s.chat.Start(sweepCtx, time.Duration(envInt("SENTRY_COMMS_SWEEP_SEC", 30))*time.Second)

	s.blobs, err = blob.Open(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentrymcp: init blob store: %v\n", err)
		os.Exit(1)
	}
	// Startup GC: drop orphan blob files no live message references and that
	// aren't pinned. The journal is untouched.
	if n, err := s.blobs.Sweep(s.chat.LiveBlobIDs()); err != nil {
		moko.Warn("blob gc at startup failed", map[string]string{"err": err.Error()})
	} else if n > 0 {
		moko.Info("blob gc at startup", map[string]string{"deleted": fmt.Sprint(n)})
	}

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

	providerHealthEvery := time.Duration(
		envInt("SENTRY_PROVIDER_HEALTH_SEC", 30),
	) * time.Second
	if providerHealthEvery < time.Second {
		providerHealthEvery = time.Second
	}

	providerHealthTicker := time.NewTicker(providerHealthEvery)
	defer providerHealthTicker.Stop()

	go monitorOllamaStatus(
		sweepCtx,
		s.providers,
		&http.Client{Timeout: 3 * time.Second},
		*ollamaURL,
		providerHealthTicker.C,
	)

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

// dlDir is the directory of public release artifacts (install.sh, signed
// sentry-reflect binaries + .sig). Overridable via SENTRY_DL_DIR.
func dlDir() string {
	if d := os.Getenv("SENTRY_DL_DIR"); d != "" {
		return d
	}
	return "/root/sentry-dl"
}

// handleInstallScript serves the public one-liner installer (no auth).
func (s *server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(filepath.Join(dlDir(), "install.sh"))
	if err != nil {
		http.Error(w, "install.sh not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// handleDownload serves a single signed release artifact from dlDir (no auth,
// read-only). The name is a bare filename — no subpaths or traversal.
func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/dl/")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.Error(w, "bad artifact name", http.StatusBadRequest)
		return
	}
	f, err := os.Open(filepath.Join(dlDir(), name))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, fi.ModTime(), f)
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
	mux.HandleFunc("/comms/subscribe", s.handleCommsSubscribe)
	mux.HandleFunc("/install.sh", s.handleInstallScript)
	mux.HandleFunc("/dl/", s.handleDownload)
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

// handleCommsSubscribe is an SSE stream that pushes a "nudge" whenever comms
// activity matching the subscriber's filter (target and/or areas, tenant-scoped)
// occurs. The nudge is advisory; the client fetches via its normal read/inbox.
func (s *server) handleCommsSubscribe(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.resolveTenant(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	target := q.Get("target")
	var areas []string
	if a := q.Get("areas"); a != "" {
		areas = strings.Split(a, ",")
	}
	if target == "" && len(areas) == 0 {
		http.Error(w, "provide target and/or areas", http.StatusBadRequest)
		return
	}
	since := parseUintQuery(q.Get("since"))

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	f := comms.Filter{Tenant: tenant, Target: target, Areas: areas}

	// Subscribe FIRST so no message can fall into the gap between catch-up and
	// the channel registration. A post in the window either appears in
	// MatchingSince or arrives on ch — worst case a duplicate nudge, which is
	// idempotent (the client re-reads from its cursor).
	ch, cancel := s.chat.Subscribe(f)
	defer cancel()

	// Catch-up: if anything matching is newer than the client's cursor, nudge once.
	if latest := s.chat.MatchingSince(f, since); latest > since {
		writeNudge(w, comms.Nudge{Seq: latest})
		flusher.Flush()
	}

	hb := time.NewTicker(25 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case n := <-ch:
			writeNudge(w, n)
			flusher.Flush()
		case <-hb.C:
			if _, err := io.WriteString(w, ":hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeNudge emits one SSE "nudge" event with the nudge JSON as data.
func writeNudge(w io.Writer, n comms.Nudge) {
	b, _ := json.Marshal(n)
	fmt.Fprintf(w, "event: nudge\ndata: %s\n\n", b)
}

// parseUintQuery parses a non-negative integer query value, defaulting to 0.
func parseUintQuery(s string) uint64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
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
		var ip struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &ip)
		return s.ok(req.ID, map[string]any{
			"protocolVersion": negotiateVersion(ip.ProtocolVersion),
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

// taskFieldSchema is the outputSchema fragment for a message's task sub-object,
// shared by read and inbox (state required; holder/leaseUntil/deadline optional).
func taskFieldSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"state":      map[string]any{"type": "string", "description": "pending|claimed|done|cancel|overdue"},
			"holder":     map[string]any{"type": "string"},
			"leaseUntil": map[string]any{"type": "integer", "description": "unix nanos"},
			"deadline":   map[string]any{"type": "integer", "description": "unix nanos; 0=none"},
		},
		"required": []any{"state"},
	}
}

func toolList() []map[string]any {
	tools := []map[string]any{
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
					"force":      map[string]any{"type": "boolean", "description": "store even if a near-duplicate already exists; use only when your fact is genuinely distinct from what recall/remember reports, not a restatement"},
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
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"count": map[string]any{"type": "integer", "description": "number of memories returned"},
					"memories": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":       map[string]any{"type": "integer"},
								"distance": map[string]any{"type": "number", "description": "squared-L2 distance, smaller=closer"},
								"text":     map[string]any{"type": "string"},
								"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							},
							"required": []any{"id", "distance", "text"},
						},
					},
				},
				"required": []any{"query", "count", "memories"},
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
			"description": "Post a message to a shared agent channel ('area') so other agents working the same project see it. Use kind=question to ask, kind=answer to reply (set ref to the question's #), kind=info to share, kind=task for a claimable unit of work (claim/resolve it), target to direct it at a specific agent (else broadcast). Optional ttl expires an ephemeral message; deadline (kind=task) flags it overdue when passed.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":     map[string]any{"type": "string", "description": "channel name, e.g. 'projX/backend' (agents agree on names)"},
					"from":     map[string]any{"type": "string", "description": "your agent label, e.g. 'backend' or '01-core'"},
					"text":     map[string]any{"type": "string", "description": "the message"},
					"kind":     map[string]any{"type": "string", "description": "question | answer | info | note | task (default note)"},
					"target":   map[string]any{"type": "string", "description": "optional agent label to direct this at; empty = broadcast"},
					"ref":      map[string]any{"type": "integer", "description": "optional message # this replies to"},
					"ttl":      map[string]any{"type": "string", "description": "optional lifetime as a duration ('90s', '10m', '2h', or integer seconds); the message drops out of the channel after it (journal is retained)"},
					"deadline": map[string]any{"type": "string", "description": "optional deadline for a kind=task as a duration from now ('10m', '2h', integer seconds); the task flags overdue once passed"},
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
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":   map[string]any{"type": "string"},
					"target": map[string]any{"type": "string"},
					"cursor": map[string]any{"type": "integer", "description": "highest # shown; use as the next since"},
					"presence": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"agent":  map[string]any{"type": "string"},
								"area":   map[string]any{"type": "string"},
								"status": map[string]any{"type": "string"},
								"ageSec": map[string]any{"type": "integer", "description": "seconds since the last heartbeat"},
							},
							"required": []any{"agent", "status", "ageSec"},
						},
					},
					"messages": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"seq":    map[string]any{"type": "integer"},
								"from":   map[string]any{"type": "string"},
								"target": map[string]any{"type": "string", "description": "directed agent label; empty = broadcast"},
								"kind":   map[string]any{"type": "string"},
								"text":   map[string]any{"type": "string"},
								"image": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"blob": map[string]any{"type": "string"},
										"mime": map[string]any{"type": "string"},
										"w":    map[string]any{"type": "integer"},
										"h":    map[string]any{"type": "integer"},
										"size": map[string]any{"type": "integer"},
									},
								},
								"task": taskFieldSchema(),
							},
							"required": []any{"seq", "from", "kind", "text"},
						},
					},
				},
				"required": []any{"area", "cursor", "messages"},
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
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{"type": "string"},
					"count":  map[string]any{"type": "integer", "description": "number of messages returned"},
					"cursor": map[string]any{"type": "integer", "description": "highest # shown; use as the next since"},
					"messages": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"seq":    map[string]any{"type": "integer"},
								"area":   map[string]any{"type": "string"},
								"from":   map[string]any{"type": "string"},
								"target": map[string]any{"type": "string", "description": "directed agent label; empty = broadcast"},
								"kind":   map[string]any{"type": "string"},
								"text":   map[string]any{"type": "string"},
								"image": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"blob": map[string]any{"type": "string"},
										"mime": map[string]any{"type": "string"},
										"w":    map[string]any{"type": "integer"},
										"h":    map[string]any{"type": "integer"},
									},
								},
								"task": taskFieldSchema(),
							},
							"required": []any{"seq", "area", "from", "kind", "text"},
						},
					},
				},
				"required": []any{"target", "count", "cursor", "messages"},
			},
		},
		{
			"name":        "post_image",
			"description": "Share an image with other agents on a channel ('area'). Pass the image as base64 in 'data' with its 'mime' (image/*). The bytes are stored server-side; other agents call get_image with the returned # to fetch it. Use 'caption' for a text description, 'target' to direct it at one agent. Max 15 MB.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"area":    map[string]any{"type": "string", "description": "channel name"},
					"from":    map[string]any{"type": "string", "description": "your agent label"},
					"data":    map[string]any{"type": "string", "description": "the image bytes, base64-encoded"},
					"mime":    map[string]any{"type": "string", "description": "image mime type, e.g. image/png, image/jpeg, image/webp"},
					"caption": map[string]any{"type": "string", "description": "optional text describing the image"},
					"target":  map[string]any{"type": "string", "description": "optional agent label to direct this at; empty = broadcast"},
					"w":       map[string]any{"type": "integer", "description": "optional width in px (auto-detected for png/jpeg/gif if omitted)"},
					"h":       map[string]any{"type": "integer", "description": "optional height in px (auto-detected for png/jpeg/gif if omitted)"},
				},
				"required": []any{"area", "from", "data", "mime"},
			},
		},
		{
			"name":        "get_image",
			"description": "Fetch an image posted to a channel by its message # (as shown by read/inbox/post_image). Returns the image itself so you can view it. Tenant-scoped.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"seq": map[string]any{"type": "integer", "description": "the image message #"}},
				"required":   []any{"seq"},
			},
		},
		{
			"name":        "pin_image",
			"description": "Pin an image so it survives channel retention and comms_clear (the blob is never GC'd while pinned). Pass the image message #.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"seq": map[string]any{"type": "integer", "description": "the image message # to pin"}},
				"required":   []any{"seq"},
			},
		},
		{
			"name":        "unpin_image",
			"description": "Remove a pin from an image so it can be garbage-collected once no live message references it. Pass the image message #.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"seq": map[string]any{"type": "integer", "description": "the image message # to unpin"}},
				"required":   []any{"seq"},
			},
		},
		{
			"name":        "blob_gc",
			"description": "Delete orphan image blobs no live message references and that aren't pinned (the journal is retained). Owner/orchestrator housekeeping.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
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
			"name":        "claim",
			"description": "Claim a task message (kind=task) so exactly one agent holds it. Atomic: the first caller wins; a second claim while the lease is live is DENIED. Re-claiming as the current holder renews the lease (doubles as a 'still working' heartbeat). An un-renewed lease auto-expires so hung tasks free up.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"seq": map[string]any{"type": "integer", "description": "the task message # to claim"},
					"by":  map[string]any{"type": "string", "description": "your agent label — becomes the holder"},
				},
				"required": []any{"seq", "by"},
			},
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"seq":        map[string]any{"type": "integer"},
					"claimed":    map[string]any{"type": "boolean", "description": "true if you now hold it; false = DENIED (someone else holds a live lease)"},
					"holder":     map[string]any{"type": "string", "description": "the live holder (you on success, the winner on DENIED)"},
					"leaseUntil": map[string]any{"type": "integer", "description": "unix nanos the lease is held until"},
					"state":      map[string]any{"type": "string", "description": "pending|claimed|done|cancel|overdue"},
					"deadline":   map[string]any{"type": "integer", "description": "unix nanos; 0=none"},
				},
				"required": []any{"seq", "claimed", "holder", "leaseUntil", "state"},
			},
		},
		{
			"name":        "resolve",
			"description": "Close a task you hold as done or cancel. Only the current holder (or the tenant owner) may resolve; a resolved task rejects further claims.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"seq":   map[string]any{"type": "integer", "description": "the task message # to resolve"},
					"by":    map[string]any{"type": "string", "description": "your agent label — must be the holder (or owner)"},
					"state": map[string]any{"type": "string", "description": "done | cancel (default done)"},
				},
				"required": []any{"seq", "by"},
			},
			"outputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"seq":      map[string]any{"type": "integer"},
					"resolved": map[string]any{"type": "boolean"},
					"state":    map[string]any{"type": "string", "description": "done | cancel"},
					"by":       map[string]any{"type": "string", "description": "the agent that resolved it"},
				},
				"required": []any{"seq", "resolved", "state", "by"},
			},
		},
		{
			"name":        "heartbeat",
			"description": "Update your ephemeral presence slot (last-status-wins) instead of posting a STANDBY message. Creates NO channel message and is never journaled; read/inbox show live agents from these slots. Send periodically; the slot goes stale and disappears if you stop.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from":   map[string]any{"type": "string", "description": "your agent label"},
					"status": map[string]any{"type": "string", "description": "what you're doing, e.g. 'building', 'idle', 'running tests'"},
					"area":   map[string]any{"type": "string", "description": "optional channel you're working in"},
				},
				"required": []any{"from", "status"},
			},
		},
		{
			"name":        "stats",
			"description": "Return how many events are stored in the journal.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"outputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"events": map[string]any{"type": "integer", "description": "count of events stored in the journal"}},
				"required":   []any{"events"},
			},
		},
	}
	return append(providerToolDefinitions(), tools...)
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
	case "provider_list",
		"provider_status",
		"provider_connect",
		"provider_connect_cancel",
		"provider_disconnect",
		"provider_invoke":
		return s.handleProviderTool(req.ID, tenant, p.Name, p.Args)
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
		return s.toolStruct(req.ID, formatRecall(query, hits), recallStruct(query, hits))
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
		// Lifecycle-v2: optional ttl (→ExpiresAt) and deadline (→Deadline), both
		// relative durations resolved to absolute unix-nanos from now. Invalid
		// values are a tool error, never a silent never-expires.
		now := time.Now()
		exp, _, ttlErr := durArg(p.Args, "ttl", now)
		if ttlErr != nil {
			return s.toolErr(req.ID, "invalid ttl: "+ttlErr.Error())
		}
		dl, _, dlErr := durArg(p.Args, "deadline", now)
		if dlErr != nil {
			return s.toolErr(req.ID, "invalid deadline: "+dlErr.Error())
		}
		// Spec: a deadline is only recorded for kind=task. The deadline arg is still
		// validated above for any kind (invalid → tool error, never a silent drop),
		// but its value is applied only to tasks — a note/question/etc. records
		// Deadline==0 regardless of what was passed. (ttl is unaffected: any kind.)
		deadline := int64(0)
		if kind == "task" {
			deadline = dl
		}
		s.mu.Lock()
		seq, err := s.chat.Post(tenant, comms.MessagePayload{Area: area, From: from, Kind: kind, Text: text, Target: target, Ref: ref, ExpiresAt: exp, Deadline: deadline})
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
		now := time.Now()
		var b strings.Builder
		// Lifecycle-v2: prepend a compact presence section (nothing when no live
		// agents → byte-identical to v1) before the message lines.
		slots := s.chat.PresenceList(tenant)
		writePresence(&b, slots, now)
		var last uint64 = since
		n := 0
		// structuredContent (MCP 2025-06-18): mirror EXACTLY the data the text lines
		// show — one entry per displayed message, with image/task sub-objects where
		// the text carries an image line or a ⟨task⟩ suffix. Never nil so it marshals
		// to a JSON array.
		outMsgs := []map[string]any{}
		for _, m := range msgs {
			if target != "" && m.Target != "" && m.Target != target {
				continue // filter: keep broadcasts + those addressed to target
			}
			to := m.Target
			if to == "" {
				to = "all"
			}
			sm := map[string]any{"seq": m.Seq, "from": m.From, "target": m.Target, "text": m.Text}
			if m.BlobID != "" {
				fmt.Fprintf(&b, "#%d [image] %s→%s: %s [%s %dx%d %dB · get_image(%d)]\n", m.Seq, m.From, to, m.Text, m.Mime, m.W, m.H, m.Size, m.Seq)
				sm["kind"] = "image"
				sm["image"] = map[string]any{"blob": m.BlobID, "mime": m.Mime, "w": m.W, "h": m.H, "size": m.Size}
			} else {
				// Task messages gain a live-state suffix; non-tasks (TaskOf ok=false)
				// render byte-identical to v1.
				fmt.Fprintf(&b, "#%d [%s] %s→%s: %s", m.Seq, m.Kind, m.From, to, m.Text)
				sm["kind"] = m.Kind
				if ts, ok := s.chat.TaskOf(tenant, m.Seq); ok {
					b.WriteString(taskSuffix(ts))
					sm["task"] = taskStruct(ts)
				}
				b.WriteByte('\n')
			}
			outMsgs = append(outMsgs, sm)
			if m.Seq > last {
				last = m.Seq
			}
			n++
		}
		structured := map[string]any{
			"area":     area,
			"cursor":   last,
			"presence": presenceStruct(slots, now),
			"messages": outMsgs,
		}
		if target != "" {
			structured["target"] = target
		}
		if n == 0 {
			// No new messages: keep v1's exact text, only prefixed by presence (if any).
			fmt.Fprintf(&b, "no new messages in %s since #%d", area, since)
			return s.toolStruct(req.ID, b.String(), structured)
		}
		fmt.Fprintf(&b, "(cursor: #%d)", last)
		return s.toolStruct(req.ID, b.String(), structured)
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
		now := time.Now()
		var b strings.Builder
		// Lifecycle-v2: prepend the presence section (empty → byte-identical to v1).
		writePresence(&b, s.chat.PresenceList(tenant), now)
		// structuredContent (MCP 2025-06-18): one entry per message (across areas),
		// mirroring the text — image/task sub-objects where the text shows them. The
		// inbox image line omits the byte size, so the image object does too.
		cursor := inboxSince
		outMsgs := []map[string]any{}
		if len(msgs) == 0 {
			fmt.Fprintf(&b, "inbox empty for %q (since #%d)", target, inboxSince)
		} else {
			fmt.Fprintf(&b, "%d message(s) for %q:\n", len(msgs), target)
			for _, m := range msgs {
				tgt := m.Target
				if tgt == "" {
					tgt = "all"
				}
				sm := map[string]any{"seq": m.Seq, "area": m.Area, "from": m.From, "target": m.Target, "text": m.Text}
				if m.BlobID != "" {
					fmt.Fprintf(&b, "#%d [image] %s→%s @%s: %s [%s %dx%d · get_image(%d)]\n", m.Seq, m.From, tgt, m.Area, m.Text, m.Mime, m.W, m.H, m.Seq)
					sm["kind"] = "image"
					sm["image"] = map[string]any{"blob": m.BlobID, "mime": m.Mime, "w": m.W, "h": m.H}
				} else {
					fmt.Fprintf(&b, "#%d [%s] %s→%s @%s: %s", m.Seq, m.Kind, m.From, tgt, m.Area, m.Text)
					sm["kind"] = m.Kind
					if ts, ok := s.chat.TaskOf(tenant, m.Seq); ok {
						b.WriteString(taskSuffix(ts))
						sm["task"] = taskStruct(ts)
					}
					b.WriteByte('\n')
				}
				outMsgs = append(outMsgs, sm)
				if m.Seq > cursor {
					cursor = m.Seq
				}
			}
			fmt.Fprintf(&b, "(cursor: #%d)", cursor)
		}
		return s.toolStruct(req.ID, b.String(), map[string]any{
			"target":   target,
			"cursor":   cursor,
			"count":    len(msgs),
			"messages": outMsgs,
		})
	case "claim":
		seq := uintArg(p.Args, "seq")
		by, _ := strArg(p.Args, "by")
		if seq == 0 || by == "" {
			return s.toolErr(req.ID, "provide 'seq' and 'by' to claim")
		}
		ts, ok, err := s.chat.Claim(tenant, seq, by, time.Now())
		if err != nil {
			return s.toolErr(req.ID, "claim failed: "+err.Error())
		}
		if !ok {
			// DENIED is a normal outcome (someone else holds a live lease), not a
			// tool error — report who holds it and until when.
			s.moko.Info("claim", map[string]string{"tenant": fmt.Sprint(tenant), "seq": fmt.Sprint(seq), "by": by, "ok": "false", "holder": ts.Holder})
			return s.toolStruct(req.ID,
				fmt.Sprintf("DENIED: #%d held by %s until %s", seq, ts.Holder, time.Unix(0, ts.LeaseUntil).Format("15:04")),
				map[string]any{"seq": seq, "claimed": false, "holder": ts.Holder, "leaseUntil": ts.LeaseUntil, "state": ts.State, "deadline": ts.Deadline})
		}
		s.moko.Info("claim", map[string]string{"tenant": fmt.Sprint(tenant), "seq": fmt.Sprint(seq), "by": by, "ok": "true"})
		return s.toolStruct(req.ID,
			fmt.Sprintf("claimed #%d by %s, lease until %s", seq, by, time.Unix(0, ts.LeaseUntil).Format("15:04")),
			map[string]any{"seq": seq, "claimed": true, "holder": ts.Holder, "leaseUntil": ts.LeaseUntil, "state": ts.State, "deadline": ts.Deadline})
	case "resolve":
		seq := uintArg(p.Args, "seq")
		by, _ := strArg(p.Args, "by")
		if seq == 0 || by == "" {
			return s.toolErr(req.ID, "provide 'seq' and 'by' to resolve")
		}
		state, _ := strArg(p.Args, "state")
		if state == "" {
			state = "done"
		}
		if err := s.chat.Resolve(tenant, seq, by, state, time.Now()); err != nil {
			return s.toolErr(req.ID, "resolve failed: "+err.Error())
		}
		s.moko.Info("resolve", map[string]string{"tenant": fmt.Sprint(tenant), "seq": fmt.Sprint(seq), "by": by, "state": state})
		return s.toolStruct(req.ID,
			fmt.Sprintf("resolved #%d as %s", seq, state),
			map[string]any{"seq": seq, "resolved": true, "state": state, "by": by})
	case "heartbeat":
		from, _ := strArg(p.Args, "from")
		status, _ := strArg(p.Args, "status")
		if from == "" || status == "" {
			return s.toolErr(req.ID, "provide 'from' and 'status' to heartbeat")
		}
		area, _ := strArg(p.Args, "area")
		if err := s.chat.Heartbeat(tenant, from, area, status, time.Now()); err != nil {
			return s.toolErr(req.ID, "heartbeat failed: "+err.Error())
		}
		s.moko.Info("heartbeat", map[string]string{"tenant": fmt.Sprint(tenant), "from": from, "area": area})
		return s.toolText(req.ID, fmt.Sprintf("presence updated for %s", from))
	case "post_image":
		area, _ := strArg(p.Args, "area")
		from, _ := strArg(p.Args, "from")
		mime, _ := strArg(p.Args, "mime")
		dataB64, _ := strArg(p.Args, "data")
		if area == "" || from == "" || mime == "" || dataB64 == "" {
			return s.toolErr(req.ID, "provide 'area', 'from', 'mime' and base64 'data'")
		}
		if !strings.HasPrefix(mime, "image/") {
			return s.toolErr(req.ID, "mime must be image/* (got "+mime+")")
		}
		// Fix 4: cheap encoded-string length pre-check — reject before materialising
		// the full decoded buffer. base64 expands at ~4/3; for a 15 MiB raw cap the
		// encoded string cannot legitimately exceed ceil(15<<20/3)*4 plus small slack.
		if len(dataB64) > ((15<<20)+2)/3*4+16 {
			return s.toolErr(req.ID, "image too large")
		}
		raw, err := base64.StdEncoding.DecodeString(dataB64)
		if err != nil {
			return s.toolErr(req.ID, "data is not valid base64: "+err.Error())
		}
		if len(raw) > 15<<20 {
			return s.toolErr(req.ID, fmt.Sprintf("image too large: %d bytes (max %d)", len(raw), 15<<20))
		}
		w, h := int(uintArg(p.Args, "w")), int(uintArg(p.Args, "h"))
		if w == 0 && h == 0 {
			w, h = imageDims(raw)
		}
		caption, _ := strArg(p.Args, "caption")
		target, _ := strArg(p.Args, "target")
		// Fix 1: Put+PostImage inside one critical section so blob_gc cannot sweep a
		// just-Put blob before its comms ref is added (no dangling-reference window).
		s.mu.Lock()
		sha, err := s.blobs.Put(raw)
		if err != nil {
			s.mu.Unlock()
			return s.toolErr(req.ID, "blob store failed: "+err.Error())
		}
		seq, err := s.chat.PostImage(tenant, comms.MessagePayload{
			Area: area, From: from, Mime: mime, BlobID: sha,
			W: w, H: h, Size: len(raw), Text: caption, Target: target,
		})
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "post_image failed: "+err.Error())
		}
		s.moko.Info("post_image", map[string]string{"tenant": fmt.Sprint(tenant), "area": area, "seq": fmt.Sprint(seq), "bytes": fmt.Sprint(len(raw))})
		return s.toolText(req.ID, fmt.Sprintf("posted image #%d in %s (%s, %dx%d, %d bytes) — fetch with get_image(%d)", seq, area, mime, w, h, len(raw), seq))
	case "get_image":
		seq := uintArg(p.Args, "seq")
		if seq == 0 {
			return s.toolErr(req.ID, "provide the 'seq' of the image message")
		}
		m, ok := s.chat.GetBySeq(tenant, seq)
		if !ok {
			return s.toolErr(req.ID, fmt.Sprintf("message #%d not found for this tenant", seq))
		}
		if m.BlobID == "" {
			return s.toolErr(req.ID, fmt.Sprintf("message #%d is not an image", seq))
		}
		raw, err := s.blobs.Get(m.BlobID)
		if err != nil {
			return s.toolErr(req.ID, "image bytes unavailable (blob "+m.BlobID+"): "+err.Error())
		}
		return s.toolImage(req.ID, base64.StdEncoding.EncodeToString(raw), m.Mime, m.Text)
	case "pin_image", "unpin_image":
		seq := uintArg(p.Args, "seq")
		if seq == 0 {
			return s.toolErr(req.ID, "provide the 'seq' of the image")
		}
		m, ok := s.chat.GetBySeq(tenant, seq)
		if !ok || m.BlobID == "" {
			return s.toolErr(req.ID, fmt.Sprintf("image #%d not found for this tenant", seq))
		}
		on := p.Name == "pin_image"
		if err := s.chat.Pin(tenant, m.BlobID, on); err != nil {
			return s.toolErr(req.ID, "pin failed: "+err.Error())
		}
		verb := "pinned"
		if !on {
			verb = "unpinned"
		}
		return s.toolText(req.ID, fmt.Sprintf("%s image #%d", verb, seq))
	case "blob_gc":
		// Fix 3: gate to owner tenant only — blob GC is an orchestrator operation.
		if tenant != s.tenant {
			return s.toolErr(req.ID, "blob_gc is restricted to the owner tenant")
		}
		// Fix 1: snapshot+sweep under s.mu so no concurrent post_image can race
		// (Put happens inside s.mu too, so LiveBlobIDs and Sweep are coherent).
		s.mu.Lock()
		n, err := s.blobs.Sweep(s.chat.LiveBlobIDs())
		s.mu.Unlock()
		if err != nil {
			return s.toolErr(req.ID, "blob_gc failed: "+err.Error())
		}
		s.moko.Info("blob_gc", map[string]string{"tenant": fmt.Sprint(tenant), "deleted": fmt.Sprint(n)})
		return s.toolText(req.ID, fmt.Sprintf("deleted %d orphan blob(s) (journal retained)", n))
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
		events := uint64(s.store.ReadNextSeq() - 1)
		return s.toolStruct(req.ID,
			fmt.Sprintf("journal holds %d events", events),
			map[string]any{"events": events})
	default:
		return s.toolErr(req.ID, "unknown tool: "+p.Name)
	}
}

// writePresence prepends the lifecycle-v2 presence section to b — one line per
// live agent: "~ <agent> [<area>]: <status> (<age>)". The bracketed area is
// omitted when empty. Nothing is written when there are no live slots, so a
// channel with no presence renders byte-identical to v1.
func writePresence(b *strings.Builder, slots []comms.Presence, now time.Time) {
	for _, p := range slots {
		b.WriteString("~ ")
		b.WriteString(p.Agent)
		if p.Area != "" {
			b.WriteString(" [")
			b.WriteString(p.Area)
			b.WriteByte(']')
		}
		b.WriteString(": ")
		b.WriteString(p.Status)
		fmt.Fprintf(b, " (%s)\n", compactAge(now.UnixNano()-p.TS))
	}
}

// taskSuffix renders a task message's live-state suffix, e.g.
//
//	"  ⟨claimed by backend-2 · lease 12:40 · due 12:55⟩"
//
// It is appended to a task's message line by read/inbox. A zero/empty state
// yields "" so non-task messages (TaskOf ok=false, or a never-seeded seq) render
// byte-identical to v1. claimed/overdue show holder + lease; pending/done/cancel
// show the bare state; a deadline is appended for any non-terminal task.
func taskSuffix(ts comms.TaskState) string {
	if ts.State == "" {
		return ""
	}
	var parts []string
	switch ts.State {
	case "claimed", "overdue":
		if ts.Holder != "" {
			parts = append(parts, ts.State+" by "+ts.Holder)
		} else {
			parts = append(parts, ts.State)
		}
		if ts.LeaseUntil > 0 {
			parts = append(parts, "lease "+time.Unix(0, ts.LeaseUntil).Format("15:04"))
		}
	default: // pending, done, cancel
		parts = append(parts, ts.State)
	}
	if ts.Deadline > 0 && ts.State != "done" && ts.State != "cancel" {
		parts = append(parts, "due "+time.Unix(0, ts.Deadline).Format("15:04"))
	}
	return "  ⟨" + strings.Join(parts, " · ") + "⟩"
}

// presenceStruct builds the structuredContent presence array from the same live
// slots writePresence renders — {agent, status, ageSec} plus area when set. ageSec
// is the whole-seconds age (clamped at 0 for clock skew) of compactAge's input, so
// the structured value tracks the human "(<age>)" the text shows. Never nil.
func presenceStruct(slots []comms.Presence, now time.Time) []map[string]any {
	out := []map[string]any{}
	for _, p := range slots {
		age := (now.UnixNano() - p.TS) / int64(time.Second)
		if age < 0 {
			age = 0
		}
		e := map[string]any{"agent": p.Agent, "status": p.Status, "ageSec": age}
		if p.Area != "" {
			e["area"] = p.Area
		}
		out = append(out, e)
	}
	return out
}

// taskStruct builds the structuredContent task object for a message whose text
// carries a ⟨task⟩ suffix (TaskOf ok). It mirrors taskSuffix's data: state always;
// holder/leaseUntil/deadline only when present (holder set, lease/deadline > 0).
func taskStruct(ts comms.TaskState) map[string]any {
	m := map[string]any{"state": ts.State}
	if ts.Holder != "" {
		m["holder"] = ts.Holder
	}
	if ts.LeaseUntil > 0 {
		m["leaseUntil"] = ts.LeaseUntil
	}
	if ts.Deadline > 0 {
		m["deadline"] = ts.Deadline
	}
	return m
}

// recallStruct builds the structuredContent for recall — {query, count, memories}
// — mirroring formatRecall: each memory carries id/distance/text, and tags only
// when non-empty (the text shows tags only then). distance is the squared-L2 score.
func recallStruct(query string, hits []memory.Memory) map[string]any {
	mems := []map[string]any{}
	for _, h := range hits {
		m := map[string]any{"id": h.ID, "distance": float64(h.Score), "text": h.Text}
		if len(h.Tags) > 0 {
			m["tags"] = h.Tags
		}
		mems = append(mems, m)
	}
	return map[string]any{"query": query, "count": len(hits), "memories": mems}
}

// compactAge renders a nanosecond age as a short human string (0s, 45s, 12m, 3h).
// A negative age (clock skew) clamps to 0s.
func compactAge(ns int64) string {
	d := time.Duration(ns)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
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

// parseDur parses the lifecycle-v2 duration grammar: a bare integer is seconds
// ("5" = 5s); otherwise the standard suffixed form ("90s", "10m", "2h", "1h30m").
// Junk is an error so an invalid ttl/deadline is never silently treated as 0.
func parseDur(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use 90s, 10m, 2h, or integer seconds)", s)
	}
	return d, nil
}

// durArg reads the ttl/deadline arg (a relative duration) and returns it as an
// ABSOLUTE unix-nanos deadline from now. Absent or "" → (0, false, nil) meaning
// "no expiry". A duration string ("90s"/"10m"/"2h") or a bare/JSON integer of
// seconds → (now+dur, true, nil). Anything unparseable → (0, false, error) so a
// bad value surfaces as a tool error instead of a silent never-expires.
func durArg(args map[string]any, key string, now time.Time) (int64, bool, error) {
	raw, present := args[key]
	if !present {
		return 0, false, nil
	}
	var d time.Duration
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false, nil
		}
		var err error
		if d, err = parseDur(s); err != nil {
			return 0, false, err
		}
	case float64:
		if v <= 0 {
			return 0, false, nil
		}
		d = time.Duration(v) * time.Second
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("invalid duration for %q: %v", key, err)
		}
		if n <= 0 {
			return 0, false, nil
		}
		d = time.Duration(n) * time.Second
	default:
		return 0, false, fmt.Errorf("%q must be a duration string (90s|10m|2h) or integer seconds", key)
	}
	return now.Add(d).UnixNano(), true, nil
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

// toolStruct returns a tool result carrying BOTH the human-readable text content
// block (byte-identical to what toolText would emit, so 2024-11-05 clients see
// no change) AND a structuredContent value (MCP 2025-06-18). Ship this only for
// tools that declare an outputSchema; structured MUST validate against it.
func (s *server) toolStruct(id json.RawMessage, text string, structured any) rpcResp {
	return s.ok(id, map[string]any{
		"content":           []map[string]any{{"type": "text", "text": text}},
		"structuredContent": structured,
	})
}

// negotiateVersion picks the protocolVersion to echo in initialize. If the
// client requested a version we support, echo exactly that — a 2024-11-05 client
// MUST keep getting 2024-11-05 so nothing changes for it. Anything else
// (unspecified, unknown, or newer than we know) negotiates down to the newest
// version we support. protocolVersion (the const) remains the compatibility
// floor/default the server is built around.
func negotiateVersion(requested string) string {
	switch requested {
	case "2025-06-18", protocolVersion:
		return requested
	default:
		return "2025-06-18"
	}
}
func (s *server) toolErr(id json.RawMessage, text string) rpcResp {
	return s.ok(id, map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true})
}

// toolImage returns an MCP image content block (base64 + mimeType), optionally
// preceded by a text caption — the protocol-native way to hand an image to any
// MCP client, local or remote.
func (s *server) toolImage(id json.RawMessage, b64, mime, caption string) rpcResp {
	content := []map[string]any{}
	if caption != "" {
		content = append(content, map[string]any{"type": "text", "text": caption})
	}
	content = append(content, map[string]any{"type": "image", "data": b64, "mimeType": mime})
	return s.ok(id, map[string]any{"content": content})
}

// imageDims best-effort decodes width/height using only stdlib decoders
// (png/jpeg/gif). Unknown formats (e.g. webp) return 0,0 — no extra deps.
func imageDims(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}
