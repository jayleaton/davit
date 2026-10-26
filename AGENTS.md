# davit — contributor and agent guide

Open-source, self-hosted fast VMs for AI agents on your own hardware: Firecracker microVMs on a Linux host, reached
over SSH or Tailscale.

Design sources: `docs/research/SPEC.md` (product/architecture contract), `docs/research/SPIKE.md` (measured
Firecracker numbers), `docs/architecture.md`.

## Standards and checks (run before every commit/PR)
- Go version pinned in `mise.toml` (`mise exec -- go ...`).
- `gofmt -l .` must print nothing.
- `go vet ./...`
- `go test ./...`
- `make check` is the single entry point and must stay green: gofmt, `go vet` (linux + darwin/arm64), staticcheck
  (pinned as a `tool` in go.mod: `mise exec -- go tool staticcheck ./...`; also run for darwin/arm64 through a
  host-built copy in .build/tools, so darwin-tagged code is linted), shellcheck over images/ packaging/
  scripts/ (pinned v0.11.0 via `scripts/shellcheck.sh` into .build/tools), actionlint, gitleaks over the tree, a
  Markdown link check,
  `go test` and CGO-free cross builds. New lint findings fail the work.
- Secret scanning: `.gitleaks.toml` (gitleaks defaults plus davit and Tailscale token rules). CI runs gitleaks over
  the full history and the tree. Allowlist only an exact synthetic fixture, never a directory.
- `make check` builds and tests two packages at a time (`DAVIT_CHECK_PROCS`, default 2; raise it on an idle machine).
  A step the OS kills for memory is reported as "KILLED BY THE OS (memory pressure) — not a test failure" by
  `scripts/gate.sh` (logs in .build/check-logs/); it still fails the gate and is never retried.
- `make check` is four stages (check-go runs as two shards), each runnable alone. Times: 2026-10-22, AMD Ryzen 7 6800U laptop (16 threads,
  27 GiB, shared with other jobs), `DAVIT_CHECK_PROCS=1`, two samples each (the spread is build-cache state and load):
  ```
  export DAVIT_CHECK_PROCS=1
  make check-lint                 # gofmt, vet + staticcheck linux/darwin, shellcheck (204 s, 24 s)
  make check-go CHECK_SHARD=1/2   # go test, first half of the packages (182 s, 212 s)
  make check-go CHECK_SHARD=2/2   # go test, second half (149 s, 223 s)
  make check-cross                # CGO-free linux/amd64, linux/arm64, darwin/arm64 (25 s, 28 s)
  make check-sdk                  # Python + TypeScript SDKs (53 s, 81 s)
  make check                      # confirms the stamps: seconds once all passed
  ```
  A stage that passes on a clean tree (committed; nothing modified or untracked) leaves a stamp keyed by the tree
  hash in .build/check-stamps/, and `make check` skips stamped stages, so it stays the definition of green. A dirty
  tree never uses or writes stamps; any change to the tree invalidates them. `make check-status` lists what passed;
  `CHECK_FORCE=1` reruns. Unsharded `make check-go` took 429 s on that machine (one sample). Shards come from
  `scripts/check-shard.sh`.
  Stamps are named `<tree>.<stage>.stamp` (never a source extension; `scripts/check-stage-test.sh` checks it in
  check-lint); older suffix-less stamps are ignored, so each stage reruns once after upgrading. Nothing under
  `.build/` is source: fmt-check skips it, and `go vet`/staticcheck never enter dot-directories.
- If your machine has a job lock or a low-priority wrapper for heavy work, set `DAVIT_HEAVY` to that command
  prefix (for example `DAVIT_HEAVY='nice -n 15 ionice -c3'`). `make` test and release steps, `images/build.sh`,
  `images/smoke.sh` and `scripts/host-install.sh` run their heavy steps under it; unset, they run directly.
- The TypeScript suite (`npm test`, sdk/typescript/test/run.ts) runs `DAVIT_TEST_CONCURRENCY` files at a time
  (make check: `DAVIT_CHECK_PROCS`). A file that fails as a whole, every subtest passing, is reported at the end
  with its exit code or signal and any error raised after a test ended, instead of node's bare `'test failed'`.
- Runtime adapters must pass `internal/runtime/runtimetest`; storage implementations the contract in `internal/storage/fs_test.go`.
- `api/openapi_test.go` pins `api/openapi.yaml` to the Go wire types by reflection; change both together.

## Rules
- Keep domain boundaries from SPEC.md §8: each area owns its packages; change shared contracts (domain types, api/openapi.yaml, guest protocol) only in a dedicated PR.
- Real-VM tests (`DAVIT_TEST_FIRECRACKER=1`, `DAVIT_BENCH=1`, `scripts/fresh-host-e2e.sh` on Firecracker) run only on a
  separate VM host through `scripts/vm-tier.sh HOST [TESTS-regex]` (docs/vm-tier.md), never on your development machine.
- Tests and experiments never point git env vars (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, …) at the real repository: use a throwaway repo under TMPDIR.
- No sudo/system installs from tests or scripts without asking.
- Never claim a speed number you did not measure; record hardware and sample counts.
