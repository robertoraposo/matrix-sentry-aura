// Command sentryadmin serves the Matrix Sentry admin dashboard (the Three.js
// "Vector Galaxy" design) as a self-contained static site. The assets are
// embedded in the binary (go:embed) so deployment is a single file, matching
// the project's ship-one-binary convention.
//
// The dashboard is a Claude Design standalone artifact: it fetches React,
// ReactDOM, Babel and three.js from CDNs and reads its own HTML via
// fetch(location.href), so it only needs to be served over HTTP. v1 runs on
// the synthetic corpus baked into corpus.js; wiring it to live MCP data is a
// follow-up.
//
// Optional HTTP basic auth (SENTRY_ADMIN_USER + SENTRY_ADMIN_PASS) gates the
// whole site so the dashboard can be exposed publicly without leaving it open.
// With neither set, the site is served unauthenticated (local/dev).
package main

import (
	"crypto/subtle"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"
)

//go:embed assets
var assetsFS embed.FS

func main() {
	httpAddr := flag.String("http", envOr("SENTRY_ADMIN_HTTP", "0.0.0.0:8810"), "listen address")
	flag.Parse()

	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sentryadmin: embed: %v\n", err)
		os.Exit(1)
	}

	fileServer := http.FileServer(http.FS(sub))
	user, pass := os.Getenv("SENTRY_ADMIN_USER"), os.Getenv("SENTRY_ADMIN_PASS")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mcpURL := os.Getenv("SENTRY_ADMIN_MCP_URL")
	mcpToken := os.Getenv("SENTRY_ADMIN_MCP_TOKEN")
	if mcpURL != "" {
		api := newAPIServer(mcpURL, mcpToken)
		mux.Handle("/api/galaxy", basicAuth(user, pass, http.HandlerFunc(api.handleGalaxy)))
		mux.Handle("/api/comms", basicAuth(user, pass, http.HandlerFunc(api.handleComms)))
		fmt.Fprintf(os.Stderr, "sentryadmin: live data ON (mcp %s)\n", mcpURL)
	} else {
		fmt.Fprintln(os.Stderr, "sentryadmin: live data OFF (set SENTRY_ADMIN_MCP_URL) — serving mock")
	}
	mux.Handle("/", basicAuth(user, pass, fileServer))

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	authState := "OPEN (no SENTRY_ADMIN_USER/PASS set)"
	if user != "" || pass != "" {
		authState = "basic-auth required"
	}
	fmt.Fprintf(os.Stderr, "sentryadmin: serving dashboard on %s — %s\n", *httpAddr, authState)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "sentryadmin: %v\n", err)
		os.Exit(1)
	}
}

// basicAuth wraps h with HTTP basic auth when a username or password is set.
// The comparison is constant-time to avoid leaking the credential via timing.
// When both are empty, h is returned unwrapped (open mode).
func basicAuth(user, pass string, h http.Handler) http.Handler {
	if user == "" && pass == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Matrix Sentry Admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
