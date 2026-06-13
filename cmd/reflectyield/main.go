// Command reflectyield is the Phase-0 measurement for auto-remember. It walks
// real Claude Code transcripts, segments each session into windows of K
// tool-uses, and emits one JSON object per window on stdout so a judge (the
// same LLM that will do self-report) can label "does this window contain a
// durable fact?". The point is to calibrate K (so each reflection trigger has
// ~1 expected durable fact) and to sanity-check that the captured facts are
// real knowledge, not noise — before any production wiring is trusted.
//
//	go run ./cmd/reflectyield -dir <transcripts> -k 40 > windows.jsonl
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"matrixsentry/internal/transcript"
)

func main() {
	dir := flag.String("dir", "", "directory of Claude Code .jsonl transcripts")
	k := flag.Int("k", 40, "tool-uses per window")
	minText := flag.Int("min-text", 200, "skip windows with fewer than N chars of prose")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: reflectyield -dir <transcripts dir> [-k N] [-min-text N]")
		os.Exit(2)
	}
	paths, _ := filepath.Glob(filepath.Join(*dir, "*.jsonl"))
	enc := json.NewEncoder(os.Stdout)
	sessions, total, emitted := 0, 0, 0
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		session := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		windows, err := transcript.Windows(session, f, *k)
		f.Close()
		if err != nil {
			continue
		}
		sessions++
		for _, w := range windows {
			total++
			if len(w.Text) < *minText {
				continue
			}
			emitted++
			_ = enc.Encode(w)
		}
	}
	fmt.Fprintf(os.Stderr, "sessions=%d windows=%d emitted(text>=%d)=%d k=%d\n",
		sessions, total, *minText, emitted, *k)
}
