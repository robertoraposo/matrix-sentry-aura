// Command cachesim measures the OPERATIONAL value of the agent's access log:
// it replays the real access stream from the journal through caching policies
// (LRU, LFU, Markov-prefetch) and reports hit-rate vs cache size. Markov-prefetch
// uses the same next-access predictor whose lift we measured live — here that
// lift becomes a concrete cache-hit-rate gain (no ranking-distortion problem).
//
//	go build -o cachesim ./cmd/cachesim
//	./cachesim -dir /root/sentry-journal -tenant 1 -caps "4,8,16,32,64"
package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"matrixsentry/internal/cache"
	"matrixsentry/sentry"
)

func main() {
	dir := flag.String("dir", "/root/sentry-journal", "journal directory")
	tenant := flag.Int("tenant", 1, "tenant id")
	capsCSV := flag.String("caps", "4,8,16,32,64", "cache sizes to sweep")
	flag.Parse()

	s, err := sentry.Open(*dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		panic(err)
	}
	defer s.Close()

	stream := accessStream(s, sentry.TenantID(*tenant))
	distinct := countDistinct(stream)
	fmt.Printf("== cachesim · tenant %d · %d accesses over %d distinct items ==\n", *tenant, len(stream), distinct)
	if len(stream) < 2 {
		fmt.Println("not enough access data yet")
		return
	}

	fmt.Printf("%-8s %-10s %-10s %-16s %-12s\n", "cap", "LRU", "LFU", "Markov-prefetch", "MK-vs-best")
	for _, c := range parseInts(*capsCSV) {
		lru := cache.HitRate(cache.NewLRU(c), stream)
		lfu := cache.HitRate(cache.NewLFU(c), stream)
		mk := cache.HitRate(cache.NewMarkovPrefetch(c), stream)
		best := lru
		if lfu > best {
			best = lfu
		}
		fmt.Printf("%-8d %-10.3f %-10.3f %-16.3f %+.3f\n", c, lru, lfu, mk, mk-best)
	}
}

// accessStream extracts the ordered item-id sequence (the same stream the
// analyzer reads) for a tenant.
func accessStream(s *sentry.Store, tenant sentry.TenantID) []int {
	var out []int
	etype := sentry.EventAccess
	t := tenant
	s.Scan(sentry.Filter{Tenant: &t, Type: &etype}, func(r sentry.Record) bool {
		var p sentry.AccessPayload
		if sentry.UnmarshalPayload(r.Payload, &p) == nil {
			out = append(out, int(p.ItemID))
		}
		return true
	})
	return out
}

func countDistinct(s []int) int {
	m := map[int]bool{}
	for _, x := range s {
		m[x] = true
	}
	return len(m)
}

func parseInts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if v, err := strconv.Atoi(p); err == nil {
				out = append(out, v)
			}
		}
	}
	return out
}
