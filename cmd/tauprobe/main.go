// Command tauprobe calibrates the auto-remember dedup threshold τ against the
// REAL embedder (the same memory.OllamaEmbedder the production MCP uses). It
// embeds a small probe set — pairs of paraphrases of the SAME durable fact, plus
// several DISTINCT facts — and prints the pairwise squared-L2 distances. A valid
// τ sits in the gap: above the largest within-paraphrase distance (so restated
// facts dedup) and below the smallest across-fact distance (so distinct facts
// are never merged). Read-only: it touches no journal, only the embed endpoint.
//
//	SENTRY_OLLAMA_URL=http://host:11434 SENTRY_EMBED_MODEL=nomic-embed-text-v2-moe \
//	  tauprobe
package main

import (
	"flag"
	"fmt"
	"os"

	"matrixsentry/memory"
)

// probe is one labeled text. Items sharing a group are paraphrases of the same
// durable fact and SHOULD dedup; different groups are distinct facts and SHOULD NOT.
type probe struct {
	group string
	text  string
}

// Representative probe set (2026-06-14): mirrors the REAL production collisions
// where tau=0.85 over-collapsed. Same group = true paraphrase (SHOULD dedup);
// different groups = distinct facts that SHARE domain vocabulary (SHOULD NOT
// dedup) — these are the close cross-group pairs the original cross-topic probe
// never measured.
var probes = []probe{
	// Paraphrase pair (must dedup): the high-volume auto-reflect restatement case.
	{"arstatus", "Matrix Sentry auto-remember is live: a global Stop hook self-reports durable knowledge and the server deduplicates remembers."},
	{"arstatus", "Auto-remember is now automatic in Matrix Sentry — a Stop hook makes the agent self-report durable facts, deduplicated server-side."},
	// Same-domain DISTINCT pair (must NOT dedup): the BlazeTeams→#4 collision.
	{"bs-stack", "BlazeSphere standard stack: Go backend with chi router + Huma v2 (OpenAPI) + pgx + River job queue, PostgreSQL primary DB."},
	{"bt-stack", "BlazeTeams backend uses Fiber + GORM (NOT chi/Huma); only the frontend follows the generic BlazeSphere convention."},
	// Same-project DISTINCT pair (must NOT dedup): the Ashley perception→#37 collision.
	{"ashley-brain", "Ashley arm64 neural-network brain: float32 neurons in an mmap arena; _brain_tick computes weight*input+bias then activate."},
	{"ashley-perc", "Ashley perception: a 16x16 retina, center-of-mass tracking of the brightest region, creator-face ADMIN AI recognition."},
	// Same-domain DISTINCT pair (must NOT dedup): the GP-feasibility→#22 collision.
	{"ivfcfg", "ivf.Recommended production config: nlist 64, M 96, K 64, nprobe 32, with exact re-rank of the top 200."},
	{"gpfeas", "GP per-query adaptive policy gated FAIL: coarse-margin features don't predict needed search depth despite 95% oracle headroom."},
}

func sqL2(a, b []float32) float32 {
	var d float32
	for i := range a {
		diff := a[i] - b[i]
		d += diff * diff
	}
	return d
}

func main() {
	url := flag.String("ollama", os.Getenv("SENTRY_OLLAMA_URL"), "Ollama base URL")
	model := flag.String("model", envOr("SENTRY_EMBED_MODEL", "nomic-embed-text"), "embedding model")
	dim := flag.Int("dim", 768, "embedding dimension")
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "set SENTRY_OLLAMA_URL or -ollama")
		os.Exit(2)
	}

	emb := memory.NewOllamaEmbedder(*url, *model, *dim)
	texts := make([]string, len(probes))
	for i, p := range probes {
		texts[i] = p.text
	}
	vecs, err := emb.Embed(texts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "embed:", err)
		os.Exit(1)
	}

	var maxWithin, minAcross float32
	minAcross = 1e30
	fmt.Printf("model=%s dim=%d n=%d\n\n", *model, *dim, len(probes))
	fmt.Println("pairwise squared-L2 (W=within-fact paraphrase, A=across distinct facts):")
	for i := 0; i < len(probes); i++ {
		for j := i + 1; j < len(probes); j++ {
			d := sqL2(vecs[i], vecs[j])
			kind := "A"
			if probes[i].group == probes[j].group {
				kind = "W"
				if d > maxWithin {
					maxWithin = d
				}
			} else if d < minAcross {
				minAcross = d
			}
			fmt.Printf("  [%s] %-9s vs %-9s : %.4f\n", kind, probes[i].group, probes[j].group, d)
		}
	}
	fmt.Printf("\nmax within-paraphrase (should dedup) = %.4f\n", maxWithin)
	fmt.Printf("min across-distinct  (must NOT dedup) = %.4f\n", minAcross)
	if maxWithin < minAcross {
		fmt.Printf("=> separable. Suggested tau = %.4f (midpoint of the gap)\n", (maxWithin+minAcross)/2)
	} else {
		fmt.Printf("=> NOT separable at this probe set: paraphrases overlap distinct facts; pick tau conservatively (<= %.4f) and prefer false-negatives (store dups) over merging distinct facts\n", minAcross)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
