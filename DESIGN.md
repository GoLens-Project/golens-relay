# golens-relay — Implementation Design

Design session doc for the multi-relay expansion of the layer-cli draft. Nothing here is implemented yet; `main.go` still holds the single-relay draft. Recommended options in the Q&A at the bottom are marked **[R]** and become binding once confirmed.

## 1. Purpose

Unchanged from the draft: TCP relay from `127.0.0.1:<local-port>` to `<host>:<remote-port>` so jailed agents (ai-jail) that cannot resolve `.local` names can dial `localhost:<local-port>` instead. The relay resolves the upstream name fresh on every connection, so DHCP/mDNS changes self-heal.

What changes: the tool goes from one hardcoded relay (`tenor.local`) to N named relays ("aliases"), each with its own host and port pair, managed by a small subcommand CLI, with lifecycle commands that can target one alias or all of them at once.

## 2. Model

An **alias** is a named relay definition:

```go
type Relay struct {
    Alias      string `json:"alias"`       // unique key, filename-safe
    Host       string `json:"host"`        // upstream hostname, e.g. tenor.local
    LocalPort  int    `json:"local_port"`  // 127.0.0.1:<LocalPort>
    RemotePort int    `json:"remote_port"` // <Host>:<RemotePort>
}
```

Rules:

- Alias must match `^[a-z0-9][a-z0-9-]*$` (it is used as pid/log filename).
- Alias is unique in the config; `add` on an existing alias errors (no silent overwrite).
- The listen address is always `127.0.0.1` — not configurable (§13).
- Host is stored per alias at `add` time; DNS still resolves per connection, so "dynamic host" at runtime means editing the stored value (`set`) or remove+re-add.

## 3. Process model

One OS daemon per running alias, same pattern as the draft: `start` spawns `relay serve <alias>` detached (`Setsid`), writing `<alias>.pid`. No shared supervisor, no control socket.

Why not a single supervisor daemon: multiplexing add/remove of listeners at runtime needs an IPC channel between CLI and daemon (socket + protocol + handshake), which is a lot of machinery for what is a handful of relays started once. Per-alias daemons keep start/stop/restart as plain pidfile operations that compose trivially with `all` (a loop), and one crashing relay cannot take down the others.

`all` fan-out is implemented purely in the CLI: iterate the config's aliases in definition order, apply the operation to each, print one result line per alias, exit non-zero if any failed.

## 4. Files

Config (who *should* run):

```
~/.config/golens-relay/config.yaml
```

A YAML list of `Relay` values (Q5 answer: YAML preferred over JSON; parsed with gopkg.in/yaml.v3). Written atomically (temp file + rename) so a crash mid-write cannot corrupt it. No file locking — concurrent CLI invocations are not a supported workflow; the pidfile already serializes duplicate `start` on the same alias.

Runtime state (where daemons put pid/logs):

```
~/.local/state/golens-relay/<alias>.pid
~/.local/state/golens-relay/<alias>.log
```

The draft used `/tmp/layer-cli`; `/tmp` is wiped on reboot, which throws away logs that are the only diagnostic when a daemon dies at startup. XDG state dir fixes that.

## 5. CLI surface

The CLI binary is named `relay`; the module, repo, and XDG directories keep the `golens-relay` name (Q1 answer).

```
relay [-c <config-file>] <command> [args]

relay add    <alias> --host <host> --local <port> --remote <port>
relay set    <alias> [--host <host>] [--local <port>] [--remote <port>]
relay remove <alias>
relay list [-t|--table]
relay start    <alias>|all
relay stop     <alias>|all
relay restart  <alias>|all
relay status   <alias>|all
relay serve    <alias>        # foreground; used by start, systemd-friendly
relay logs     <alias>        # print last 50 log lines (§13)
relay version                                  # print semver of the installed binary (§11)
relay completion [bash|zsh|fish|powershell]    # emit shell completion script (§10)
```

Semantics:

