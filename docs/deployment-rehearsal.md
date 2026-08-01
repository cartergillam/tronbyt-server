# Local deployment rehearsal

This environment is isolated from production. It builds the current server
tree, persists SQLite and rendered WebPs in a named Docker volume, mounts the
adjacent `tronbyt-apps` checkout read-only, and uses fixed development-only API
keys:

- device: `rehearsal-device-key`
- user: `rehearsal-user-key`
- device ID: `rehearsal-display`
- server: `http://127.0.0.1:18000`

Never reuse these values outside the local rehearsal.

## Lifecycle

Run commands from the server repository:

```sh
./scripts/rehearsal.sh start
./scripts/rehearsal.sh seed
./scripts/rehearsal.sh inspect-migration
./scripts/rehearsal.sh poll --count 5 --interval 15s
./scripts/rehearsal.sh load --duration 60s --poll-interval 15s
./scripts/rehearsal.sh backup
./scripts/rehearsal.sh restore deploy/rehearsal/backups/<archive>.tar.gz
./scripts/rehearsal.sh stop
```

`reset` is intentionally destructive and removes only the Compose project's
named rehearsal volume. Backups stop the rehearsal server first and omit the
read-only local apps checkout. Restore rejects absolute paths, traversal, and
attempts to replace the apps mount; the server is kept stopped until extraction
finishes.

`seed` creates Clock and MLB installations with a synthetic Toronto-timezone
location and 1,000 representative custom-repository catalogue summaries. The
summaries have deterministic local icons and do not call external services.
Clock and MLB themselves resolve from the adjacent apps checkout.

## Poll and load evidence

The poller authenticates with the development device key and records JSON lines
containing HTTP status, selected app/installation headers, response bytes,
SHA-256 prefix, and latency. It fails immediately on authentication rejection
and flags a frame that remains unchanged beyond the configurable stale limit.

The load command keeps the poller active while four workers repeat searches,
six workers request icons, and one worker refreshes diagnostics. It requires at
least 1,000 catalogue summaries and reports request count, errors, error rate,
p50, p95, and maximum latency for each traffic class. It also verifies that the
development `/metrics` and goroutine profile endpoints remain reachable.

Release guardrails are:

- zero expected poll errors and no traffic class above a 1% error rate;
- physical-poll p95 below 2 seconds and maximum below 5 seconds;
- catalogue, icon, and diagnostics p95 below 2 seconds;
- no authentication rejection, deadlock, panic, or continuously growing
  goroutine/SQLite-wait signal in the captured server metrics and logs.

The process exits non-zero when an automated latency or error guardrail fails.
Review `docker compose ... logs web`, `/metrics`, and the development-only pprof
endpoint before accepting a run. Docker and its Compose plugin are required;
this repository does not install them.

## Rollback rehearsal

Create a backup, make the candidate change, start the server, wait for health,
run the synthetic poll and diagnostics checks, then stop the server and restore
the backup. Treat the SQLite database and WebP files as one rollback unit. The
apps checkout is versioned separately and is never included in the archive.
