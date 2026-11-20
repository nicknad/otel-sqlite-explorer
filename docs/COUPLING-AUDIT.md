# Coupling audit: otel-sqlite-explorer ↔ otel-sqlite

**Date:** 2026-11-19 · **Method:** executable schema diffs (sink migrations applied
to in-memory SQLite 3.53.1, compared against both explorer DDL mirrors), git
history correlation, and direct reading of both repositories. All findings below
are evidence, not opinion. Decisions marked **[OPEN D-n]** remain the
maintainer's; nothing in this document decides them for you.

---

## Update — 2026-11-19 (afternoon)

- **D-5 resolved: GitHub is the only forge.** Codeberg is to be archived; this
  repo's contributor and conduct links now point at GitHub.
- **The sink image is published:** ghcr.io/nicknad/otel-sqlite (tag `latest`;
  the sink gained publish.yml/scan.yml workflows and docs/operations/docker.md).
  §7's "no tags, no releases, no publish job" premise is obsolete: channel (a)
  GHCR is live, build-from-source (c) is retired, and the §11 tracer bullet is
  implemented against the image in internal/tests/sink_image_test.go.

---

## 1. Executive summary

1. **The contract exists but is maintained by hand — and it has already drifted
   twice.** The sink's canonical schema (baseline_006 ≡ upgrades 000–006, verified
   identical) is copied in this repo twice; the compat mirror self-documents as
   "000 + 003 + 004" and predates the Nov 7–8 squash and search-mode changes;
   the mockdb schema diverges in 13 objects. The explorer still *works* against a
   real 006 database (all nine floor columns are present in the 20-column view) —
   but its tests validate a schema that no longer exists.
2. **One live bug class found:** sink `[search] mode = "off"` installs `logs_fts`
   present-but-empty with no triggers; the explorer's `HasFTS()` (table existence)
   then reports true and searches silently return zero rows instead of falling
   back to substring matching.
3. **The only full-stack verification is manual.** `tests/docker/compose.stack-e2e.yml`
   in the sink explicitly says "MANUAL / NON-HERMETIC … NOT run by CI" and builds
   the explorer from a sibling checkout.
4. **CI asymmetry:** the sink has nine CI arms; the explorer has none at all
   (no `.github/` directory exists in this repo).
5. **Canonical forge is GitHub; Codeberg is a stale leftover.** The sink's local
   remote is `git@github.com:nicknad/otel-sqlite.git` (236 commits, pushed
   2026-11-16). The Codeberg copy sits on `master`, untouched since 2026-09-15
   (`mirror: false`). This repo still sends contributors to Codeberg
   (`CONTRIBUTING.md:16`, `README.md` ×3, `CODE_OF_CONDUCT.md:39`).

## 2. The contract surface: one source of truth, two stale mirrors

| Copy | Lives in | Used by | Verified drift vs baseline_006 |
|------|----------|---------|-------------------------------|
| Canonical | sink `migrations/baseline_006.sql` | the actual database | — (chain 000→006 replay produces an identical schema; diff empty) |
| Mirror A | `internal/schema/schema.go` `const DDL` | mockdb + integration tests | Missing 13 objects (`schema_migrations`, `search_index_state`, `logs_fts` + 4 FTS shadow tables, both triggers, all 4 canonical indexes); adds 3 non-canonical indexes; `log_event.id` AUTOINCREMENT vs INTEGER PK; `log_resource` lacks `created_at`; `logs` view 9 vs 20 columns |
| Mirror B | `internal/tests/collector_compat_test.go` `const collectorDDL` | compat tests | Still carries `metric`/`scope` tables dropped by 005; legacy six-column `logs_fts` + six-column trigger (the `full` mode, not the default `body`); missing `search_index_state`; `log_event` column order differs (`attributes_json` last vs 15th) |
| (Variant) | `cmd/mockdb/main.go` | demo DBs | A *third* FTS shape: `fts5(body, service_name, content='')` — no `contentless_delete`, no diacritics handling, **no triggers** (one-shot backfill), no state table |

Consequences:

- `cmd/mockdb/main.go`'s header claims "the exact production schema" — false since
  at least the Nov 7 squash. Mock DBs exercise neither the real access-path
  indexes nor the real FTS lifecycle.
- The compat tests pin the explorer to the legacy `full` FTS shape, so the
  *default* `body` shape and the `off` mode have never been tested.
