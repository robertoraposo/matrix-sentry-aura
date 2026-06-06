package main

import (
	"fmt"
	"os"
	"matrixsentry/sentry"
	"matrixsentry/sentry/access"
)

func main() {
	fmt.Println("== Matrix Sentry · SentryLog End-to-End Demo ==")

	dir, err := os.MkdirTemp("", "sentrydemo")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	fmt.Printf("Journal dir: %s\n\n", dir)

	// 1. Setup Store
	s, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		panic(err)
	}

	tenantA := sentry.TenantID(1)
	tenantB := sentry.TenantID(2)

	// 2. Append Interleaved Structured Access for Tenant A
	fmt.Println("[*] Appending structured access stream for Tenant A...")
	pattern := []uint64{10, 20, 30, 40}
	for i := 0; i < 100; i++ {
		// Sequential pattern: 10->20->30->40->10...
		item := pattern[i%len(pattern)]
		if _, err := s.Append(tenantA, sentry.EventAccess, sentry.AccessPayload{ItemID: item}); err != nil {
			panic(err)
		}

		// Noise for Tenant B
		if i%5 == 0 {
			if _, err := s.Append(tenantB, sentry.EventAccess, sentry.AccessPayload{ItemID: uint64(i * 1000)}); err != nil {
				panic(err)
			}
		}
	}

	// 3. Simulate Crash/Restart
	fmt.Println("[*] Simulating crash/restart...")
	if err := s.Close(); err != nil {
		panic(err)
	}

	s2, err := sentry.Open(dir, sentry.Options{FsyncEvery: 0})
	if err != nil {
		panic(err)
	}
	defer s2.Close()

	fmt.Printf("Recovered journal: %d total records\n", s2.ReadNextSeq()-1)

	// 4. Analyze Tenant A
	fmt.Println("\n[*] Analyzing Tenant A Predictability (Mechanism D feasibility):")
	report, err := access.Analyze(s2, tenantA)
	if err != nil {
		panic(err)
	}

	fmt.Printf("  Total Accesses : %d\n", report.Total)
	fmt.Printf("  Markov Hit     : %.2f%%\n", report.MarkovHit*100)
	fmt.Printf("  Marginal Hit   : %.2f%%\n", report.MarginalHit*100)
	fmt.Printf("  LIFT           : %.2f%%\n", report.Lift*100)
	fmt.Printf("  Coverage       : %.2f%%\n", report.Coverage*100)

	if report.Lift > 0.5 {
		fmt.Println("\n[SUCCESS] Lift detected! Mechanism D will be highly effective on this tenant.")
	} else {
		fmt.Println("\n[INFO] Low lift. This tenant has low sequential structure.")
	}
}
