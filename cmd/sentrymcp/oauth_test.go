package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testIssuer = "https://mcp.example.com"
const testSecret = "owner-passphrase-123"

func newTestOAuth() *oauthProvider { return newOAuth(testIssuer, testSecret) }

func TestPKCEVerifyS256(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !verifyPKCE(verifier, challenge) {
		t.Fatal("correct verifier rejected")
	}
	if verifyPKCE("wrong", challenge) {
		t.Fatal("wrong verifier accepted")
	}
}

func TestJWTRoundTripTamperExpiry(t *testing.T) {
	o := newTestOAuth()
	tok := o.signToken("user", "access", time.Hour)
	if sub, ok := o.verifyToken(tok, "access"); !ok || sub != "user" {
		t.Fatalf("valid token rejected: sub=%q ok=%v", sub, ok)
	}
	if _, ok := o.verifyToken(tok, "refresh"); ok {
		t.Fatal("access token accepted as refresh kind")
	}
	if _, ok := o.verifyToken(tok+"x", "access"); ok {
		t.Fatal("tampered token accepted")
	}
	expired := o.signToken("user", "access", -time.Minute)
	if _, ok := o.verifyToken(expired, "access"); ok {
		t.Fatal("expired token accepted")
	}
	// a different provider (different key) must reject our token
	other := newOAuth(testIssuer, "different-secret")
	if _, ok := other.verifyToken(tok, "access"); ok {
		t.Fatal("token verified under a different signing key")
	}
}

func TestAuthCodeSingleUseAndExpiry(t *testing.T) {
	o := newTestOAuth()
	code := o.issueCode(authCode{clientID: "c", redirectURI: "https://claude.ai/cb", codeChallenge: "ch"})
	got, ok := o.consumeCode(code)
	if !ok || got.clientID != "c" {
		t.Fatalf("first consume failed: %v %+v", ok, got)
	}
	if _, ok := o.consumeCode(code); ok {
		t.Fatal("code consumed twice (must be single-use)")
	}
	// expired code
	o.mu.Lock()
	o.codes["old"] = authCode{clientID: "c", expiry: time.Now().Add(-time.Minute)}
	o.mu.Unlock()
	if _, ok := o.consumeCode("old"); ok {
		t.Fatal("expired code accepted")
	}
}

func TestProtectedResourceMetadata(t *testing.T) {
	o := newTestOAuth()
	rec := httptest.NewRecorder()
	o.handleProtectedResource(rec, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil))
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["resource"] != testIssuer {
		t.Fatalf("resource = %v", m["resource"])
	}
	as, _ := m["authorization_servers"].([]any)
	if len(as) != 1 || as[0] != testIssuer {
		t.Fatalf("authorization_servers = %v", m["authorization_servers"])
	}
}

func TestAuthServerMetadataHasRequiredFields(t *testing.T) {
	o := newTestOAuth()
	rec := httptest.NewRecorder()
	o.handleAuthServerMeta(rec, httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in AS metadata", k)
		}
	}
	methods, _ := m["code_challenge_methods_supported"].([]any)
	found := false
	for _, x := range methods {
		if x == "S256" {
			found = true
		}
	}
	if !found {
		t.Fatal("S256 not advertised")
	}
}

func TestRegisterReturnsClientID(t *testing.T) {
	o := newTestOAuth()
	body := `{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"client_name":"Claude"}`
	req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	o.handleRegister(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status %d, want 201", rec.Code)
	}
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m["client_id"] == nil || m["client_id"] == "" {
		t.Fatalf("no client_id: %v", m)
	}
	ru, _ := m["redirect_uris"].([]any)
	if len(ru) != 1 || ru[0] != "https://claude.ai/api/mcp/auth_callback" {
		t.Fatalf("redirect_uris not echoed: %v", m["redirect_uris"])
	}
}

func TestAuthorizeGETShowsConsentForm(t *testing.T) {
	o := newTestOAuth()
	u := "/authorize?response_type=code&client_id=c&redirect_uri=https://claude.ai/api/mcp/auth_callback&code_challenge=ch&code_challenge_method=S256&state=xyz"
	rec := httptest.NewRecorder()
	o.handleAuthorize(rec, httptest.NewRequest("GET", u, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize GET status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<form") {
		t.Fatal("consent form not rendered")
	}
}

// full flow: authorize POST (correct passphrase) -> code -> token -> usable access token
func TestFullAuthorizationCodeFlow(t *testing.T) {
	o := newTestOAuth()
	verifier := "abc123abc123abc123abc123abc123abc123abc123ZZ"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	redirect := "https://claude.ai/api/mcp/auth_callback"

	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {"c1"},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"st8"},
		"passphrase":            {testSecret},
	}
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	o.handleAuthorize(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize POST status %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("state") != "st8" {
		t.Fatalf("state not echoed: %s", rec.Header().Get("Location"))
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatal("no code in redirect")
	}

	// exchange
	tform := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {"c1"},
		"code_verifier": {verifier},
	}
	treq := httptest.NewRequest("POST", "/token", strings.NewReader(tform.Encode()))
	treq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	trec := httptest.NewRecorder()
	o.handleToken(trec, treq)
	if trec.Code != http.StatusOK {
		t.Fatalf("token status %d: %s", trec.Code, trec.Body.String())
	}
	var tr map[string]any
	json.Unmarshal(trec.Body.Bytes(), &tr)
	at, _ := tr["access_token"].(string)
	if at == "" || tr["token_type"] != "Bearer" {
		t.Fatalf("bad token response: %v", tr)
	}
	if _, ok := o.verifyToken(at, "access"); !ok {
		t.Fatal("issued access token does not verify")
	}
}

