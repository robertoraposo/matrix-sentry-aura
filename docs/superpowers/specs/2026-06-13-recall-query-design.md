# Recall Query Improvement · Design Spec

> Date: 2026-06-13 · Owner: Alvin Nuñez (AlvinTLC) · Repo: `github.com/AlvinTLC/matrix-sentry` (private)
> Status: approved. Small, single-file change — inline TDD execution.

## Problem

The `sentry-recall` SessionStart hook builds its recall query from the cwd basename (`queryFromCwd` →
`"matrix-sentry"`). A bare directory name is a near-semantics-free token: it embeds far from and roughly
equidistant to everything, so recall distances cluster (~1.3 observed) and ordering is weak. The query
should resemble what is stored (project-describing prose), not how the folder is named.

## Decision

Build the query from richer, locally-available signals: the **git remote slug** (precise identity) plus the
**README intro** (semantics that embed in the same region as the project's stored memories), combined, with
graceful fallbacks. No network, no `git` binary — parse files directly (the hook stays zero-dependency and
best-effort).

## Components (all in `cmd/sentry-recall/main.go`, pure Go)

- **`gitSlug(cwd string) string`** — reads `cwd/.git/config`, finds the `[remote "origin"]` `url`, and
  extracts `org/repo`. Handles both HTTPS (`https://github.com/AlvinTLC/matrix-sentry.git`) and SSH
  (`git@github.com:AlvinTLC/matrix-sentry.git`) forms; strips a trailing `.git`. Returns `""` when there is
  no `.git/config`, no origin url, or the url has fewer than two path segments.
- **`readmeIntro(cwd string) string`** — finds a README (`README.md`, `README`, `README.txt`, case-insensitive
  match on `README*`), reads up to the first ~1KB, and returns the first heading line plus the first non-empty
  prose paragraph, with markdown markers (`#`, `>`, surrounding whitespace) stripped and internal newlines
  collapsed to spaces. Returns `""` when no README is found or it has no usable text.
- **`buildQuery(cwd string) string`** — composes with fallbacks, capping the result to `maxQueryLen` (500):
  - both present → `"<slug>: <readmeIntro>"`
  - only slug → `slug`
  - only README → `readmeIntro` (no leading `": "`)
  - neither → `queryFromCwd(cwd)` (the existing basename, retained as the last-resort fallback)
- **`main()`** — replace `query := queryFromCwd(cwd)` with `query := buildQuery(cwd)`. Everything else
  (config gating, the 5s synchronous recall, `formatOutput` injection) is unchanged.

Example for this repo: `"AlvinTLC/matrix-sentry: Matrix Sentry. Operational memory for code agents. A
from-scratch vector-search + memory engine in pure Go …"` (capped at 500).

## Error handling

Best-effort, matching the hook's existing contract: any read/parse failure in `gitSlug`/`readmeIntro` returns
`""` (never panics, never errors the session). `buildQuery` always returns a non-empty string when the cwd has
a basename (the final fallback), preserving today's behavior in the worst case.

## Testing (TDD, table-driven, temp-dir fixtures)

- `gitSlug`: HTTPS url → `AlvinTLC/matrix-sentry`; SSH url `git@github.com:Org/Repo.git` → `Org/Repo`;
  missing `.git/config` → `""`; config present but no origin → `""`; url with trailing `.git` stripped.
- `readmeIntro`: temp dir with a `README.md` (heading + blockquote + paragraph) → returns cleaned
  heading + first paragraph, markdown markers gone; no README → `""`; result length-capped.
- `buildQuery`: both signals → `"slug: intro"`; slug-only (no README) → `slug`; README-only (no git) →
  `intro`; neither → basename; total length ≤ `maxQueryLen`.

## Deployment

Rebuild and reinstall `~/.local/bin/sentry-recall` (Mac, native build). Activates on the next session start.
No `settings.json` change — the SessionStart hook registration is unchanged; only the binary's query logic
improves.

## Out of scope (YAGNI)

- Shelling out to the `git` binary (parse `.git/config` instead — no external dependency).
- Parsing the whole README (intro only).
- Touching the recall/injection flow or the MCP server.
- Worktree/submodule `.git` *file* (gitdir pointer) resolution — if `.git` is a file not a dir, `gitSlug`
  returns `""` and the README/basename fallbacks apply. (Can revisit if it matters in practice.)
