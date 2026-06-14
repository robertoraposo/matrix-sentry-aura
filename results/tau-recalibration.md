# Dedup τ Recalibration (2026-06-14)

> Triggered by three independent production collisions where `τ=0.85` over-collapsed DISTINCT facts:
> my session (#22/#40), the Ashley session (perception vs #37 brain), and the BlazeTeams session
> (Fiber+GORM stack deduped against #4's chi+Huma BlazeSphere-generic stack — a *contradicting* collapse
> that would make recall return the wrong stack).

## Why the original calibration was wrong

The first `cmd/tauprobe` run measured "distinct" with **cross-topic** facts (journal gotcha vs ivf config vs
bot-fight) → min distance **1.238**, so `τ=0.85` looked safe. But real distinct facts that share **domain
vocabulary** embed far closer. Re-measured with representative same-domain pairs against the live embedder
(`nomic-embed-text-v2-moe`, dim 768):

```
[W] arstatus  vs arstatus      : 0.2072   (true paraphrase — SHOULD dedup)
[A] bs-stack  vs bt-stack      : 0.6846   (BlazeSphere vs BlazeTeams stack — distinct, MUST NOT dedup) ← real floor
[A] ashley-brain vs ashley-perc: 1.1427   (distinct)
[A] ivfcfg    vs gpfeas        : 1.3802   (distinct)

max within-paraphrase (should dedup)  = 0.2072
min across-distinct  (must NOT dedup) = 0.6846   ← τ=0.85 was ABOVE this → collapse
```

## Decision: τ = 0.45

Clean, comfortable separation — paraphrase 0.21 ≪ **0.45** ≪ 0.68 distinct (margin ~0.24 both sides):
- still dedups the high-volume auto-reflect restatements (e.g. repeated "auto-remember is live" status, 0.21);
- lets distinct same-domain facts through (the BlazeSphere/BlazeTeams pair at 0.68 now stores separately).

Biased toward false-negatives by design: now that `forget` exists, a stored near-duplicate is cheap to clean,
whereas a collapsed distinct fact (or a contradicting collapse like BlazeTeams→#4) is silent corruption.

**It is a SERVER-side change (`SENTRY_DEDUP_TAU` env), so it fixes ALL clients at once — including the
already-connected sessions whose stale tool cache can't reach `force`/`supersede`/`forget`.** That is why
lowering τ is the right lever for the real world, not "reconnect every session".

## Deploy status: DONE + verified (2026-06-14, user-authorized)

`SENTRY_DEDUP_TAU=0.85 → 0.45` on the VM + `systemctl restart sentrymcp` (active). Live verification against
the exact production scenario:
- BlazeTeams (Fiber+GORM) distinct fact → `remembered as memory #62` (stored separately; at τ=0.85 it
  deduped against #4 — the bug). Forgotten afterward (`forgot #62`) to keep the corpus clean.
- BlazeSphere paraphrase of #4 → `already known as memory #4 (deduped)` — true paraphrases still collapse.

Because it is server-side, it fixes ALL clients at once, including the stale Ashley/BlazeTeams sessions that
can't reach `force` — no reconnect needed. `cmd/tauprobe`'s probe set is updated to the representative pairs
so the calibration is reproducible.
