package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGitConfig writes a minimal .git/config with the given origin url into dir.
func writeGitConfig(t *testing.T, dir, url string) {
	t.Helper()
	gitdir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = " + url + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(gitdir, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGitSlug(t *testing.T) {
	httpsDir := t.TempDir()
	writeGitConfig(t, httpsDir, "https://github.com/AlvinTLC/matrix-sentry.git")
	if got := gitSlug(httpsDir); got != "AlvinTLC/matrix-sentry" {
		t.Errorf("https slug = %q, want AlvinTLC/matrix-sentry", got)
	}

	sshDir := t.TempDir()
	writeGitConfig(t, sshDir, "git@github.com:Org/Repo.git")
	if got := gitSlug(sshDir); got != "Org/Repo" {
		t.Errorf("ssh slug = %q, want Org/Repo", got)
	}

	noGit := t.TempDir()
	if got := gitSlug(noGit); got != "" {
		t.Errorf("no .git/config slug = %q, want empty", got)
	}

	noOrigin := t.TempDir()
	gitdir := filepath.Join(noOrigin, ".git")
	os.MkdirAll(gitdir, 0o755)
	os.WriteFile(filepath.Join(gitdir, "config"), []byte("[core]\n\tbare = false\n"), 0o644)
	if got := gitSlug(noOrigin); got != "" {
		t.Errorf("no origin slug = %q, want empty", got)
	}
}

func TestReadmeIntro(t *testing.T) {
	dir := t.TempDir()
	readme := "# Matrix Sentry\n\n> Operational memory for code agents. A vector-search engine in pure Go.\n\nMore detail below.\n"
	os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644)
	got := readmeIntro(dir)
	if !strings.Contains(got, "Matrix Sentry") || !strings.Contains(got, "Operational memory for code agents") {
		t.Fatalf("intro missing heading/paragraph: %q", got)
	}
	if strings.ContainsAny(got, "#>") {
		t.Fatalf("markdown markers not stripped: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("intro should be single-line (newlines collapsed): %q", got)
	}

	if got := readmeIntro(t.TempDir()); got != "" {
		t.Errorf("no README -> %q, want empty", got)
	}
}

func TestBuildQueryComposition(t *testing.T) {
	// both signals -> "slug: intro"
	both := t.TempDir()
	writeGitConfig(t, both, "https://github.com/AlvinTLC/matrix-sentry.git")
	os.WriteFile(filepath.Join(both, "README.md"), []byte("# Matrix Sentry\n\nOperational memory for agents.\n"), 0o644)
	q := buildQuery(both)
	if !strings.HasPrefix(q, "AlvinTLC/matrix-sentry: ") || !strings.Contains(q, "Operational memory") {
		t.Fatalf("combined query = %q", q)
	}

	// slug only (no README) -> slug
	slugOnly := t.TempDir()
	writeGitConfig(t, slugOnly, "git@github.com:Org/Repo.git")
	if q := buildQuery(slugOnly); q != "Org/Repo" {
		t.Fatalf("slug-only query = %q, want Org/Repo", q)
	}

	// README only (no git) -> intro, no leading ": "
	readmeOnly := t.TempDir()
	os.WriteFile(filepath.Join(readmeOnly, "README.md"), []byte("# Thing\n\nDoes stuff.\n"), 0o644)
	q = buildQuery(readmeOnly)
	if strings.HasPrefix(q, ":") || !strings.Contains(q, "Does stuff") {
		t.Fatalf("readme-only query = %q", q)
	}

	// neither -> basename fallback
	bare := filepath.Join(t.TempDir(), "myproj")
	os.MkdirAll(bare, 0o755)
	if q := buildQuery(bare); q != "myproj" {
		t.Fatalf("bare query = %q, want myproj (basename fallback)", q)
	}
}

