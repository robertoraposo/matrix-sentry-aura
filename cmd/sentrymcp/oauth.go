package main

// Native OAuth 2.1 Authorization Server for the MCP endpoint, so claude.ai's
// custom connector (which requires OAuth + discovery, not a static bearer) can
// connect and sync across all of a user's Claude clients. Pure Go, zero deps.
//
// Single-user design: the resource owner authorizes a client by entering the
// server's approval passphrase (= SENTRY_MCP_TOKEN) on the /authorize consent
// page. Tokens are stateless HS256 JWTs signed with a key derived from that
// secret, so they survive restarts without server-side session storage; only
// short-lived authorization codes are kept in memory. PKCE (S256) is required.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	accessTokenTTL  = 30 * 24 * time.Hour
	refreshTokenTTL = 365 * 24 * time.Hour
	authCodeTTL     = 5 * time.Minute
)

type authCode struct {
	clientID      string
	redirectURI   string
	codeChallenge string // PKCE S256 challenge
	scope         string
	expiry        time.Time
}

type oauthProvider struct {
	issuer        string // public base URL, e.g. https://mcp.blazesphere.net
	approveSecret string // passphrase the owner enters to consent
	signKey       []byte // HMAC key derived from approveSecret
	mu            sync.Mutex
	codes         map[string]authCode
}

func newOAuth(issuer, approveSecret string) *oauthProvider {
	k := sha256.Sum256([]byte("matrix-sentry-oauth-v1|" + approveSecret))
	return &oauthProvider{
		issuer:        strings.TrimRight(issuer, "/"),
		approveSecret: approveSecret,
		signKey:       k[:],
		codes:         map[string]authCode{},
	}
}

// --- crypto helpers ---

func randToken(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// allowedRedirect is the redirect_uri allowlist that prevents authorization-code
// theft: without it, an attacker who phishes the owner into approving a request
// with their own PKCE pair could redirect the code to a site they control and
// exchange it (PKCE alone doesn't help — the attacker generated the pair). We
// allow only Anthropic's HTTPS callback hosts and loopback (RFC 8252) for local
// MCP clients. Exact host match, no substring/suffix tricks, no userinfo.
func allowedRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	host := u.Hostname()
	if u.Scheme == "https" && (host == "claude.ai" || host == "claude.com") {
		return true
	}
	if (u.Scheme == "http" || u.Scheme == "https") && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return true
	}
	return false
}

// verifyPKCE checks that S256(verifier) equals the stored challenge.
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

type jwtClaims struct {
	Sub  string `json:"sub"`
	Iss  string `json:"iss"`
	Kind string `json:"kind"` // "access" | "refresh"
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}

func (o *oauthProvider) signToken(subject, kind string, ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	now := time.Now()
	cl := jwtClaims{Sub: subject, Iss: o.issuer, Kind: kind, Iat: now.Unix(), Exp: now.Add(ttl).Unix()}
	cb, _ := json.Marshal(cl)
	payload := base64.RawURLEncoding.EncodeToString(cb)
	signing := header + "." + payload
	mac := hmac.New(sha256.New, o.signKey)
	mac.Write([]byte(signing))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing + "." + sig
}

func (o *oauthProvider) verifyToken(tok, kind string) (string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", false
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, o.signKey)
	mac.Write([]byte(signing))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) != 1 {
		return "", false
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var cl jwtClaims
	if err := json.Unmarshal(cb, &cl); err != nil {
		return "", false
	}
	if cl.Kind != kind || cl.Iss != o.issuer || time.Now().Unix() >= cl.Exp {
		return "", false
	}
	return cl.Sub, true
}

// --- authorization codes (in-memory, short-lived, single-use) ---

func (o *oauthProvider) issueCode(c authCode) string {
	if c.expiry.IsZero() {
		c.expiry = time.Now().Add(authCodeTTL)
	}
	code := randToken(32)
	o.mu.Lock()
	o.codes[code] = c
	o.mu.Unlock()
	return code
}

func (o *oauthProvider) consumeCode(code string) (authCode, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.codes[code]
	if !ok {
		return authCode{}, false
	}
	delete(o.codes, code) // single use
	if time.Now().After(c.expiry) {
		return authCode{}, false
	}
	return c, true
}

// --- CORS (claude.ai is a browser origin) ---

// cors sets permissive CORS headers and answers preflight. Returns true if it
// fully handled the request (an OPTIONS preflight), false to continue.
func (o *oauthProvider) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, Mcp-Protocol-Version")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// --- discovery ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (o *oauthProvider) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	if o.cors(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              o.issuer,
		"authorization_servers": []string{o.issuer},
	})
}

func (o *oauthProvider) handleAuthServerMeta(w http.ResponseWriter, r *http.Request) {
	if o.cors(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                o.issuer,
		"authorization_endpoint":                o.issuer + "/authorize",
		"token_endpoint":                        o.issuer + "/token",
		"registration_endpoint":                 o.issuer + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"mcp"},
	})
}