- `add` validates host (non-empty, no port suffix), ports (1–65535), alias charset, uniqueness; writes config. Does **not** start anything.
- `-c <path>` (global flag): use an alternate config file instead of `~/.config/golens-relay/config.yaml` (Q5 answer). Mutating commands (`add`, `set`, `remove`) persist their change to whichever file is in effect — CLI-set values overwrite file-set values, there is no separate runtime layer.
- `set` edits stored fields; refuses a `--local` that collides with another alias's local port only at `start` time (the bind fails there anyway — no pre-flight check, keep it simple).
- `remove` deletes the definition; if the relay is running, it refuses unless the relay is stopped first (explicit over implicit — no surprise daemon kills).
- `start <alias>`: spawn daemon, wait ≤2s for the listener to answer, exactly like the draft; idempotent (already-running → no-op, exit 0).
- `stop`: SIGTERM → 3s grace → SIGKILL; idempotent; cleans stale pidfile.
- `restart`: stop + start, safe in either state.
- `status`: one line per target: `alias: running (pid N), listening on 127.0.0.1:L -> host:R` / `not running` / `stale pidfile ...`.
- `<alias>|all` is a single positional argument; `all` expands to every alias in the config. Operations on an alias not in config exit 2 with `unknown alias "x"` (and print the valid list).
- `list`: default renders one **info cluster** per relay — a `key: value` block that stays readable as relays gain fields; every relay defined in config is shown, running or not (§13):

```
litellm — running (pid 4321)
  listen    127.0.0.1:8080
  upstream  tenor.local:4000

ollama — stopped
  listen    127.0.0.1:11434
  upstream  tenor.local:11434
```

- `list -t` / `--table` switches to a compact aligned table of the same data:

```
ALIAS    STATE     LISTEN           UPSTREAM
litellm  running   127.0.0.1:8080   tenor.local:4000
ollama   stopped   127.0.0.1:11434  tenor.local:11434
```

- Both views are pure rendering over the same typed `Relay` + live-state values (see §7); cluster and table are two render functions, not two data paths.

- Exit codes: 0 success (including idempotent no-ops), 1 operational failure, 2 usage/unknown alias. Under `all`, exit 1 if any relay failed, 0 otherwise.
- `-h` / `--help` works on the root command (overview + command list) and on every subcommand (`relay add -h` shows usage, flags with defaults, one concrete example). Invalid usage prints that subcommand's help to stderr and exits 2, never a bare usage line.
- Tab completion: `eval "$(relay completion zsh)"` installs shell completion; subcommand and flag names complete statically, positional `<alias>` arguments complete dynamically — candidates come from the live config plus the `all` keyword, filtered by the prefix the user already typed. See §10.
- `version` prints the binary's semver (see §11).

## 6. Relay engine

Unchanged from the draft — it is correct and stays as-is:

- `handle`: dial upstream per connection (fresh DNS), `io.Copy` both directions, half-close (`CloseWrite`) on completion, wait for the reverse direction to drain.
- `serve <alias>`: load config, find the alias, bind `127.0.0.1:<local>`, install SIGTERM/SIGINT handler that closes the listener and removes its own pidfile, hourly log prune keeping the last 7 days (sliding window — Q3 answer).
- Daemon must remove only **its own** pidfile on shutdown (it knows its alias; no global pidfile anymore).

## 7. Package layout

Thin router at the top, logic in internal packages, no cross-package state mutation:

```
main.go                  — argv routing to subcommand funcs only; no business logic
internal/cli/cli.go      — command wiring (cobra), help text, dynamic completion funcs
internal/config/config.go — Relay type, Load/Save/Add/Set/Remove, alias validation
internal/daemon/daemon.go — pidfile read/write/clear, alive, Spawn/Terminate, status probe
internal/relay/relay.go   — serve + handle + closeWrite + log prune (the engine)
```

- `config` is pure data + file I/O; knows nothing about processes.
- `daemon` knows pidfiles and signals; takes a resolved `Relay` value, never reads config itself.
- `relay` is the foreground engine; the only package that touches sockets.
- `main` composes them; `all` fan-out lives in `main` as a loop over `config.List()`.

**`internal/` vs `pkg/` policy:** implementation specific to this tool lives under `internal/` (compiler-enforced privacy — nothing outside the module can import it). `pkg/` is reserved for reusable, non-project-specific code — generic utils or integrations that another project could plausibly import — and is used only where it makes sense, not as a default dumping ground. Nothing in the bootstrap qualifies (the relay engine, config, and daemon logic are all domain-specific), so everything starts in `internal/`; if a genuinely reusable piece falls out later (e.g. a standalone TCP half-close proxy or a typed pidfile helper), it moves to `pkg/<name>/` at that moment, with its tests moving alongside.

