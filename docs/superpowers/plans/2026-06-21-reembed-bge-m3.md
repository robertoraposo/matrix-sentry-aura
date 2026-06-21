# Re-embed Corpus to bge-m3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development for Task 1. Task 2 (the prod migration) is owner-coordinated — do NOT run it autonomously.

**Goal:** Build `cmd/reembed`, a lossless journal rewriter that re-embeds every memory's text to bge-m3 (1024-d) while copying all other events verbatim, so the 8808 corpus can move from nomic-768 to bge-m3-1024.

**Architecture:** Two-pass over the source journal: batch-embed all memory texts via the Ollama-compatible bge-m3 endpoint, then stream every record into a new journal (memory records get the new vector; everything else copied byte-for-byte, in order → seqs preserved).

**Tech Stack:** Go (sentry.Store Open/Scan/Append, memory.OllamaEmbedder, encoding/json, httptest for the test).

---

### Task 1: `cmd/reembed`

**Files:** Create `cmd/reembed/main.go`, `cmd/reembed/main_test.go`.

CONTEXT: `sentry.Open(dir string, sentry.Options{}) (*sentry.Store, error)`; `(*Store).Scan(Filter{}, func(Record) bool) error` (forward, seq order); `(*Store).Append(tenant sentry.TenantID, t sentry.EventType, payload any) (Seq, error)` (marshals payload via json; `json.RawMessage(b)` round-trips raw bytes). `sentry.Record{Seq, Tstamp int64, Type EventType, Tenant TenantID, Payload []byte}`. `memory.EventMemory` (=3); `memory.MemoryPayload{ID uint64, Text string, Vector []float32, Tags []string, Source string, Supersedes uint64}` (json tags: id,text,vec,tags,src,sup). `memory.NewOllamaEmbedder(url, model string, dim int) *OllamaEmbedder` with `.Embed([]string)([][]float32,error)` + `.Dim() int`. `memory.New(journal, embedder)` validates persisted dim == embedder dim.

- [ ] **Step 1: Write the failing test (`cmd/reembed/main_test.go`)**

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"matrixsentry/memory"
	"matrixsentry/sentry"
)

// fake bge-m3: Ollama-style /api/embed → fixed 4-d vector per input.
func fakeEmbedServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Input []string `json:"input"` }
		json.NewDecoder(r.Body).Decode(&in)
		out := struct{ Embeddings [][]float32 `json:"embeddings"` }{}
		for range in.Input {
			out.Embeddings = append(out.Embeddings, []float32{1, 2, 3, 4})
		}
		json.NewEncoder(w).Encode(out)
	})
}

func TestReembedRewritesMemoriesPreservesRest(t *testing.T) {
	srv := fakeEmbedServer()
	defer srv.Close()

	srcDir := filepath.Join(t.TempDir(), "src")
	src, err := sentry.Open(srcDir, sentry.Options{})
	if err != nil { t.Fatal(err) }
	// seed: 2 memories (one superseded by the second), 1 forget of #1, 1 access (non-memory)
	src.Append(1, memory.EventMemory, memory.MemoryPayload{ID: 1, Text: "alpha", Vector: []float32{9, 9}, Tags: []string{"x"}})
	src.Append(1, memory.EventMemory, memory.MemoryPayload{ID: 2, Text: "beta", Vector: []float32{8, 8}, Source: "s", Supersedes: 0})
	src.Append(1, sentry.EventAccess, sentry.AccessPayload{ItemID: 42, Source: "Read"})
	src.Append(1, memory.EventForget, memory.ForgetPayload{ID: 1})
	src.Close()

	src2, _ := sentry.Open(srcDir, sentry.Options{})
	defer src2.Close()
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst, _ := sentry.Open(dstDir, sentry.Options{})

	emb := memory.NewOllamaEmbedder(srv.URL, "bge-m3", 4)
	st, err := reembed(src2, dst, emb, 64)
	if err != nil { t.Fatal(err) }
	if st.records != 4 || st.memories != 2 {
		t.Fatalf("want 4 records / 2 memories, got %+v", st)
	}
	dst.Close()

	// reopen dst through memory.New with the 4-d embedder: must succeed (dim consistent)
	// and rebuild the SAME live set (memory #1 forgotten, #2 live).
	dst2, _ := sentry.Open(dstDir, sentry.Options{})
	defer dst2.Close()
	mem, err := memory.New(dst2, emb)
	if err != nil { t.Fatalf("memory.New on re-embedded journal failed: %v", err) }
	live := mem.List(1)
	if len(live) != 1 || live[0].ID != 2 {
		t.Fatalf("live set wrong after reembed: %+v", live)
	}
	if len(live[0].Vector) != 4 {
		t.Fatalf("re-embedded vector dim = %d, want 4", len(live[0].Vector))
	}
	// non-memory record preserved verbatim: scan dst for the access event
	et := sentry.EventAccess
	found := false
	dst2.Scan(sentry.Filter{Type: &et}, func(r sentry.Record) bool {
		var ap sentry.AccessPayload
		if sentry.UnmarshalPayload(r.Payload, &ap) == nil && ap.ItemID == 42 && ap.Source == "Read" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("non-memory (access) record was not copied verbatim")
	}
}
```
Run `go test ./cmd/reembed/ -run TestReembed -v` → FAIL (no package / `reembed` undefined).

- [ ] **Step 2: Implement `cmd/reembed/main.go`**

```go
// Command reembed rewrites a Matrix Sentry journal into a new one, re-embedding
// every memory's text with a new embedder (e.g. moving nomic-768 → bge-m3-1024).
// Every non-memory record (access, pathmap, forget, message, recall) is copied
// VERBATIM in original order, so journal seqs and all cross-references are
// preserved — only EventMemory vectors change vector space.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"matrixsentry/memory"
	"matrixsentry/sentry"
)

