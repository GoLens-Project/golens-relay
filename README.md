# golens-relay

[![tests](https://img.shields.io/github/actions/workflow/status/GoLens-Project/golens-relay/ci.yml?branch=master&label=tests)](https://github.com/GoLens-Project/golens-relay/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/codecov/c/github/GoLens-Project/golens-relay)](https://codecov.io/gh/GoLens-Project/golens-relay)
[![release](https://img.shields.io/github/v/release/GoLens-Project/golens-relay)](https://github.com/GoLens-Project/golens-relay/releases)

A tiny Go CLI that runs TCP relays from `127.0.0.1:<local-port>` to `<upstream-host>:<remote-port>` on the host. Jailed agents (ai-jail) cannot resolve `.local` mDNS names — their `/run` tmpfs hides the Avahi socket that host-side mDNS resolution depends on — but `localhost` is the one name ai-jail's synthesized `/etc/hosts` always carries. So a jailed agent dials `http://localhost:<local-port>/v1`, and this relay does the `.local` resolution on the host side, where mDNS works.

The upstream name is resolved fresh on every connection (a `net.Dial` per connection), so a DHCP lease change on the upstream host self-heals on the next request — nothing is pinned anywhere. Several relays on different local ports coexist on one host and are started, stopped and logged independently.

The upstream host is currently hardcoded to `tenor.local` (`cmd/relay/main.go:75`); the listen address is always `127.0.0.1`. What varies per relay is the local/remote port pair.

## Install

From source (any platform Go supports):

```sh
go install github.com/GoLens-Project/golens-relay/cmd/relay@latest
```

Or grab the prebuilt linux/amd64 binary from a [GitHub Release](https://github.com/GoLens-Project/golens-relay/releases).

Requires Go 1.27+. Stdlib-only — no runtime dependencies.

## Usage

```
relay start <local-port> <remote-port> [--log=true|false] [--log-days=N]
relay stop <local-port> <remote-port>
relay restart <local-port> <remote-port>
relay status <local-port> <remote-port>
relay status all
relay serve <local-port> <remote-port> [--log=true|false] [--log-days=N]
relay version
relay completion [bash|zsh]
```

### Start a relay

```sh
relay start 8080 4000
# started (pid 4321, listening on 127.0.0.1:8080 -> tenor.local:4000)
```

`start` detaches a daemon (its own session, survives the shell) and returns once the listener is actually up and the pidfile is written — a lost port bind surfaces as a failure, not a false success. It is idempotent: starting an already-running relay is a no-op with exit 0. The daemon's stdout/stderr go to its log file.

Flags (accepted by `start` and `serve` only):

- `--log` — write the daemon file log (default `true`). With `--log=false` the daemon produces no log file.
- `--log-days` — log retention in days (default `7`). The daemon prunes its log hourly, keeping the last 7 days in a sliding window. Must be 1–65536.

### Lifecycle

`stop` sends SIGTERM (the daemon closes the listener, removes its own pidfile and exits), waits 3 seconds, then SIGKILL. It is idempotent, and it cleans up a stale pidfile (one whose pid is gone) instead of failing. `restart` is stop + start and is safe in either state.

```sh
relay stop 8080      # stopped (pid 4321)
relay restart 8080   # stop + start in one call
```

### Status

```sh
relay status 8080
# running (pid 4321), listening on 127.0.0.1:8080 -> tenor.local:4000
```

`status all` scans the state dir and prints one status line per known relay (every relay that was ever started on this machine), exiting 1 if any is broken — a live daemon whose listener no longer answers:

```
8080: running (pid 4321), listening on 127.0.0.1:8080 -> tenor.local:4000
11434: not running
```

### Serve in the foreground

`relay serve 8080 4000` runs the same engine in the foreground (no pidfile detach) — useful under systemd or for debugging. `start` is just a detached `serve`.

### Shell completion

```sh
eval "$(relay completion bash)"   # or: eval "$(relay completion zsh)"
```

Completion covers subcommands, the `--log`/`--log-days` flags on `start`/`serve`, known local ports (from the state dir) as the first positional, and the typed relay's recorded remote port as the second. `status` additionally completes `all`. Completion is read-only — it never touches daemons or state.

### Version

```sh
relay version      # or: relay -v / relay --version
```

Release binaries carry the tag they were built from; source builds report `dev`.

## State and logs

State lives in `/tmp/layer-cli` (a historical path kept so existing pidfiles stay valid): one `relay-<local-port>.pid` / `relay-<local-port>.log` pair per relay. The pidfile records `<pid> <local-port> <remote-port>`, which `status all` and completion read to know each relay's mapping. Note `/tmp` is wiped on reboot — daemons don't survive it, and stale pidfiles are cleaned up automatically on the next `stop`, `status`, or `start`.

## Building and testing

```sh
go build -o /dev/null ./...   # compile check
go test ./...                 # test suite
make test                     # same as go test ./...
```

To stamp a local build with a version:

```sh
go build -ldflags "-X main.version=vX.Y.Z" -o relay ./cmd/relay
```

## Releases

A pushed `vX.Y.Z` tag triggers the `release` workflow, which builds the linux/amd64 binary with the tag stamped as the version and attaches it to a GitHub Release with generated notes.

## Design and status

`DESIGN.md` holds the implementation design. The shipped code is the single-upstream, port-pair bootstrap documented above; everything below is **planned, not implemented**.

### Planned: multi-alias expansion

The bootstrap hardcodes one upstream (`tenor.local`) and identifies relays by port pair. The expansion replaces that with **N named relays ("aliases")**, each a `Relay` value carrying its own alias, host, and local/remote port pair, stored in a YAML config at `~/.config/golens-relay/config.yaml` (written atomically; edited by `add`/`set`/`remove`):

```go
type Relay struct {
    Alias      string // unique key, ^[a-z0-9][a-z0-9-]*$ (it becomes the pid/log filename)
    Host       string // upstream hostname, e.g. tenor.local
    LocalPort  int
    RemotePort int
}
```

Planned CLI surface (aliases replace port pairs; `-c <path>` selects an alternate config):

```
relay add    <alias> --host <host> --local <port> --remote <port>
relay set    <alias> [--host <host>] [--local <port>] [--remote <port>]
relay remove <alias>
relay list [-t|--table]
relay start|stop|restart|status   <alias>|all
relay serve <alias>
relay logs  <alias>
```

Semantics that go beyond the bootstrap: `add` refuses an existing alias instead of silently overwriting; `remove` refuses while the relay is running (explicit over implicit — no surprise daemon kills); `all` fans out over every alias in the CLI as a plain loop, exiting 1 if any relay failed; `list` renders every defined relay, running or not, as readable info clusters by default or a compact table with `-t`; `logs <alias>` prints the last 50 log lines. The relay engine itself (per-connection fresh DNS, bidirectional copy, half-close, log pruning) is unchanged.

Also planned with the expansion:

- **Cobra CLI** — the only non-config dependency. Earned specifically by dynamic completion (per-shell `ValidArgsFunction` machinery would be a few hundred lines of hand-owned shell script) and help templates; engine/daemon/config stay stdlib-only. Dynamic completion proposes `all` plus every configured alias, filtered by the typed prefix, read-only over the config.
- **XDG state dir** — runtime state moves from the historical `/tmp/layer-cli` to `~/.local/state/golens-relay/<alias>.{pid,log}`. `/tmp` is wiped on reboot, throwing away logs that are the only diagnostic when a daemon dies at startup. (This README documents the `/tmp` path because that is what the shipped code uses.)
- **Single-config lookup** — daemon lookup switches from "scan the state dir for pidfiles" to reading the config, with `-c` support; config write is crash-safe via temp-file + rename.

### Planned: CI and release expansion

- **`check` workflow** (planned; only `ci.yml` and `release.yml` ship today — `ci.yml` runs the test suite with coverage on PRs and uploads the profile to Codecov): build + `go vet`, `go test -race`, coverage gates (every `internal/*` package ≥80%, repo total ≥75%), cyclomatic complexity gates (≤15 per function, at most 2 repo-wide exceptions), and semver validation of `v*` tags.
- **Automatic release on merge to main** (planned; release is currently a manual `v*` tag push): the bump level derives from the merged branch prefix — `patch/*` → PATCH, `minor/*` → MINOR, `major/*` → MAJOR; other prefixes don't release. The workflow re-runs the preflight test and never tags while it is red.

### Package layout (with the expansion)

The bootstrap is a single `cmd/relay/main.go`. The expansion splits it into `internal/config` (pure data + file I/O), `internal/daemon` (pidfiles and signals), `internal/relay` (the socket engine), and `internal/cli` (cobra wiring) behind a thin router — logic in internals, `main` only dispatching, no cross-package state mutation. `pkg/` stays reserved for genuinely reusable pieces; nothing qualifies today.

## License

[MIT](LICENSE)
