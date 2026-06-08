// Command distserve is a tiny, no-auth static file server used to distribute the
// sentry-record hook binary and a one-shot installer across the homelab network
// (curl http://HOST:PORT/install.sh | sh). It serves ONLY an explicit allowlist
// of filenames and refuses anything else (including symlinks and path traversal),
// so the fixed serving directory cannot be used to read arbitrary files.
//
// No-auth + 0.0.0.0 are intentional (peers on the private network must pull
// without credentials); the served files carry no secrets, and the installer
// verifies the binary's pinned sha256 before executing it.
//
//	go build -o distserve ./cmd/distserve
//	./distserve -addr 0.0.0.0:8810 -dir /root/sentry-dist
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// allowed is the exact set of request paths distserve will serve.
var allowed = map[string]bool{
	"/install.sh":                 true,
	"/sentry-record.linux-amd64":  true,
	"/sentry-record.linux-arm64":  true,
}

// distHandler serves only allowlisted, regular (non-symlink) files from dir.
func distHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		// filepath.Base strips any traversal; the allowlist already pins the name.
		p := filepath.Join(dir, filepath.Base(r.URL.Path))
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() { // reject symlinks / specials
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, p)
	}
}

func main() {
	addr := flag.String("addr", "0.0.0.0:8810", "listen address")
	dir := flag.String("dir", ".", "directory to serve")
	flag.Parse()
	log.Printf("distserve serving %s on %s (allowlist: install.sh + sentry-record.linux-*)", *dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, distHandler(*dir)))
}