- Mirror B's `logs` view column set matches canonical (good), but the FTS
  trigger bodies, index shape and `search_index_state` do not.

## 3. Git history: how often does the sink force explorer changes?

Explorer: 43 commits (2026-07-01 → 2026-10-15). Sink: 236 commits, default
branch `development`, no tags, no releases.

| Date | Sink schema event | Explorer reaction |
|------|------------------|-------------------|
| 2026-07-01 | FTS incremental index era (001, pre-Rust) | `b47201b` same day: "Add FTS5 full-text search…" |
| 2026-07-02 | migration 003 era | `4407a7c` "Adapt SQL queries to otel-sqlite migration 003 schema" |
| 2026-08-29 | — | `5b1f671` exposes `severity_number`/`attributes_json` (003 columns) |
| 2026-09-05 | — | `e372ecc` "test: collector schema compatibility coverage" — Mirror B born, self-dated 000+003+004 |
| 2026-09-15 | Rust rewrite settles; 005 drops metrics | (none needed — logs untouched) |
| 2026-11-07 | migrations squashed → `baseline_006.sql` | none yet |
| 2026-11-08 | 006: search index optional, `off`/`body`/`full` + `search_index_state` | none yet — mirror stale, `off`-mode bug unhandled |

Verdict: ~4 of 43 explorer commits (~9%) were directly forced by sink schema
changes, in bursts; the whole compat apparatus (≈600-line mirror + mockdb schema
claim) exists only to compensate for the split. Coupling is real but low-frequency
— the *verification gap*, not the change rate, is the pain.

## 4. Existing cross-repo verification inventory

- `tests/docker/compose.stack-e2e.yml` (sink): full topology nginx → otelcol-contrib
  → sink → explorer, with a `verify` service that drives traffic and polls both the
  DB and `http://explorer:8080/`. **Manual only** — builds the explorer from
  `../../../otel-sqlite-explorer`; the header says it cannot run from a bare CI clone.
- `tests/otel-sqlite-e2e` (sink): in-process e2e harness (`generator.rs`,
  `scenarios.rs`: baseline, backpressure) — already exercised by the sink CI
  `e2e-benchmark` arm. **This is the natural fixture generator for the explorer.**
- Sink CI (9 arms): lint-and-test, crash-recovery-release, e2e-benchmark,
  cargo-deny, cargo-audit, fuzz-smoke, fuzz-nightly, crash-e2e-container,
  container-smoke (release image gate: health SERVING, uid 10001, writable /data).
  No explorer coverage, no image publish job, no tags/releases.
- Explorer: `make test/lint/vet` + lefthook, locally only. **No CI at all.**

## 5. Layout decision framework — do NOT decide this for me

Options: **A** monorepo; **B** polyrepo + a third contract/fixtures repo;
**C** polyrepo + release-asset fixtures + git-subtree glossary + drift CI + pinned
compose (a disciplined status quo).

Weights (adjust to taste): atomicity 3, CI cost 2, iteration speed 2, release
independence 2, single-contributor overhead 2, polyglot tooling friction 2,
fixture/glossary sharing ease 2, explorer's independent generic-UI identity 1.
Scores 1–5.

| Criterion (w) | A monorepo | B third repo | C polyrepo+discipline |
|---|---|---|---|
| Atomic cross-repo change (3) | 5 | 3 | 2 |
| CI cost/complexity (2) | 3 | 3 | 4 |
| Fast UI iteration (2) | 4 | 3 | 4 |
| Release independence (2) | 2 | 4 | 5 |
| Single-contributor overhead (2) | 3 | 2 | 3 |
| Polyglot tooling friction (2) | 2 | 3 | 4 |
| Sharing ease (2) | 5 | 4 | 3 |
| Independent identity (1) | 1 | 4 | 5 |
| **Weighted total /80** | **54** | **51** | **57** |

**Recommendation (yours to reject): C, narrowly over A.**
**Strongest counterargument:** the coupling arrives in bursts — Nov 7–8 moved the
schema twice while this repo was idle for a month — and the existing drift
(Mirror B stuck at "000+003+004") is direct evidence that polyrepo discipline does
not sustain itself for free. A monorepo makes every such burst one atomic commit
with no mirrors to forget. Note also: if the identity concern is dissolved by
keeping the floor contract (`docs/CONTRACT.md`) in a monorepo and documenting the
generic-DB support story, A's identity score rises to 4 and **A ties C at 57** —
the decision then genuinely reduces to how much you value release independence
versus atomicity.