func TestAuthorizeRejectsBadPassphrase(t *testing.T) {
	o := newTestOAuth()
	form := url.Values{
		"response_type": {"code"}, "client_id": {"c"}, "redirect_uri": {"https://claude.ai/cb"},
		"code_challenge": {"ch"}, "state": {"s"}, "passphrase": {"WRONG"},
	}
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	o.handleAuthorize(rec, req)
	if rec.Code == http.StatusFound {
		t.Fatal("bad passphrase produced a redirect with a code")
	}
}

func TestTokenRejectsBadPKCE(t *testing.T) {
	o := newTestOAuth()
	code := o.issueCode(authCode{clientID: "c", redirectURI: "https://claude.ai/cb", codeChallenge: "challengevalue"})
	tform := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {"https://claude.ai/cb"}, "client_id": {"c"},
		"code_verifier": {"this-does-not-hash-to-the-challenge"},
	}
	req := httptest.NewRequest("POST", "/token", strings.NewReader(tform.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	o.handleToken(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("token issued despite PKCE mismatch")
	}
}

func TestCORSPreflightAllowsClaude(t *testing.T) {
	o := newTestOAuth()
	req := httptest.NewRequest("OPTIONS", "/token", nil)
	req.Header.Set("Origin", "https://claude.ai")
	rec := httptest.NewRecorder()
	handled := o.cors(rec, req)
	if !handled {
		t.Fatal("preflight not handled")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatal("no CORS allow-origin on preflight")
	}
}

func TestRefreshTokenGrant(t *testing.T) {
	o := newTestOAuth()
	rt := o.signToken("user", "refresh", time.Hour)
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	o.handleToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh grant status %d: %s", rec.Code, rec.Body.String())
	}
	var tr map[string]any
	json.Unmarshal(rec.Body.Bytes(), &tr)
	if at, _ := tr["access_token"].(string); at == "" {
		t.Fatalf("refresh did not return access_token: %v", tr)
	}
}

var _ = io.Discard

func TestMCPAuthAcceptsStaticAndOAuthAndChallenges(t *testing.T) {
	s := newTestServer(t)
	s.token = "static123"
	s.oauth = newOAuth(testIssuer, "static123")

	call := func(authz string) *httptest.ResponseRecorder {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		s.handleHTTP(rec, req)
		return rec
	}

	// no auth -> 401 with WWW-Authenticate pointing at discovery
	rec := call("")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth status %d, want 401", rec.Code)
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "oauth-protected-resource") {
		t.Fatalf("missing/invalid WWW-Authenticate: %q", wa)
	}

	// static bearer (Claude Code) -> ok
	if rec := call("Bearer static123"); rec.Code != http.StatusOK {
		t.Fatalf("static bearer status %d, want 200", rec.Code)
	}

	// oauth access token (claude.ai) -> ok
	at := s.oauth.signToken("owner", "access", time.Hour)
	if rec := call("Bearer " + at); rec.Code != http.StatusOK {
		t.Fatalf("oauth token status %d, want 200", rec.Code)
	}

	// garbage -> 401
	if rec := call("Bearer nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status %d, want 401", rec.Code)
	}
}

func TestAuthorizeRejectsUnregisteredRedirect(t *testing.T) {
	o := newTestOAuth()
	form := url.Values{
		"response_type": {"code"}, "client_id": {"c"},
		"redirect_uri":          {"https://evil.example.com/steal"},
		"code_challenge":        {"ch"}, "code_challenge_method": {"S256"},
		"state": {"s"}, "passphrase": {testSecret}, // correct passphrase!
	}
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	o.handleAuthorize(rec, req)
	if rec.Code == http.StatusFound {
		t.Fatalf("issued a code to an unregistered redirect_uri (auth-code theft): %s", rec.Header().Get("Location"))
	}
}

func TestAllowedRedirect(t *testing.T) {
	ok := []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://claude.com/api/mcp/auth_callback",
		"http://localhost:8976/callback",
		"http://127.0.0.1:53/cb",
	}
	bad := []string{
		"https://evil.com/cb",
		"https://claude.ai.evil.com/cb",
		"http://evil.com/cb",
		"https://notclaude.ai/cb",
		"ftp://localhost/x",
		"",
		"https://claude.ai@evil.com/cb",
	}
	for _, u := range ok {
		if !allowedRedirect(u) {
			t.Errorf("should allow %q", u)
		}
	}
	for _, u := range bad {
		if allowedRedirect(u) {
			t.Errorf("should reject %q", u)
		}
	}
}

func TestTokenBindsClientID(t *testing.T) {
	o := newTestOAuth()
	code := o.issueCode(authCode{clientID: "clientA", redirectURI: "https://claude.ai/cb", codeChallenge: "ch"})
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {"https://claude.ai/cb"}, "client_id": {"clientB"}, // mismatched
		"code_verifier": {"whatever"},
	}
	req := httptest.NewRequest("POST", "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	o.handleToken(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("token issued despite client_id mismatch (code not bound to client)")
	}
}

func TestRedirectEncodesStateAndCode(t *testing.T) {
	o := newTestOAuth()
	verifier := "abc123abc123abc123abc123abc123abc123abc123ZZ"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	form := url.Values{
		"response_type": {"code"}, "client_id": {"c1"},
		"redirect_uri":          {"https://claude.ai/api/mcp/auth_callback"},
		"code_challenge":        {challenge}, "code_challenge_method": {"S256"},
		"state":      {"a b&c=d"}, // needs encoding
		"passphrase": {testSecret},
	}
	req := httptest.NewRequest("POST", "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	o.handleAuthorize(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("redirect not a valid URL: %v", err)
	}
	if loc.Query().Get("state") != "a b&c=d" {
		t.Fatalf("state not round-tripped through encoding: %q", loc.Query().Get("state"))
	}
}
