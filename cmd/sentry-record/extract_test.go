package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// touch creates an empty regular file and returns its absolute path.
func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractFilePathTools(t *testing.T) {
	dir := t.TempDir()
	a := touch(t, dir, "a.go")

	for _, tool := range []string{"Read", "Edit", "Write", "MultiEdit"} {
		got := extractPaths(tool, map[string]any{"file_path": a}, nil, dir)
		if !reflect.DeepEqual(got, []string{a}) {
			t.Errorf("%s: got %v, want [%s]", tool, got, a)
		}
	}
}

func TestExtractNotebookPath(t *testing.T) {
	dir := t.TempDir()
	nb := touch(t, dir, "nb.ipynb")
	got := extractPaths("NotebookEdit", map[string]any{"notebook_path": nb}, nil, dir)
	if !reflect.DeepEqual(got, []string{nb}) {
		t.Errorf("got %v, want [%s]", got, nb)
	}
}

// A path that does not exist on disk is filtered out by the regular-file gate.
func TestExtractGateDropsNonexistent(t *testing.T) {
	dir := t.TempDir()
	got := extractPaths("Read", map[string]any{"file_path": filepath.Join(dir, "ghost.go")}, nil, dir)
	if len(got) != 0 {
		t.Errorf("expected nothing for nonexistent file, got %v", got)
	}
}

// A directory is not a regular file and must be dropped.
func TestExtractGateDropsDirectory(t *testing.T) {
	dir := t.TempDir()
	got := extractPaths("Read", map[string]any{"file_path": dir}, nil, dir)
	if len(got) != 0 {
		t.Errorf("expected nothing for a directory, got %v", got)
	}
}

// A relative path is resolved against cwd before the gate.
func TestExtractRelativePath(t *testing.T) {
	dir := t.TempDir()
	abs := touch(t, dir, "rel.go")
	got := extractPaths("Write", map[string]any{"file_path": "rel.go"}, nil, dir)
	if !reflect.DeepEqual(got, []string{abs}) {
		t.Errorf("got %v, want [%s]", got, abs)
	}
}

// Bash: only tokens that resolve to existing regular files survive; flags,
// subcommands and directory globs are dropped.
func TestExtractBashTokens(t *testing.T) {
	dir := t.TempDir()
	a := touch(t, dir, "a.go")
	cmd := "cat " + a + " && go test ./... && rm -f missing.txt"
	got := extractPaths("Bash", map[string]any{"command": cmd}, nil, dir)
	if !reflect.DeepEqual(got, []string{a}) {
		t.Errorf("got %v, want [%s]", got, a)
	}
}

// Grep content mode: lines look like "path:line:match"; the path prefix is
// recovered and gated.
func TestExtractGrepContentLines(t *testing.T) {
	dir := t.TempDir()
	a := touch(t, dir, "a.go")
	resp := a + ":42:\tfoo := bar\n" + a + ":51:\tbaz()"
	got := extractPaths("Grep", map[string]any{"pattern": "foo"}, resp, dir)
	if !reflect.DeepEqual(got, []string{a}) {
		t.Errorf("got %v, want [%s] (deduped)", got, a)
	}
}

// Glob: the response is a newline-separated list of files.
func TestExtractGlobList(t *testing.T) {
	dir := t.TempDir()
	a := touch(t, dir, "a.go")
	b := touch(t, dir, "b.go")
	got := extractPaths("Glob", map[string]any{"pattern": "*.go"}, a+"\n"+b, dir)
	if !reflect.DeepEqual(got, []string{a, b}) {
		t.Errorf("got %v, want [%s %s]", got, a, b)
	}
}

// Repeated paths within one tool call collapse to one entry, first-seen order.
func TestExtractDedup(t *testing.T) {
	dir := t.TempDir()
	a := touch(t, dir, "a.go")
	got := extractPaths("Bash", map[string]any{"command": "cat " + a + " " + a}, nil, dir)
	if !reflect.DeepEqual(got, []string{a}) {
		t.Errorf("got %v, want [%s]", got, a)
	}
}

func TestExtractUnknownTool(t *testing.T) {
	if got := extractPaths("WebFetch", map[string]any{"url": "http://x"}, nil, "/"); len(got) != 0 {
		t.Errorf("expected nothing for unknown tool, got %v", got)
	}
}
