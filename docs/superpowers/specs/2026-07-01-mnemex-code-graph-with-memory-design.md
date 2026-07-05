# Mnemex — Code Graph WITH Memory · Design Spec

> Date: 2026-07-01 · Owner: Alvin Nuñez (AlvinTLC) · Status: proposal (design approved, no code yet)
> Working name: **Mnemex** (alt: SentryGraph). A standalone code-intelligence product that competes with
> DeusData's `codebase-memory-mcp` by DIFFERENTIATION, not parity: a code graph that remembers its own
> history and knows how it is actually used, with agents coordinating over it.

## Problem / opportunity

AI coding agents need a map of the codebase. The incumbent, DeusData `codebase-memory-mcp` (23k★, C, arXiv
preprint, 158 languages via tree-sitter, Hybrid-LSP type resolution, 14 MCP tools incl. `semantic_query` with
bundled Nomic embeddings and manual ADR management), is excellent at **structural** code intelligence: it
indexes the source AS-IS into a knowledge graph (symbols, refs, call chains, routes) and answers "where is X,
who calls it, what breaks."

Its graph is **static**: re-index and it is identical for everyone. It has no memory of **what the team/agents
did, why, and how the code got to be this way**, and no model of **how the code is actually used** (which
symbols are hot, what changes together). Those two are exactly the DNA of the MatrixSentry engine (semantic
recall/remember, a next-access Markov model, access-gated refinement, and a multi-agent comms channel).

**Thesis:** don't out-build their AST parser (their moat, years-deep). Build the category they cannot occupy
from a static index — a code graph that gets *smarter the more the team works in it*.

## Strategic positioning

- **Pitch:** *Not just what your code is — how it got this way, why, what's hot, and your agents coordinating
  over it.*
- **Moat = data flywheel.** A static index is the same for everyone after re-indexing. Mnemex's graph is
  enriched by every decision recorded and every access observed, so it compounds with use — a moat a static
  index structurally cannot have.
- **Do NOT chase** 158 languages or Hybrid-LSP. Ship 2-3 languages via tree-sitter + real LSP later. Win by
  CATEGORY (memory + access fusion), not parser parity.

## Architecture — a graph in 5 layers

| Layer | Responsibility | Source |
|-------|----------------|--------|
| **L1 Structure** | tree-sitter → symbols / refs / call-graph nodes+edges | table-stakes (tree-sitter, same as incumbent) |
| **L2 Episodic memory** | every symbol/file linked to the decisions/attempts/outcomes that touched it, over time | MatrixSentry memory engine (recall/remember) |
| **L3 Access / hotness** | per-node usage weight (who/what touches it, what co-changes), next-access prediction, impact surface | MatrixSentry Markov access model + access-gated refinement |
| **L4 Fusion / query** | join structure + history + hotness in one query | new (the glue) |
| **L5 MCP + multi-agent** | single binary, local embeddings, agents coordinate over one live graph | MatrixSentry sentrymcp + comms |

Each layer is an isolated unit with a defined interface: L1 produces the structural graph; L2 attaches
episodic edges keyed by symbol qualified-name; L3 attaches usage weights from an access stream; L4 joins them
for queries; L5 serves them over MCP. L1–L3 can be developed and tested independently; L4 depends on all three;
L5 wraps L4.

## The star query (the "10×" moment)

One MCP tool **`explain(symbol)`** returns, in a single call:
- **L1:** definition + callers/callees + type-resolved refs.
- **L2:** the decisions / PRs / "why" that touched this symbol over time (episodic recall).
- **L3:** hotness + what it co-changes with + predicted impact surface.

No incumbent returns L1+L2+L3 fused in one call. This is the product's identity and the demo.

## Reuse of the MatrixSentry engine (head start)

Mnemex is **Go** (matches MatrixSentry, so the engine is reusable; tree-sitter has Go bindings). It reuses,
not reinvents:
- the SentryLog journal + in-RAM index pattern (durable append-only + fast reads),
- the vector store / recall-remember (L2 semantic memory),
- the access model + access-gated refinement (L3),
- the hand-rolled MCP server + comms channel (L5),
- the build-on-Mac→ship-linux, single-binary, zero-external-dep discipline.

New code is concentrated in L1 (tree-sitter binding + graph model), L3's code-specific access mapping, and L4
(the fusion query). L2/L5 are largely adaptation of existing engine code.

## Honest cost / risk

- Multi-month product, MVP-first. The expensive part you must NOT build is their AST/type-resolution at 158
  languages + Hybrid-LSP; start with 2-3 languages and defer/borrow LSP.
- Risk of a diffuse message ("all three fused") is mitigated by leading with the single `explain` query as the
  headline and shipping the MVP wedge (below) before L3/L5 breadth.
- Embeddings: incumbent bundles Nomic `nomic-embed-code` compiled in (no Ollama). MVP may start on the existing
  Ollama/embedder path and move to a bundled/compiled embedder later (a known, deferrable optimization).

## MVP wedge (Phase 1) — proves the whole thesis on the cheapest slice

**L1 (tree-sitter, Go + 2-3 languages) + L2 (episodic memory tied to symbols, reusing MatrixSentry) + the
`explain` tool over MCP.** Defer L3 hotness, full multi-agent, 158 languages, Hybrid-LSP. Goal: demonstrate
"the code that remembers why" on real repos, end-to-end through an MCP client. Detailed in the Phase 1 plan
(`docs/superpowers/plans/2026-07-01-mnemex-phase1.md`).

## Roadmap (phases)

- **P1 (MVP):** L1 structure + L2 episodic memory + `explain` (Go + 2-3 langs). ← this proposal's plan
- **P2:** L3 access/hotness (Markov next-access + co-change + predicted impact).
- **P3:** L5 multi-agent over a live shared graph + more languages.
- **P4:** Hybrid-LSP-equivalent type resolution, scale, compiled/bundled embeddings.

## Out of scope (YAGNI, for the product and especially the MVP)

- Competing on language count or Hybrid-LSP in P1 (their moat; deferred to P4).
- A GUI/graph-visualization (incumbent has one; not the differentiator).
- Cypher-compatible query language in P1 (the `explain` tool + a few targeted queries beat a general query
  language for the MVP demo).
- Re-implementing a bundled embedder in P1 (reuse the existing embedder path first).