type stats struct {
	records  int
	memories int
}

// reembed streams src → dst: EventMemory texts are re-embedded via emb; all other
// records are copied verbatim. Two passes: batch-embed all memory texts, then
// rewrite in order.
func reembed(src, dst *sentry.Store, emb memory.Embedder, batch int) (stats, error) {
	var st stats
	// Pass 1: collect memory texts by seq.
	type mrec struct {
		seq uint64
		p   memory.MemoryPayload
	}
	var mems []mrec
	if err := src.Scan(sentry.Filter{}, func(r sentry.Record) bool {
		if r.Type == memory.EventMemory {
			var p memory.MemoryPayload
			if err := sentry.UnmarshalPayload(r.Payload, &p); err == nil {
				mems = append(mems, mrec{seq: uint64(r.Seq), p: p})
			}
		}
		return true
	}); err != nil {
		return st, err
	}
	// Batch-embed.
	vecBySeq := make(map[uint64][]float32, len(mems))
	if batch <= 0 {
		batch = 64
	}
	for i := 0; i < len(mems); i += batch {
		j := i + batch
		if j > len(mems) {
			j = len(mems)
		}
		texts := make([]string, 0, j-i)
		for _, m := range mems[i:j] {
			texts = append(texts, m.p.Text)
		}
		vecs, err := emb.Embed(texts)
		if err != nil {
			return st, fmt.Errorf("embed batch %d-%d: %w", i, j, err)
		}
		if len(vecs) != len(texts) {
			return st, fmt.Errorf("embed batch returned %d for %d texts", len(vecs), len(texts))
		}
		for k, m := range mems[i:j] {
			if len(vecs[k]) != emb.Dim() {
				return st, fmt.Errorf("embed dim %d != %d (seq %d)", len(vecs[k]), emb.Dim(), m.seq)
			}
			vecBySeq[m.seq] = vecs[k]
		}
	}
	// Pass 2: rewrite every record in order.
	if err := src.Scan(sentry.Filter{}, func(r sentry.Record) bool {
		st.records++
		if r.Type == memory.EventMemory {
			var p memory.MemoryPayload
			if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
				return true // skip undecodable (shouldn't happen)
			}
			p.Vector = vecBySeq[uint64(r.Seq)]
			if _, err := dst.Append(r.Tenant, memory.EventMemory, p); err != nil {
				st.records = -1
				return false
			}
			st.memories++
			return true
		}
		if _, err := dst.Append(r.Tenant, r.Type, json.RawMessage(r.Payload)); err != nil {
			st.records = -1
			return false
		}
		return true
	}); err != nil {
		return st, err
	}
	if st.records < 0 {
		return st, fmt.Errorf("append to dst failed mid-stream")
	}
	return st, nil
}