// --- dynamic client registration (RFC 7591) ---

func (o *oauthProvider) handleRegister(w http.ResponseWriter, r *http.Request) {
	if o.cors(w, r) {
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	body, _ := readLimited(r)
	_ = json.Unmarshal(body, &req)
	// Public client + PKCE: we don't issue a secret and accept the client's
	// redirect URIs (the PKCE binding + consent passphrase are what secure the
	// flow for this single-user server).
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  "ms-" + randToken(12),
		"redirect_uris":              req.RedirectURIs,
		"client_name":                req.ClientName,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// --- authorize (consent) ---

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Matrix Sentry — Authorize</title>
<style>body{font-family:system-ui,sans-serif;max-width:30rem;margin:4rem auto;padding:0 1rem}
input,button{font-size:1rem;padding:.6rem;width:100%;box-sizing:border-box;margin:.3rem 0}
button{background:#111;color:#fff;border:0;border-radius:.4rem;cursor:pointer}
.c{color:#666;font-size:.9rem}</style></head>
<body><h2>Authorize Claude to access Matrix Sentry</h2>
<p class="c">A client wants to connect to your memory server and will receive the
authorization at:<br><b>{{.RedirectHost}}</b><br>Only continue if you recognize it.
Enter your approval passphrase to allow it.</p>
<form method="POST" action="/authorize">
<input type="password" name="passphrase" placeholder="approval passphrase" autofocus required>
<input type="hidden" name="response_type" value="{{.ResponseType}}">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
<input type="hidden" name="state" value="{{.State}}">
<input type="hidden" name="scope" value="{{.Scope}}">
<button type="submit">Authorize</button>
</form></body></html>`))

func (o *oauthProvider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if o.cors(w, r) {
		return
	}
	r.ParseForm()
	get := func(k string) string { return r.Form.Get(k) }

	// Reject a bad redirect_uri up front (both GET and POST) so a malicious one
	// never reaches the consent form or a code.
	if ru := get("redirect_uri"); ru == "" || !allowedRedirect(ru) {
		http.Error(w, "invalid_request: redirect_uri not allowed", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet {
		rh := ""
		if u, err := url.Parse(get("redirect_uri")); err == nil {
			rh = u.Hostname()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		consentTmpl.Execute(w, map[string]string{
			"ResponseType":        get("response_type"),
			"ClientID":            get("client_id"),
			"RedirectURI":         get("redirect_uri"),
			"RedirectHost":        rh,
			"CodeChallenge":       get("code_challenge"),
			"CodeChallengeMethod": get("code_challenge_method"),
			"State":               get("state"),
			"Scope":               get("scope"),
		})
		return
	}

	// POST: validate consent passphrase, then issue an authorization code.
	redirectURI := get("redirect_uri")
	challenge := get("code_challenge")
	if challenge == "" || get("code_challenge_method") != "S256" {
		http.Error(w, "invalid_request: PKCE S256 required", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(get("passphrase")), []byte(o.approveSecret)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<p>Incorrect passphrase. <a href="javascript:history.back()">Try again</a>.</p>`)
		return
	}
	code := o.issueCode(authCode{
		clientID: get("client_id"), redirectURI: redirectURI,
		codeChallenge: challenge, scope: get("scope"),
	})
	// Build the redirect with proper URL encoding, preserving any existing query.
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid_request: bad redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("code", code)
	if st := get("state"); st != "" {
		q.Set("state", st)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// --- token ---

func (o *oauthProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	if o.cors(w, r) {
		return
	}
	r.ParseForm()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		c, ok := o.consumeCode(r.Form.Get("code"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		if c.redirectURI != "" && r.Form.Get("redirect_uri") != c.redirectURI {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "redirect_uri mismatch"})
			return
		}
		// Bind the code to the client it was issued to (RFC 6749 §4.1.3).
		if c.clientID != "" && r.Form.Get("client_id") != c.clientID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "client_id mismatch"})
			return
		}
		if !verifyPKCE(r.Form.Get("code_verifier"), c.codeChallenge) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "PKCE verification failed"})
			return
		}
		o.emitTokens(w, "owner")
	case "refresh_token":
		sub, ok := o.verifyToken(r.Form.Get("refresh_token"), "refresh")
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		o.emitTokens(w, sub)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (o *oauthProvider) emitTokens(w http.ResponseWriter, subject string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  o.signToken(subject, "access", accessTokenTTL),
		"refresh_token": o.signToken(subject, "refresh", refreshTokenTTL),
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"scope":         "mcp",
	})
}

// wwwAuthenticate is the 401 challenge that points MCP clients at discovery.
func (o *oauthProvider) wwwAuthenticate() string {
	return fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, o.issuer)
}

func readLimited(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil || len(buf) > 1<<20 {
			return buf, nil
		}
	}
}
