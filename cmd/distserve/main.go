// Command distserve is a tiny, no-auth static file server used to distribute the
// sentry-record hook binary and a one-shot installer across the homelab network
// (curl http://HOST:PORT/install.sh | sh). It serves exactly one directory and
// nothing else — keep only the installer + binaries in that dir.
//
//	go build -o distserve ./cmd/distserve
//	./distserve -addr 0.0.0.0:8810 -dir /root/sentry-dist
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:8810", "listen address")
	dir := flag.String("dir", ".", "directory to serve")
	flag.Parse()
	log.Printf("distserve serving %s on %s", *dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, http.FileServer(http.Dir(*dir))))
}