func main() {
	srcDir := flag.String("src", "", "source journal dir (current)")
	dstDir := flag.String("dst", "", "destination journal dir (new, must be empty)")
	url := flag.String("url", "http://100.93.11.62:11435", "embed base URL (Ollama-compatible)")
	model := flag.String("model", "bge-m3", "embedding model name")
	dim := flag.Int("dim", 1024, "new embedding dimension")
	batch := flag.Int("batch", 64, "embed batch size")
	flag.Parse()
	if *srcDir == "" || *dstDir == "" {
		fmt.Fprintln(os.Stderr, "reembed: -src and -dst required")
		os.Exit(2)
	}
	src, err := sentry.Open(*srcDir, sentry.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reembed: open src: %v\n", err)
		os.Exit(1)
	}
	defer src.Close()
	dst, err := sentry.Open(*dstDir, sentry.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reembed: open dst: %v\n", err)
		os.Exit(1)
	}
	emb := memory.NewOllamaEmbedder(*url, *model, *dim)
	st, err := reembed(src, dst, emb, *batch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reembed: %v\n", err)
		os.Exit(1)
	}
	dst.Close()
	// Verify dst rebuilds cleanly at the new dim.
	dst2, err := sentry.Open(*dstDir, sentry.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reembed: reopen dst: %v\n", err)
		os.Exit(1)
	}
	defer dst2.Close()
	if _, err := memory.New(dst2, emb); err != nil {
		fmt.Fprintf(os.Stderr, "reembed: VERIFY FAILED — dst does not rebuild at dim %d: %v\n", *dim, err)
		os.Exit(1)
	}
	fmt.Printf("reembed OK: %d records copied, %d memories re-embedded to %d-d. New journal: %s\n", st.records, st.memories, *dim, *dstDir)
}
```

Run `go test ./cmd/reembed/ -run TestReembed -v` → PASS. Then `go build ./... && go test ./... -race`, `go vet ./cmd/reembed/`.

- [ ] **Step 3: Commit**

```bash
git add cmd/reembed/
git commit -m "feat(reembed): lossless journal rewriter — re-embed memories to a new dim, copy rest verbatim"
```

---

### Task 2: Owner-coordinated prod migration (DO NOT auto-run)

**Files:** none (operational). The controller builds + ships, then STOPS and notifies the owner to run the cutover together (brief downtime on the live 8808 + corpus swap).

- [ ] **Step 1: Build + ship reembed to server1**
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/reembed ./cmd/reembed
scp -i ~/.ssh/matrix_sentry_homelab -o IdentitiesOnly=yes /tmp/reembed root@10.10.10.96:/root/reembed
```

- [ ] **Step 2: NOTIFY the owner — pause here.** Report that the tool is built/shipped and present the cutover steps (below) for the owner's go-ahead (it stops the live server). DO NOT proceed without it.

- [ ] **Step 3: Cutover (run WITH the owner)**
```bash
ssh matrix-sentry '
systemctl stop sentrymcp
/root/reembed -src /root/sentry-journal -dst /root/sentry-journal-1024 -url http://100.93.11.62:11435 -model bge-m3 -dim 1024
'   # verify: "reembed OK: N records, M memories re-embedded to 1024-d"
ssh matrix-sentry '
mv /root/sentry-journal /root/sentry-journal-768.bak
mv /root/sentry-journal-1024 /root/sentry-journal
sed -i "s|^SENTRY_OLLAMA_URL=.*|SENTRY_OLLAMA_URL=http://100.93.11.62:11435|; s|^SENTRY_EMBED_MODEL=.*|SENTRY_EMBED_MODEL=bge-m3|" /root/sentrymcp.env
grep -q "^SENTRY_EMBED_DIM=" /root/sentrymcp.env && sed -i "s|^SENTRY_EMBED_DIM=.*|SENTRY_EMBED_DIM=1024|" /root/sentrymcp.env || echo "SENTRY_EMBED_DIM=1024" >> /root/sentrymcp.env
systemctl start sentrymcp && sleep 2 && systemctl is-active sentrymcp
'
```
NOTE: confirm the sentrymcp systemd unit/env actually reads `SENTRY_EMBED_DIM` (the flag is `-embed-dim`, default 768) — if the unit pins `-embed-dim 768` on the ExecStart line, change it there to 1024 instead of/in addition to the env. Verify `systemctl cat sentrymcp` first.

- [ ] **Step 4: Verify the new space**
```bash
ssh matrix-sentry 'T=$(grep -h "^SENTRY_MCP_TOKEN=" /root/sentrymcp.env | cut -d= -f2-)
# count unchanged + a recall returns sane results in bge-m3 space + dim=1024
curl -s -H "Authorization: Bearer $T" http://localhost:8808/admin/corpus | python3 -c "import sys,json;d=json.load(sys.stdin);print(\"count\",d[\"count\"],\"dim\",d[\"dim\"])"
'
```
Expected: count unchanged (~227), dim 1024, recall on a known topic returns the right memory. Re-run the dedup-τ probe — relevance should be as good or better (multilingual).

- [ ] **Step 5: Rollback (only if broken)**
```bash
ssh matrix-sentry 'systemctl stop sentrymcp; rm -rf /root/sentry-journal; mv /root/sentry-journal-768.bak /root/sentry-journal; sed -i "s|11435|11434|; s|bge-m3|nomic-embed-text-v2-moe|; s|SENTRY_EMBED_DIM=1024|SENTRY_EMBED_DIM=768|" /root/sentrymcp.env; systemctl start sentrymcp'
```

- [ ] **Step 6: After success — update HANDOFF + the config memory; keep the .768.bak a while; commit docs.**
```bash
git add HANDOFF.md && git commit -m "docs: 8808 corpus re-embedded to bge-m3 1024-d (Ante Crucible), Ollama dropped"
```