Dependency policy (§13): stdlib plus exactly two earned dependencies — `spf13/cobra` for the help/completion machinery (§10) and `gopkg.in/yaml.v3` for the YAML config. Both live in `internal/cli` and `internal/config` only; the daemon and relay packages stay stdlib-only.

## 8. Migration from the draft

- Rename module `layer-cli` → `golens-relay` in `go.mod` (see Q1); binary name follows.
  - rename to relay, only in all mentions
- Rewrite `main.go` (it becomes the thin router); lift `handle`/`closeWrite`/prune into `internal/relay`, pidfile logic into `internal/daemon`.
- Old `/tmp/layer-cli` state is abandoned, not migrated: relays get re-`add`ed and re-`start`ed once.

## 9. Testing plan

**Bootstrap status: no tests are written yet.** Everything below is the suites to be created during this first implementation, in order; nothing here is claimed to exist today. The first deliverable is the preflight test — it must pass before any other code lands, and the release flow refuses to tag while it is red.
- make test is built, but contains only a print statement; use it as preflight test

- Preflight (written first, gates everything): the package builds (`go build ./...`), the CLI boots (`relay -h`, `relay version`), an empty config loads cleanly, and one end-to-end relay round-trip succeeds against a local echo upstream; CI fails without it and `release` never tags while it is red.

- `internal/config`: add/set/remove round-trip, alias charset/uniqueness rejection, atomic-write leaves valid JSON on simulated interruption (write to temp path, assert original intact).
- `internal/daemon`: pidfile lifecycle against a scratch state dir (spawn a real `sleep` child), stale-pid cleanup, SIGTERM-then-SIGKILL escalation with a child that ignores SIGTERM.
- `internal/relay`: `go test` integration — spin an in-process `net.Listen` echo upstream, run `serve` on a high port, assert byte round-trip and EOF propagation; assert per-connection re-resolution by pointing host at `localhost` (already the re-resolution path).
- Fan-out: `start all` / `stop all` against 2–3 scratch aliases, assert per-alias lines and exit codes including one deliberately failing alias.
- Completion: the `ValidArgsFunction`s are pure (typed args in → candidate list out) and unit-tested against a scratch config, no shell involved.
- Verification per project rules: `go build -o /dev/null ./...` and `go test ./...`.
- add test coverage; make it reach 80%+ of coverage for functional code; 75%+ overall — enforced by the CI gates job: every `internal/*` package ≥80%, repo total ≥75%
- add complexit check with 15 max + 2 max overall — enforced by `gocyclo` in the CI gates job; the "2 max overall" clause is defined in Q15 (§13)

## 10. CLI ergonomics — help & completion

**Help.** Root help lists every subcommand with a one-liner. Each subcommand's help shows usage, every flag with its default, and one concrete example invocation. Both `-h` and `--help` work everywhere; a usage error prints that subcommand's help to stderr with exit 2 instead of a bare one-line usage string.

**Completion.** Static completion covers subcommand and flag names. Dynamic completion covers positional alias arguments: the candidate set is `all` plus every alias currently in `~/.config/golens-relay/config.yaml`, filtered by the prefix already typed, so `relay start <TAB>` proposes `all litellm ollama` and `relay start li<TAB>` proposes `litellm`. `remove`, `serve`, and `logs` complete aliases only (no `all`). Completion paths are read-only over the config — they must never mutate state or spawn/kill daemons.

**Why cobra earns its place here (decided — §13).** Implementing dynamic completion natively on stdlib means hand-writing per-shell scripts (bash `complete`/`COMP_WORDS` parsing and zsh `compdef` are fundamentally different models) plus a hidden `__complete` wire protocol between the shell script and the binary — a few hundred lines of shell the project would own and could barely test. Cobra ships exactly this machinery (`completion` command for bash/zsh/fish/powershell, `ValidArgsFunction` which receives the already-typed arguments) plus help templates, so the completion and help requirements added in this session are precisely its reason to exist. The dependency is confined to `internal/cli`; the engine, config, and daemon packages stay stdlib-only.

## 11. Versioning, install & release

