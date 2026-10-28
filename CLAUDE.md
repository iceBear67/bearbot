# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

bearbot connects the pi coding agent (`@earendil-works/pi-coding-agent` 1.4.0) to Telegram, QQ and a local terminal console. Administrators ask the agent in chat to extend itself by writing pi extensions, skills and MCP configuration under the workspace `.pi/`. Each change is checked, snapshotted with git and applied by restarting the agent process. Failures are repaired by a separate recovery agent, and administrators can roll back. One container runs one agent and one workspace.

`DESIGN.md` describes the implementation exactly, and code comments cite its sections (for example "DESIGN 5.12"). When code behavior changes, update the matching DESIGN.md section in the same change. `DESIGN_MULTI_WORKSPACE.md` and `DESIGN_STALE.md` are abandoned designs; do not use them. The WeCom adapter (`packages/chat-adapter-wecom/`, DESIGN 6.2.3) is not implemented, and config validation rejects `[platforms.wecom] enabled = true`.

## Commands

Run these from the repository root unless noted. Install dependencies with `npm ci`.

| Task | Command |
|---|---|
| Build gateway (`gateway/dist/main.js`; the Go e2e tests need it) | `npm run build -w gateway` |
| Typecheck gateway and its tests | `npm run typecheck -w gateway` |
| Typecheck bearbot-im, SDK, recovery | `node_modules/.bin/tsc -p extensions/tsconfig.json` and `node_modules/.bin/tsc -p extensions/bearbot-recovery/tsconfig.json` |
| Gateway tests | `npm test -w gateway` (a single file: `npm test -w gateway -- test/<name>.test.ts`) |
| Extension tests | `node_modules/.bin/vitest run extensions` (a single file or test: `node_modules/.bin/vitest run extensions/bearbot-im/test/e2e.test.ts -t "<name>"`) |
| Go vet and all Go tests | `cd daemon && go vet ./... && go test -count=1 -timeout 40m ./...` |
| A single Go test | `cd daemon && go test -count=1 -run '^TestName$' ./e2e/` (or `./internal/<pkg>/`) |
| Regenerate drizzle migrations | `npm run db:generate -w gateway` |

- The `daemon/e2e` suite takes about 11 minutes, which exceeds the default `go test` timeout of 10 minutes, so always pass `-timeout`.
- The gateway console tests need util-linux `script` and `tmux`. The Go and TS test environments symlink `/usr/bin/python3.14`, `/usr/bin/uv` and `/usr/bin/uvx`.
- Run `gofmt` on Go changes.
- Tests that need external services skip when their environment variables are missing:
  - OpenAI-compatible model: `BEARBOT_TEST_OPENAI_BASE_URL`, `_API_KEY`, `_MODEL` (written as provider `testprovider`)
  - Telegram: `BEARBOT_TEST_TELEGRAM_BOT_TOKEN`, `_CHAT_ID`
  - QQ: `BEARBOT_TEST_QQ_APP_ID`, `_CLIENT_SECRET`

### Testing rules

All tests use real components: real `bearbotd`, gateway, pi processes, git, uv and npm. Do not use stand-in adapters or servers, `vi.fn()`, or module replacement. Tests run as a normal user through rootless mode:

- `bearbotd --root <dir> --rootless <subcommand>` prefixes every path with `<dir>`. With `--rootless`, children run as the current user and ownership is never changed.
- `daemon/internal/testenv` builds the test root under `/tmp`, because unix socket paths are limited to 108 bytes. It symlinks the repository's gateway, extensions, sdk, skills, recovery-profile and `node_modules` pi into the root and writes `bearbot.toml` from a template.
- `extensions/bearbot-im/test/daemon-env.ts` is the TypeScript copy of the same environment. When you change templates or symlinks in one, change the other too.

## Architecture

### Processes

`bearbotd` (Go, runs as root) is the container entry and starts every other process. Children switch to a different user through `SysProcAttr.Credential`. Every child starts with `setsid`, so none has a controlling terminal.

| Process | User | Role |
|---|---|---|
| gateway | `bearbot-gw` | The only holder of the chat-sdk `Chat` object. It stays connected to the IM platforms while the agent is broken or stopped. |
| agent process | `bearbot` | `pi --mode rpc` with the `bearbot-im` extension loaded via `-e`. |
| trial run | `bearbot` | A temporary pi started during a check. |
| recovery | `bearbot` | pi with its own profile and the `bearbot-recovery` extension. |

The sockets live in `/run/bearbot/`:

