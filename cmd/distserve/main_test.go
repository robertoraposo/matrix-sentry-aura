package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDistHandlerAllowlist(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "install.sh"), []byte("#!/bin/sh\necho hi\n"), 0644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("TOPSECRET"), 0644)
	// a symlink that, if followed, would leak an out-of-allowlist file
	os.Symlink("/etc/hostname", filepath.Join(dir, "sentry-record.linux-amd64"))

	h := distHandler(dir)

	cases := []struct {
		path string
		want int
	}{
		{"/install.sh", http.StatusOK},          // allowlisted regular file
		{"/secret.txt", http.StatusNotFound},    // not on the allowlist
		{"/sentry-record.linux-amd64", http.StatusNotFound}, // allowlisted name but a symlink -> rejected
		{"/../etc/passwd", http.StatusNotFound}, // traversal
		{"/", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.path, rec.Code, c.want)
		}
	}
}