## 6. Fast iteration loop design

Principle: separate slow fixture **production** (sink-owned, cached, pinned by
schema version + index mode) from the fast **consume** loop (explorer, seconds).

- `make fixtures`: run the sink's own e2e harness per `[search] mode`
  (`cargo run --release -p otel-sqlite-e2e --bin otel-sqlite-e2e -- --scenario baseline`,
  config overridden per mode) → `.fixtures/otel-logs.{body,full}.db`; for `off`,
  reconcile a body fixture with mode=off or drop the triggers+rows in the
  generation step (implementation detail to settle in the tracer bullet).
  Cache key: sink git rev + mode. Cold cost ≈ one cargo release build
  (minutes, amortized); warm cost ≈ 0 (skip).
- `make serve-fixtures` (`go run ./cmd/server -db .fixtures/otel-logs.body.db`):
  seconds. `make test-fixtures`: `go test ./...` wired to the fixture set.
- **Budget (target, to be measured at tracer-bullet time):** warm edit→verified
  < 60 s; cold fixture regeneration ≤ 10 min, rare.
- `mockdb` is demoted to what it actually is — a synthetic *data* generator — and
  must stop creating its own schema/FTS shape (drift source). Compat tests stop
  using hand-copied DDL and consume fixtures or the canonical baseline instead.
- `compose.stack-e2e.yml` stays the periodic full-pipeline check; see §10 (6).

## 7. Fixture/image strategy — costs and trade-offs **[OPEN D-3]**

Ground fact: the sink already builds its production image in
`container-smoke.sh` but has **no tags, no releases, and no publish job**; you do
not need any third-party sqlite image — everything below publishes *your own*
artifacts under `ghcr.io/nicknad`.

| Channel | Setup | CI cost per run | Notes |
|---|---|---|---|
| (a) GHCR image `ghcr.io/nicknad/otel-sqlite:<tag>` + fixture images `…:schema-006-body` | one workflow edit in the sink release process (GITHUB_TOKEN, ~30 lines) + tagging | seconds (pull/extract) | hermetic, digest-pinned, also fixes the compose file's sibling-checkout problem |
| (b) GitHub **release assets** (`.db.gz` attached to sink releases) | tagging + one upload step | seconds (download) | no registry auth for public repos; needs the tag/release process you lack anyway; size limit 2 GB per asset — ample |
| (c) build-from-source in explorer CI (cargo build sink) | none | 2–10 min every run | slowest; no pinning unless you pin a ref |

Recommendation (open): start with **(b)** — you need sink tags for any versioned
compatibility policy anyway — and graduate to **(a)** when the stack compose gets
promoted into CI. (c) only as a fallback for hermetic paranoia.

## 8. Glossary sharing — mechanisms **[OPEN D-4]**

Ground facts: the sink's `GLOSSARY.md` is canonical (30 terms, `_Avoid_` lists,
spelling rules) and enforced by `crates/otel-sqlite-core/tests/glossary.rs` — a
*shape* test (required terms present, no duplicates), **not** a prose scan. This
repo uses banned terms today ("log event" at `README.md:32`, `mockdb/main.go:45,344,483`,
`collector_compat_test.go:157`, `integration_query_test.go:77,107`,
`handlers_test.go:20`) and its UI copy claims FTS matches "body & service"
(`logs.html:31`) — true only in legacy `full` mode.

| Mechanism | Cost | Drift risk |
|---|---|---|
| (a) git subtree (sink owns; explorer `git subtree pull`) | two commands, no new repo | low; subtree merges can be messy but are explicit |
| (b) synced copy + CI equality check (fetch raw file from sink default branch, byte-compare) | one small job | explicit staleness signal; simplest |
| (c) tiny published docs package | new repo/publish ceremony for one file | overkill |
| (d) monorepo path | free | none — only meaningful under layout A |

Go-side enforcement (mirrors glossary.rs, plus a prose scan): parse
`**Term**:`/`_Avoid_:` entries; assert REQUIRED_TERMS; scan `*.md`,
`*.html`, and non-test `.go` comments for banned terms, with an allowlist for
schema identifiers (`log_event`, `logs_fts`) and the engine name "FTS5". The
glossary governs prose; it must not rename the frozen schema.

## 9. Forge