func TestBuildQueryCapsLength(t *testing.T) {
	dir := t.TempDir()
	writeGitConfig(t, dir, "https://github.com/Org/Repo.git")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Big\n\n"+strings.Repeat("word ", 500)), 0o644)
	if q := buildQuery(dir); len(q) > maxQueryLen {
		t.Fatalf("query %d chars, want <= %d", len(q), maxQueryLen)
	}
}

func TestParseSessionStart(t *testing.T) {
	in := `{"cwd":"/Users/x/Downloads/matrix-sentry","source":"startup","hook_event_name":"SessionStart"}`
	h, err := parseHook([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if h.Cwd != "/Users/x/Downloads/matrix-sentry" || h.Source != "startup" {
		t.Fatalf("parsed %+v", h)
	}
}

func TestQueryFromCwd(t *testing.T) {
	cases := map[string]string{
		"/Users/x/Downloads/matrix-sentry": "matrix-sentry",
		"/home/alvin/blazeolt/":            "blazeolt",
		"/srv/repo":                        "repo",
		"":                                 "",
	}
	for in, want := range cases {
		if got := queryFromCwd(in); got != want {
			t.Errorf("queryFromCwd(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShouldInject(t *testing.T) {
	// inject on startup/resume; skip compact/clear (context already present)
	yes := []string{"startup", "resume", ""}
	no := []string{"compact", "clear"}
	for _, s := range yes {
		if !shouldInject(s) {
			t.Errorf("shouldInject(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if shouldInject(s) {
			t.Errorf("shouldInject(%q) = true, want false", s)
		}
	}
}

func TestBuildRecallBody(t *testing.T) {
	b := buildRecallBody("matrix-sentry", 5)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	p := m["params"].(map[string]any)
	if p["name"] != "recall" {
		t.Fatalf("name = %v", p["name"])
	}
	args := p["arguments"].(map[string]any)
	if args["query"] != "matrix-sentry" || args["k"].(float64) != 5 {
		t.Fatalf("args = %v", args)
	}
}

func TestParseRecallResponseExtractsText(t *testing.T) {
	resp := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"3 relevant memories for \"x\":\n1. [#2] foo"}]}}`
	txt := parseRecallText([]byte(resp))
	if !strings.Contains(txt, "relevant memories") {
		t.Fatalf("extracted %q", txt)
	}
}

func TestParseRecallResponseHandlesError(t *testing.T) {
	if txt := parseRecallText([]byte(`not json`)); txt != "" {
		t.Fatalf("bad json should yield empty, got %q", txt)
	}
	if txt := parseRecallText([]byte(`{"result":{}}`)); txt != "" {
		t.Fatalf("missing content should yield empty, got %q", txt)
	}
}

func TestFormatOutputInjectsContext(t *testing.T) {
	out := formatOutput("3 relevant memories for \"matrix-sentry\":\n1. [#2] foo")
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	hso := m["hookSpecificOutput"].(map[string]any)
	if hso["hookEventName"] != "SessionStart" {
		t.Fatalf("hookEventName = %v", hso["hookEventName"])
	}
	ctx, _ := hso["additionalContext"].(string)
	if !strings.Contains(ctx, "foo") || !strings.Contains(strings.ToLower(ctx), "memor") {
		t.Fatalf("additionalContext missing memory content: %q", ctx)
	}
}

func TestFormatOutputEmptyWhenNoMemories(t *testing.T) {
	// recall text saying nothing was found -> no injection (empty output)
	for _, txt := range []string{"", "no memories found for \"x\" (0 stored matches)"} {
		if out := formatOutput(txt); out != "" {
			t.Fatalf("expected no output for %q, got %q", txt, out)
		}
	}
}

func TestFormatOutputTruncatesTo10k(t *testing.T) {
	big := "1 relevant memory:\n" + strings.Repeat("x", 20000)
	out := formatOutput(big)
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	ctx := m["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	if len(ctx) > 10000 {
		t.Fatalf("additionalContext %d chars, must be <= 10000", len(ctx))
	}
}
