// Command cachesim measures the OPERATIONAL value of the agent's access log:
// it replays the real access stream from the journal through caching policies
// (LRU, LFU, Markov-prefetch) and reports hit-rate vs cache size. Markov-prefetch
// uses the same next-access predictor whose lift we measured live — here that
// lift becomes a concrete cache-hit-rate gain (no ranking-distortion problem).
//
// The cost model turns hit-rate into operational numbers under the REAL access
// distribution: critical-path latency per access = h·Lhit + (1−h)·Lmiss (the
// agent only waits on misses; prefetch happens off the critical path), and
// fetch bandwidth = (misses + prefetch loads) per access — the traffic a
// prefetching policy pays that a classical one does not.
//
// SCOPE (adversarial review, 2026-06-09): the journal stream is ID-ADDRESSED
// (the agent re-accesses a known item id/path), so Lmiss is the cost of
// FETCHING AN ITEM BY ID from storage (CAS/disk; default 200µs, deployment-
// dependent) — NOT the ~ms full ANN search, which belongs to query-addressed
// workloads where a hit cannot even be detected without searching. The
// relative columns (saved%, xtra-band) are insensitive to the Lmiss scale
// whenever Lmiss ≫ Lhit; only the absolute µs/access column depends on it.
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
	lhitUS := flag.Float64("lhit-us", 0.3, "hit cost µs (resident-id lookup; cachebench-measured ~250ns)")
	lmissUS := flag.Float64("lmiss-us", 200, "miss cost µs (ID-ADDRESSED storage fetch — deployment-dependent; NOT an ANN search)")
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

	fmt.Printf("cost model: Lhit=%.1fµs Lmiss=%.0fµs (latency = h·Lhit + (1−h)·Lmiss; agent waits only on misses)\n\n", *lhitUS, *lmissUS)
	fmt.Printf("%-8s %-7s | %-7s %-12s %-9s | %-10s %-10s\n",
		"cap", "policy", "hit%", "µs/access", "saved", "fetch/acc", "xtra-band")
	for _, c := range parseInts(*capsCSV) {
		lru := cache.Replay(cache.NewLRU(c), stream)
		lfu := cache.Replay(cache.NewLFU(c), stream)
		mk := cache.Replay(cache.NewMarkovPrefetch(c), stream)

		lat := func(st cache.Stats) float64 {
			h := st.HitRatio()
			return h**lhitUS + (1-h)**lmissUS
		}
		band := func(st cache.Stats) float64 {
			return float64(st.Accesses-st.Hits+st.PrefetchLoads) / float64(st.Accesses)
		}
		lruLat := lat(lru)
		for _, row := range []struct {
			name string
			st   cache.Stats
		}{{"LRU", lru}, {"LFU", lfu}, {"MK", mk}} {
			fmt.Printf("%-8d %-7s | %-7.3f %-12.0f %-9s | %-10.3f %-10s\n",
				c, row.name, row.st.HitRatio(), lat(row.st),
				fmt.Sprintf("%+.1f%%", (lruLat-lat(row.st))/lruLat*100),
				band(row.st),
				fmt.Sprintf("%+.1f%%", (band(row.st)-band(lru))/band(lru)*100))
		}
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
