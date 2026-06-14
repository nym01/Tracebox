# Tracebox — Phase 5 Notes (Run Persistence & Audit Trail)

Phase 5 turns the per-run structured stdout logs (Phase 6) and eBPF trace events
(Phase 4a/4b/4c) into a **queryable audit trail**: completed runs and their trace
events are persisted to SQLite, and two read endpoints expose them.

This is **purely additive**. The existing stdout JSON logging
(`emitRunLog`/`emitTraceEvents`) is unchanged and still useful for live tailing;
SQLite is a second, durable sink. `/run`, `/healthz`, `/readyz`, and `/info`
behavior and response shapes are untouched.

## What was added

- `internal/store` — SQLite-backed persistence (`Open`, `SaveRun`, `GetRun`,
  `ListRuns`). All methods are nil-safe so the rest of the code wires it
  unconditionally, the same way the eBPF tracer degrades when unavailable.
- `internal/api/runs.go` — `persistRun` (called alongside the existing stdout
  logging at both run-completion points) and the read handlers.
- Endpoints:
  - `GET /runs/{run_id}` — full audit record for one run; `404` (`run_not_found`)
    if unknown.
  - `GET /runs?limit=N` — recent runs (newest first; default 50, capped at 200)
    for browsing without a specific id.
- `cmd/tracebox/main.go` — opens the store at startup (non-fatal on failure).
- `docker-compose.yml` — `TRACEBOX_DB_PATH=/data/tracebox.db` on a named
  `tracebox-data` volume so the trail survives container restarts.

## SQLite driver choice — `modernc.org/sqlite` (pure Go)

The binary is built with **`CGO_ENABLED=0`** (see `Dockerfile`): the eBPF program
is embedded via `bpf2go` specifically so the build needs no cgo. The cgo-based
`mattn/go-sqlite3` would force cgo back on for the entire build, complicating the
static Linux build. **`modernc.org/sqlite` is pure Go**, requires no C toolchain,
and works under `CGO_ENABLED=0` on both the Linux container and a Windows dev
machine. It registers under the driver name `"sqlite"`.

## Schema

```
runs(
  run_id PK, language, status, exit_code, duration_ms, memory_peak_kb,
  timestamp, source, stdout, stderr, compile_output
)

trace_events(
  id PK, run_id FK->runs.run_id, event, syscall,
  path, argv, dest_ip, dest_port, timestamp
)
  index idx_trace_events_run_id on (run_id)
```

Design decisions:

- **`runs` captures more than the stdout log line.** The Phase 6 log line has
  `run_id, language, status, exit_code, duration_ms, timestamp`. An audit trail
  should record *what code produced what result*, so the row also stores the
  submitted `source`, the run `stdout`/`stderr`, and the build phase's
  `compile_output`. These are available in the handler where `emitRunLog` is
  called (`req.Source`, the test results, `buildResult`) but were never logged.
- **Aggregation across tests.** A request can carry multiple tests. The single
  `runs` row stores `stdout`/`stderr` as the per-test outputs concatenated (in the
  common single-test case this is exactly that test's output), `memory_peak_kb`
  as the max across tests, and `compile_output` as the build phase's combined
  stdout+stderr (empty for interpreted languages). Per-test granularity remains
  available in the `/run` HTTP response; the trace events themselves are per-run,
  not per-test.
- **`trace_events`: typed columns, not a JSON blob.** The type-specific fields are
  a small fixed set (`path` for file_open/exec, `dest_ip`/`dest_port` for connect),
  so nullable typed columns are cleaner and directly queryable
  (e.g. "all runs that connected to port 53") than an opaque JSON payload.
  The one genuinely list-shaped field, exec `argv`, is stored as a JSON text
  column and round-tripped back to a string array on read. Inapplicable columns
  store `NULL`.

## Design-question flag: unbounded database growth

> **Flagged per the Phase 5 task.** Storing full `source`, `stdout`, `stderr`,
> and `compile_output` for every run, plus one `trace_events` row per syscall,
> means the database **grows without bound** and per-run rows can be large.

Concretely:

- Input is already bounded by request validation: `source` ≤ 256 KiB, each test
  field ≤ 64 KiB, ≤ 50 tests. So a single run's persisted text is bounded, but
  potentially large (low-MB worst case).
- Trace events are **not** bounded by request size — they reflect what the run
  actually did. A compile-heavy run produces hundreds of rows (the C connect
  verification run stored 584 events: 576 file_open + 7 exec + 1 connect).
- Nothing prunes or rotates the database. Over many runs it grows monotonically.

**Decision for this project's scope (portfolio/demo, not production
multi-tenant): store everything, no truncation or limits, accepted as a known
scaling consideration.** This keeps the audit trail complete and the code simple,
which matches the demo goal. It is documented here rather than silently shipped.

Natural follow-ups if this ever needed production hardening (explicitly **out of
scope** now): a retention policy (TTL or max-row cap with eviction of oldest
runs), per-field truncation with a stored "truncated" marker, a periodic `VACUUM`,
and a row/byte cap on `trace_events` per run.

## Persistence across restarts

`docker-compose.yml` mounts a named volume `tracebox-data` at `/data` and points
`TRACEBOX_DB_PATH` there, so the audit trail survives `docker compose
down`/`up` (volume removal still wipes it). A bare `docker run` without the volume
keeps the DB **container-local/ephemeral** — fine for one-off verification, lost
when the container is removed.

## Verification (live container, rebuilt image)

- `go build ./...`, `go vet ./...`, `go test ./...` — all pass.
- Escape suite `go test -tags escapetests ./escapetests/...` — **21/21 pass**,
  0 failures; all 7 languages still functional (`/readyz` reports each).
- `/healthz`, `/readyz` — OK after rebuild.
- py3 hello-world via `/run` → `GET /runs/{id}` returned source, stdout, status,
  and file_open trace events.
- C `connect()` to `8.8.8.8:53` → stored `trace_events` included the `connect`
  event with `dest_ip=8.8.8.8 dest_port=53`, plus exec events with full argv.
- `GET /runs/{nonexistent-uuid}` → `404 run_not_found`.
- `GET /runs` → recent runs, newest first.
- CORS: `WithCORS` wraps the whole mux, so the new endpoints automatically carry
  the CORS headers and answer `OPTIONS` preflight (verified: `204` + headers).