| Socket | Protocol | Connection |
|---|---|---|
| `gateway.sock` | newline-delimited JSON-RPC | `bearbot-im` → gateway. Carries messages, posts and history. |
| `gateway-ctl.sock` | newline-delimited JSON-RPC | daemon ⇄ gateway. Both sides send requests: status, `/bb` command actions, tokens, notifications, agent state. |
| `agent.sock` | HTTP/JSON | `bearbot-im` and recovery → daemon. Carries check, apply, snapshot restore, the classifier turn record and recovery reports. Which endpoints are accepted depends on the current daemon state. |
| `admin.sock` | HTTP with NDJSON responses | `bearbotd` CLI subcommands → daemon. |

The daemon also talks to the agent over pi's stdin/stdout RPC: `get_state` for heartbeats, `abort`, `compact`, and the event stream.

Every node script, including pi, tsc and npm, starts as `<node> <script>`. `<node>` is resolved from the daemon's PATH, because the PATH of the children begins with the agent-writable `/bearbot/profile/bin`. pi processes run with `JITI_FS_CACHE=false`. Keep both rules when you add new child processes.

### Daemon (`daemon/`)

- `cmd/bearbotd/run.go` holds the startup order and shutdown (DESIGN 5.2). `main.go` defines the cobra commands and the hidden `__snapshot scan|restore` helper.
- `internal/agent` owns the state machine (DESIGN 5.4) and runs every state change on a single lifecycle goroutine:
  - The states are STARTING, RUNNING, DRAINING, RESTARTING, INCIDENT, REPAIRING, MAINTENANCE and ROLLING_BACK.
  - `incident` (repair flow and maintenance) and `rollback` reach the state and the process through the `Host` interfaces of `agent`.
  - A change lock serializes check, apply, snapshot restore, rollback and the repair commits.
- `internal/state` holds `state.json`, which is written atomically and rejects unknown fields. Its fields include `runningSnapshot`, `lastGoodPending` and `probationUntil`, which drive probation: whenever `runningSnapshot` changes, set `lastGoodPending`, and the next start then enters a probation window of fixed length. At the end of the window, `last-good` moves to `runningSnapshot`.
- `internal/snapshot` manages the bare repo `profile/bearbot/snapshots.git`:
  - Root never opens workspace files. It runs `bearbotd __snapshot scan` and `restore` as user `bearbot`, imports the returned pack with `unpack-objects --strict`, validates every entry, and builds trees and commits itself.
  - Snapshot refs: `init`, `apply/<n>`, `incident/<id>/...`, `rollback/<n>/before|after`, `resume/<n>`, `last-good`. Refs are validated with a regular expression, and git calls pass `--end-of-options`.
- `internal/check` runs the location check, the tsc type check and the trial run. `internal/classifierturn` compares `CLASSIFIER.md` after each member turn and restores it if the turn changed it. `internal/wire` holds the JSON types shared between packages.

### Gateway (`gateway/src/`)

- The daemon generates `/run/bearbot/gateway.json` from `bearbot.toml`; the gateway only reads that file.
- The gateway receives all messages through `onDirectMessage`, `onNewMention` and `onNewMessage(/[\s\S]*/)` with the `concurrent` strategy. Handlers only write to SQLite and return; queueing, merging and debouncing happen in `bearbot-im`.
- Data is stored in SQLite through drizzle (migrations in `gateway/drizzle/`), and `state/sqlite-state.ts` is the chat-sdk `StateAdapter` on the same database.
- `commands.ts` handles `/bb` commands, `tokens.ts` the admin tokens, and `console-adapter.ts` the console platform. The console runs on file descriptors 3 and 4, which the daemon hands over from its own stdin and stdout. Its sender is the built-in administrator `console:operator`.
- `safe-open.ts` opens workspace files without following symlinks.

### bearbot-im (`extensions/bearbot-im/`)

- `bridge.ts` connects to the gateway and the daemon.
- `pipeline.ts` runs subscribers and middleware.
- `dispatch.ts` keeps one queue for all bound channels and hands messages to `pi.sendUserMessage`. It tracks the dispatched turn itself, because a rejected dispatch is reported only as an `extension_error` on `<runtime>`/`send_user_message`, which the daemon forwards.
- `classifier.ts` gates tool calls in `tool_call` for member turns.
- Other extensions get the API over `pi.events` through `sdk/index.ts`. Module state is not shared across extensions, because jiti uses `moduleCache: false`.

### Self-modification flow (DESIGN 9)

1. The agent edits `workspace/.pi/`.
2. `bearbot_check` runs the location check, the type check and the trial run.
3. `bearbot_apply` commits the `apply/<n>` snapshot and restarts the agent with the same session id after the current turn.
4. The new process runs a confirmation turn and then enters probation.

If the agent exits inside probation or fails to start, the daemon opens an incident. The incident goes to recovery, then to a check, then to a restart in probation. When recovery cannot fix it, the daemon enters maintenance; administrators then run `/bb repair`, `/bb resume` or `/bb rollback`. Rollback is handled by gateway and daemon only, so it works even when the agent's own extensions lock it up.