GitHub is canonical for both repos (remotes + push activity). Codeberg hosts a
`master`-branch copy of the sink untouched since 2026-09-15. This repo's
`CONTRIBUTING.md:16` currently tells contributors to open PRs on Codeberg.
Actions independent of the layout decision: update the five Codeberg links;
then archive Codeberg — decided 2026-11-19: GitHub only, archive, no mirror.

## 10. Verification plan — the CI jobs that prove each claim

Explorer repo (new `.github/workflows/ci.yml` — none exists today):

1. **build-and-test** — `go vet`, golangci-lint (config already present),
   `go test ./...`. Baseline.
2. **fixture-compat matrix** — for each fixture {006 × off/body/full}:
   `db.Open` + floor validation + scan/filter/detail queries; FTS behavior per
   mode: `off` → substring fallback asserted (this job goes red on today's
   `HasFTS` bug until §11 step 3 fixes it); `body`/`full` → match + bm25 join.
3. **drift-detect** (daily cron + dispatch) — fetch the sink's
   `baseline_006.sql` at a pinned ref, apply it in-process via the repo's own
   `modernc.org/sqlite` (no python, no docker), and compare object set, `logs`
   view columns, FTS shape and `search_index_state` against a checked-in
   expectations snapshot. Red = sink DDL moved without explorer sign-off. This is
   the job that makes Mirror A/B unnecessary.
4. **sabotage canary** — an in-repo test that mutates a fixture (renames one floor
   column) and asserts `validateReadModel` *fails*: proves the suite can fail.
5. **glossary test** — §8 enforcement.

Sink repo (additions to its existing CI):

6. **stack-e2e promotion** — make `compose.stack-e2e.yml` hermetic by building the
   explorer from a pinned git ref or image tag instead of the sibling path; then
   run it nightly or on dispatch. Proves the full round-trip incl. explorer HTTP.
7. **release fixture publish** — §7 channel of your choice, tagged by schema
   version (requires bootstrapping sink tags).

The FTS-absent job is row 2's `off` cell — part of the default matrix, not a
special case.

## 11. Ordered implementation plan (tracer bullet first)

Each step leaves the repo green. Steps 1–5 are **layout-agnostic** — they pay off
under monorepo *or* polyrepo, so the layout decision gates nothing.

1. **Tracer bullet (≈half a day):** generate one real sink DB via
   `cargo run --release -p otel-sqlite-e2e --bin otel-sqlite-e2e -- --scenario baseline`
   (check `--help` for its DB output path), copy it in, and add one Go test:
   `db.Open` → floor columns OK → one FTS match → then a second fixture with
   triggers/mode-off (drop triggers in the test setup) → assert substring
   fallback. This single test verifies a real sink → explorer round-trip and will
   surface the `off`-mode bug immediately.
2. **Contract handshake** — client.go reads `schema_migrations` (warn on unknown
   versions) and `search_index_state` when present (FTS capability = mode +
   triggers probe fallback).
3. **Fix the `off`-mode fallback bug** + UI copy ("body & service" → mode-aware).
4. **Explorer CI** jobs 1–2 (fixtures may be committed `.db.gz` initially;
   switch to release assets per D-3 later).
5. **Drift-detect + sabotage canary** (jobs 3–4).
6. **Glossary sync** per D-4 + Go enforcement test + fix the eight "log event"
   spots + Codeberg link fixes (D-5 in parallel).
7. **Layout ADR** — decide D-1/D-3/D-4/D-6 using §5's matrix; write
   `docs/adr/0001-repo-layout.md` recording the call and the rejected alternative.
8. **Post-decision polish** — mockdb demotion, `make fixtures/serve-fixtures/test-fixtures`
   targets, measure the §6 budget, then (optionally) the §10(6) stack promotion.

## 12. Open decisions (all yours)

| # | Decision | Where discussed |
|---|----------|-----------------|
| D-1 | Compatibility policy: floor-only vs verified-current vs N-1 vs lockstep | CONTRACT §5 |
| ~~D-2~~ | FTS-off detection — **resolved 2026-11-20: trigger-maintained fts5 is the capability; state row deferred to the UI-copy pass** | CONTRACT §3 |
| D-3 | Fixture channel: release assets vs GHCR image vs build-from-source | §7 |
| D-4 | Glossary mechanism: subtree vs synced copy+CI vs monorepo path | §8 |
| ~~D-5~~ | Codeberg: archive or mirror — **resolved 2026-11-19: archive** | §9 |
| D-6 | Repo layout: monorepo vs polyrepo+discipline (matrix weights adjustable) | §5 |