**go install.** Decided (Q10): module path `github.com/golens/golens-relay`, so `go install github.com/golens/golens-relay@vX.Y.Z` works per the community standard; the Q1 rename sets `go.mod` accordingly. Note that `go install` compiles from source on the target machine — it never downloads a prebuilt binary — and the release ships no binaries either, `go install` is the only install path (Q14, §13).

**Version source of truth.** `runtime/debug.ReadBuildInfo().Main.Version`: binaries installed via `go install module@vX.Y.Z` automatically report `vX.Y.Z`, local `go build` reports `(devel)` — no ldflags injection, no version constant to forget to bump. `relay version` prints it, and root help shows it.

**Tag format.** Strict semver `vMAJOR.MINOR.PATCH` (first release `v0.0.1`). While in 0.x, breaking CLI/config changes bump MINOR and fixes bump PATCH, per Go module convention. Tagging happens through the `release` workflow, not local git (§12, Q16).

## 12. CI (GitHub Actions)

Two workflows (no goreleaser — Q14):

`check` — runs on every push and pull request:
1. build job: `go build ./...` plus `go vet ./...`;
2. test job: `go test ./... -race`;
3. gates job (decided, §9): coverage — every `internal/*` package ≥80%, repo total ≥75% (via `go test -coverprofile` + `go tool cover -func`); complexity — `gocyclo` over all non-test code per Q3; during bootstrap, before those suites exist, the gates job runs the preflight test only and the coverage/complexity enforcement switches on as each suite lands;
4. semver job: on tag pushes (`v*`) it validates the tag against strict semver and that the tagged commit is reachable from main — a malformed or off-main tag fails here.

`release` — the final stage; fully automatic, triggered by push to main (i.e. a PR merge), no manual input (Q16):
1. derives the bump level from the merged branch's prefix — the prefix is the semver level: `patch/*` → PATCH, `minor/*` → MINOR, `major/*` → MAJOR (e.g. `patch/log-window` bumps the last tag's patch digit); any other prefix (`chore/`, `docs/`, ...) → no release (Q16, Q17);
2. validates the computed version is strictly newer than the latest existing tag, and that the workflow runs on main;
3. re-runs build and tests — the preflight test at minimum (§9); never releases while it is red;
4. creates tag `v<version>` (requires `contents: write`) and the GitHub Release with generated notes;
5. no prebuilt binaries and no checksums — install is `go install` everywhere (Q14).

## 13. Decisions (resolved)

Resolved choices; the rationale lives in the sections referenced by the body.

| # | Decision |
|---|----------|
| Q1 | Binary/CLI is named `relay`; module, repo, and XDG dirs keep the `golens-relay` name |
| Q2 | cobra for command wiring (help + completion), gopkg.in/yaml.v3 for config; engine/daemon stay stdlib-only |
| Q3 | State in `~/.local/state/golens-relay/`; logs pruned to a 7-day sliding window |
| Q4 | `list` shows all defined relays with a live STATE field (default cluster view, `-t` table) |
| Q5 | YAML config; global `-c <path>`; `add`/`set`/`remove` persist changes to the active config file |
| Q6 | `all` fan-out exits 1 if any relay failed, 0 only if all succeeded |
| Q8 | `logs <alias>` ships in v1, prints the last 50 lines |
| Q9 | Aliases only — no ad-hoc unnamed relays |
| Q10 | Module path `github.com/golens/golens-relay`, per the go install community standard |
| Q12 | Version bumps and tags happen through the `release` GitHub Actions workflow (§12, Q16) |
| Q13 | Listen address hardwired to `127.0.0.1`/localhost — loopback only, no bind flag |
| Q14 | `go install` is the only install path (it compiles from source; Go never distributes prebuilt binaries); the release ships tag + notes only |
| Q15 | Complexity: cyclomatic ≤ 15 per function; at most 2 functions repo-wide may exceed it, the 3rd fails CI |
| Q16 | Release is fully automatic when code lands on main (PR merge); no manual tag, no manual dispatch |
| Q17 | Release bump level comes from the merged branch's prefix, named directly after the semver level: `patch/*` → PATCH, `minor/*` → MINOR, `major/*` → MAJOR; other prefixes don't release |

## 14. Open questions (gaps)

None — the design is closed. New gaps found during implementation get added here with indexed options before code is written.