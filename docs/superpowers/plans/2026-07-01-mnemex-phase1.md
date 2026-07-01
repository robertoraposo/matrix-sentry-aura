# Mnemex Phase 1 (MVP) — Implementation Plan

> Date: 2026-07-01 · Status: PLAN ONLY — no code yet (per owner). Spec:
> `docs/superpowers/specs/2026-07-01-mnemex-code-graph-with-memory-design.md`.
> Phase 1 = the MVP wedge: **L1 structure + L2 episodic memory + the `explain` MCP tool**, on Go + 2-3
> languages, reusing the MatrixSentry engine. Proves "the code that remembers why" end-to-end.

**Goal:** From a real repo, an MCP client calls `explain(symbol)` and gets structure (def + callers/callees)
FUSED with the recorded decisions/why that touched that symbol.

**Tech:** Go, zero external deps beyond tree-sitter bindings, single binary, build-on-Mac→ship-linux. Reuses
MatrixSentry's journal, vector store (recall/remember), and hand-rolled MCP server.

## Global constraints (Phase 1)

- Go; single static binary; reuse MatrixSentry engine (journal + vector store + MCP server), do not fork it
  gratuitously.
- Languages: start with **Go + TypeScript + Python** (highest agent-usage; all have solid tree-sitter grammars).
- No L3 hotness, no multi-agent breadth, no Cypher, no GUI, no bundled embedder — all deferred to P2+.
- TDD when code begins; each task ends with an independently testable deliverable.
- The star acceptance: `explain(symbol)` returns L1 + L2 fused in one call, verified on a real repo.

## Task breakdown (deliverables + interfaces + acceptance — code written at execution time)

### Task 1 — `codegraph`: tree-sitter → structural graph (L1)
- **Deliverable:** a Go package that parses a repo with tree-sitter (Go/TS/Python grammars) into a graph of
  nodes (function, method, class, file) and edges (CALLS, IMPORTS, DEFINES), each node keyed by a stable
  **qualified name** (e.g. `pkg/path.Type.Method`).
- **Interface (to define):** `Index(root string) (*Graph, error)`; `Graph.Symbol(qname) (Node, bool)`;
  `Graph.Callers(qname) []Node`; `Graph.Callees(qname) []Node`.
- **Acceptance:** on a fixture repo, symbols/edges match hand-verified expectations for the 3 languages;
  qualified names are stable across re-index.
- **Notes:** table-stakes; do not chase type resolution (that's P4). Unresolved calls recorded as best-effort.

### Task 2 — episodic-memory binding (L2)
- **Deliverable:** a mapping from a graph symbol (qualified name) to MatrixSentry memories (decisions/why)
  that reference it. Two directions: record a memory tagged with symbol qnames; recall memories for a symbol.
- **Interface (to define):** `Attach(qname string, memoryID uint64)`; `MemoriesFor(qname string) []Memory`;
  reuse MatrixSentry `remember`/`recall` for storage — Phase 1 links by symbol-qname tags + semantic recall.
- **Acceptance:** a decision recorded against `pkg.Foo.Bar` is retrievable via `MemoriesFor("pkg.Foo.Bar")`;
  semantic recall also surfaces related decisions even without an exact tag.
- **Open design point (resolve at execution):** exact linkage model — explicit qname tags on memories vs.
  embedding-similarity between the decision text and the symbol's context. Phase 1: start with explicit tags +
  semantic recall as fallback; measure before adding more.

### Task 3 — `explain` fusion query (L4, minimal)
- **Deliverable:** given a symbol, join L1 (def + callers/callees) with L2 (memories/why) into one result.
- **Interface (to define):** `Explain(qname string) ExplainResult` where `ExplainResult` has
  `{Definition, Callers, Callees, Decisions []Memory}`.
- **Acceptance:** on the fixture repo with seeded decisions, `Explain` returns both structure and the linked
  decisions in one call; empty-history symbols return structure with an empty decisions list (no error).

### Task 4 — MCP tool `explain` + binary (L5, minimal)
- **Deliverable:** expose `explain(symbol)` as an MCP tool on the MatrixSentry-style hand-rolled server; a
  single binary that indexes a repo and serves the tool.
- **Interface:** MCP tool `explain` args `{symbol}` → text/structured result rendering `ExplainResult`.
- **Acceptance:** from a real MCP client (Claude Code), `explain` on a symbol in a real repo returns fused
  structure+decisions; round-trip verified; indexing a mid-size repo completes in seconds.

### Task 5 — dogfood + measure
- **Deliverable:** run Mnemex on the MatrixSentry and/or blazeagent repos; seed a handful of real decisions;
  verify `explain` surfaces them fused with structure. Capture rough numbers (index time, query latency,
  answer usefulness) to validate the wedge before investing in P2.
- **Acceptance:** a short written result: does `explain` demonstrably beat "grep + read files" for "why is this
  the way it is" on a real repo? Go/no-go signal for P2 (L3 hotness).

## Sequencing & risk

- T1 (structure) and T2 (memory binding) are independent and could parallelize; T3 depends on both; T4 wraps
  T3; T5 validates.
- Biggest unknown = T2's linkage model (explicit tags vs embedding similarity) — flagged as an open design
  point to resolve with a measurement, not a guess.
- Deferred to later phases: L3 access/hotness, multi-agent live graph, 158 langs, Hybrid-LSP, bundled embedder.

## Explicitly NOT in Phase 1

Hotness/access (P2), multi-agent (P3), type resolution / 158 languages / Hybrid-LSP (P4), Cypher query
language, GUI, compiled embedder. Phase 1 is the smallest slice that proves the fusion thesis.
