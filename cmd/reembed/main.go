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
	var failErr error // fail loud on any corrupt/undecodable record — never silently drop (this rewrites prod data)
	if err := src.Scan(sentry.Filter{}, func(r sentry.Record) bool {
		if r.Type == memory.EventMemory {
			var p memory.MemoryPayload
			if err := sentry.UnmarshalPayload(r.Payload, &p); err != nil {
				failErr = fmt.Errorf("decode EventMemory seq %d: %w", r.Seq, err)
				return false
			}
			mems = append(mems, mrec{seq: uint64(r.Seq), p: p})
		}
		return true
	}); err != nil {
		return st, err
	}
	if failErr != nil {
		return st, failErr
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
				failErr = fmt.Errorf("decode EventMemory seq %d: %w", r.Seq, err)
				return false
			}
			p.Vector = vecBySeq[uint64(r.Seq)]
			if _, err := dst.Append(r.Tenant, memory.EventMemory, p); err != nil {
				failErr = fmt.Errorf("append memory seq %d: %w", r.Seq, err)
				return false
			}
			st.memories++
			return true
		}
		if _, err := dst.Append(r.Tenant, r.Type, json.RawMessage(r.Payload)); err != nil {
			failErr = fmt.Errorf("append seq %d type %d: %w", r.Seq, r.Type, err)
			return false
		}
		return true
	}); err != nil {
		return st, err
	}
	if failErr != nil {
		return st, failErr
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
