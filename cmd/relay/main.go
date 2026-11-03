// Command relay forwards TCP connections from 127.0.0.1:<local-port>
// to tenor.local:<remote-port>.
//
// It exists because ai-jail cannot resolve `.local` names: its /run tmpfs
// hides the Avahi socket that host-side mDNS resolution depends on, so
// jailed agents fail with "Name or service not known" the moment they dial
// the LiteLLM proxy by name (see aider-selfhost.md incident addendum,
// 2026-09-25). Jailed agents therefore dial http://localhost:<local-port>/v1
// — localhost is the one name ai-jail's synthesized /etc/hosts always
// carries — and this relay does the .local resolution on the host side,
// where mDNS works.
//
// The upstream name is resolved fresh on every connection (net.Dial per
// handle), so a DHCP lease change on tenor self-heals on the next request:
// nothing is pinned anywhere.
//
// The local listen port and the remote upstream port are required arguments
// on start, stop, restart, status and serve; there are no defaults. relay
// status all is the one fan-out form: it scans the state dir and prints one
// status line per relay found there. State (pidfile, daemon log) is per
// local port, so relays on different ports coexist on one host and are
// stopped, restarted and logged independently. start and serve additionally
// accept --log (default true) and --log-days (default 7) for the daemon's
// file log; start forwards them on the daemon's argv. Logging is
// daemon-side state only: stop, status, restart and version do not take the
// flags. relay version (or a global -v / --version) prints the release
// version stamped at build time; source builds report "dev".
//
// The pidfile records the daemon's pid and the port pair it was started
// with ("<pid> <local> <remote>"), which is what completion and status all
// read to know each relay's mapping.
//
// Usage:
//
//	relay start <local-port> <remote-port> [--log=true|false] [--log-days=N]
//	relay stop <local-port> <remote-port>      # SIGTERM the daemon, wait for exit; no-op if not running
//	relay restart <local-port> <remote-port>   # stop + start; safe in either state
//	relay status <local-port> <remote-port>    # report daemon and listener state
//	relay status all                           # one line per known relay, exit 1 if any is broken
//	relay serve <local-port> <remote-port> [--log=true|false] [--log-days=N]
//	relay version                              # print the release version
//	relay completion [bash|zsh]                # print a shell completion script (eval it)
//	relay __complete <words...>                # hidden: completion candidates, one per line
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is stamped at release time with -ldflags "-X main.version=vX.Y.Z";
// source builds report "dev".
var version = "dev"

// ponytail: stateDir keeps the historical /tmp/layer-cli path because that
// is where the host's existing relay state lives; renaming it would orphan
// the pidfiles the running fleet was started with. Ceiling: rename together
// with the next fleet migration window. It is a var, not a const, so the
// completion and status-all tests can point it at a scratch state dir.
var stateDir = "/tmp/layer-cli"

const (
	upstreamHost = "tenor.local"

	defaultLogDays = 7

	// maxLogDays bounds --log-days so days*24h cannot overflow
	// time.Duration (int64 ns) and wrap negative, which would put the
	// prune cutoff in the future and drop every log line.
	maxLogDays = 1 << 16
)

// pidPath and logPath name the per-local-port state files. One file pair
// per relay: several daemons coexist on one host, and a single shared
// pidfile would make every start/stop/status hit whichever daemon wrote it
// last — stop 8888 would have killed 11434's daemon.
func pidPath(localPort string) string { return stateDir + "/relay-" + localPort + ".pid" }
func logPath(localPort string) string { return stateDir + "/relay-" + localPort + ".log" }

// logConfig carries the daemon-side file-log options. start forwards them
// on the spawned serve argv so the daemon inherits the caller's values.
type logConfig struct {
	enabled bool
	days    int
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}
	// Global version check before subcommand dispatch.
	if args[0] == "-v" || args[0] == "--version" {
		fmt.Println(version)
		return
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Println(version)
	case "start":
		localPort, remotePort, lc := parseServeArgs(rest)
		start(localPort, remotePort, lc)
	case "stop":
		localPort, _ := parsePorts(rest)
		stop(localPort)
	case "restart":
		localPort, remotePort := parsePorts(rest)
		stop(localPort)
		start(localPort, remotePort, logConfig{enabled: true, days: defaultLogDays})
	case "status":
		if len(rest) == 1 && rest[0] == "all" {
			statusAll()
			return
		}
		localPort, remotePort := parsePorts(rest)
		fmt.Println(status(localPort, remotePort))
	case "serve":
		localPort, remotePort, lc := parseServeArgs(rest)
		serve(localPort, remotePort, lc)
	case "completion":
		completionCmd(rest)
	case "__complete":
		for _, c := range complete(rest) {
			fmt.Println(c)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relay [start|stop|restart|status|serve|version|completion] <local-port> <remote-port>")
	fmt.Fprintln(os.Stderr, "       start/serve flags: --log=true|false --log-days=N (defaults: true, 7)")
	fmt.Fprintln(os.Stderr, "       status all: one line per known relay; completion [bash|zsh]: install with eval \"$(relay completion SHELL)\"")
	os.Exit(2)
}

// parsePorts validates the two required port arguments and returns them
// verbatim (both must be numeric and in 1-65535).
func parsePorts(args []string) (local, remote string) {
	if len(args) != 2 {
		usage()
	}
	for _, p := range args {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			fmt.Fprintf(os.Stderr, "relay: invalid port %q\n", p)
			os.Exit(2)
		}
	}
	return args[0], args[1]
}

// parseServeArgs parses a start/serve argument list: the two required port
// positionals plus the optional --log/--log-days flags in either order.
// Flags must carry their value inline (--log=false, --log-days=3); the
// space-separated form would put the value among the positionals and is
// rejected as a stray argument by the port check.
func parseServeArgs(args []string) (local, remote string, lc logConfig) {
	var flagArgs, positionals []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positionals = append(positionals, a)
		}
	}
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	fs.BoolVar(&lc.enabled, "log", true, "write the daemon file log")
	fs.IntVar(&lc.days, "log-days", defaultLogDays, "file-log retention in days")
	fs.Parse(flagArgs)
	if lc.days < 1 || lc.days > maxLogDays {
		fmt.Fprintf(os.Stderr, "relay: --log-days must be 1-%d, got %d\n", maxLogDays, lc.days)
		os.Exit(2)
	}
	local, remote = parsePorts(positionals)
	return local, remote, lc
}

// relayState is the pidfile record: the daemon pid plus the remote port it
// forwards to. The local port is not kept here — it is already the name of
// the file (relay-<local>.pid) — and remote is empty for a legacy
// single-number pidfile, which records the pid only.
type relayState struct {
	pid    int
	remote string
}

// readState parses a pidfile. The pid comes first; the port pair follows in
// files written by the current serve, and is tolerated absent so old
// single-number pidfiles still load. pid is 0 when the file is missing or
// unusable.
func readState(pidFile string) relayState {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return relayState{}
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return relayState{}
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return relayState{}
	}
	st := relayState{pid: pid}
	if len(fields) > 2 {
		st.remote = fields[2]
	}
	return st
}

// readPID returns the daemon's recorded pid, or 0 if there is no usable
// pidfile.
func readPID(pidFile string) int {
	return readState(pidFile).pid
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func clearPID(pidFile string) { os.Remove(pidFile) }

// start brings the daemon up. Idempotent: if a live daemon is already
// recorded it reports and exits 0 without touching anything; a stale
// pidfile is cleared and startup proceeds. Success is reported only once
// the daemon has bound the listener and written its pidfile, so losing
// the bind to a foreign port holder surfaces as a failure, not success.
func start(localPort, remotePort string, lc logConfig) {
	pidFile, logFile := pidPath(localPort), logPath(localPort)
	if pid := readPID(pidFile); pid != 0 {
		if alive(pid) {
			fmt.Printf("already running (pid %d); nothing to do\n", pid)
			return
		}
		clearPID(pidFile)
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve self: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", stateDir, err)
		os.Exit(1)
	}
	var logf *os.File
	if lc.enabled {
		logf, err = os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", logFile, err)
			os.Exit(1)
		}
		defer logf.Close()
	}

	// Detach into its own session so it survives this shell; stdio goes to
	// the log file, not the terminal (nowhere with --log=false). The ports
	// ride on the serve argv so the daemon forwards the mapping it was
	// started with, and the log flags ride along so the daemon inherits
	// the caller's values.
	argv := []string{"serve", localPort, remotePort,
		"--log=" + strconv.FormatBool(lc.enabled),
		"--log-days=" + strconv.Itoa(lc.days)}
	child := exec.Command(exe, argv...)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdout, child.Stderr = logf, logf
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "spawn daemon: %v\n", err)
		os.Exit(1)
	}

	listenAddr := "127.0.0.1:" + localPort
	upstream := upstreamHost + ":" + remotePort

	// Success waits for the pidfile, which serve writes only after binding
	// the listener: an existing pidfile is the proof that OUR daemon owns
	// the port (any stale pidfile was cleared before the spawn). The child
	// is reaped through childDone — without Wait it would linger as a
	// zombie that alive() misreads as running — so a bind loss surfaces
	// promptly as a death report, never as a false success.
	childDone := make(chan struct{})
	go func() {
		child.Wait()
		close(childDone)
	}()
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		select {
		case <-childDone:
			fmt.Fprintf(os.Stderr, "daemon died at startup; last log lines:\n  %s", tail(logFile))
			clearPID(pidFile)
			os.Exit(1)
		default:
		}
		if pid := readPID(pidFile); pid != 0 {
			fmt.Printf("started (pid %d, listening on %s -> %s)\n", pid, listenAddr, upstream)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "listener not ready after 2s; check %s\n", logFile)
	os.Exit(1)
}

// stop brings the local-port daemon down. Idempotent: reports and exits 0
// when nothing is running; cleans up a stale pidfile if it finds one.
func stop(localPort string) {
	pidFile := pidPath(localPort)
	pid := readPID(pidFile)
	if pid == 0 {
		fmt.Println("not running; nothing to do")
		return
	}
	if !alive(pid) {
		fmt.Printf("stale pidfile (pid %d gone); cleaned up\n", pid)
		clearPID(pidFile)
		return
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if !alive(pid) {
			clearPID(pidFile)
			fmt.Printf("stopped (pid %d)\n", pid)
			return
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	clearPID(pidFile)
	fmt.Printf("force-killed (pid %d)\n", pid)
}

// probeRelay renders one relay's status line and reports whether the state
// is a failure: a daemon with a live recorded pid whose listener is gone.
// A relay that is simply stopped, or a stale pidfile this probe cleans up,
// is reported, not failed — the same treatment the single-relay commands
// give those states.
func probeRelay(localPort, remotePort string) (line string, failed bool) {
	listenAddr := "127.0.0.1:" + localPort
	// remotePort is empty for a legacy pidfile found by status all; the
	// explicit status form always carries a validated port.
	upstream := upstreamHost
	if remotePort != "" {
		upstream += ":" + remotePort
	}
	pidFile, logFile := pidPath(localPort), logPath(localPort)
	pid := readPID(pidFile)
	switch {
	case pid == 0:
		return "not running", false
	case !alive(pid):
		clearPID(pidFile)
		return fmt.Sprintf("stale pidfile (pid %d gone); cleaned up", pid), false
	}
	state := fmt.Sprintf("running (pid %d)", pid)
	conn, err := net.DialTimeout("tcp", listenAddr, 500*time.Millisecond)
	if err != nil {
		return state + " — NOT LISTENING on " + listenAddr + " (check " + logFile + ")", true
	}
	conn.Close()
	return state + ", listening on " + listenAddr + " -> " + upstream, false
}

func status(localPort, remotePort string) string {
	line, _ := probeRelay(localPort, remotePort)
	return line
}

// statusAll prints one status line per known relay — every relay-*.pid in
// the state dir, running or not, with each relay's remote port taken from
// its pidfile — and exits 1 if any of them is broken, 0 otherwise: the
// fan-out exit rule. Each line is prefixed with the local port: not running
// and stale lines carry no listen address to self-identify, and a fan-out
// line that cannot be mapped back to its relay defeats the purpose of all.
func statusAll() {
	failed := false
	for _, localPort := range knownRelays() {
		line, broke := probeRelay(localPort, readState(pidPath(localPort)).remote)
		fmt.Println(localPort + ": " + line)
		if broke {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

// knownRelays lists the local ports with a state file in the state dir —
// every relay this machine has started, running or not — sorted. The port
// rides on the filename, so even a stale or legacy-format pidfile is
// listed.
func knownRelays() []string {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}
	var relays []string
	for _, e := range entries {
		if port, ok := relayPort(e.Name()); ok {
			relays = append(relays, port)
		}
	}
	sort.Strings(relays)
	return relays
}

// relayPort extracts the local port from a state-dir filename
// (relay-<port>.pid) and reports whether the name is one of ours.
func relayPort(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "relay-")
	if !ok {
		return "", false
	}
	port, ok := strings.CutSuffix(rest, ".pid")
	if !ok || port == "" {
		return "", false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", false
	}
	return port, true
}

// commands are the visible subcommands; __complete is deliberately absent
// (hidden machinery, never offered by completion).
var commands = []string{"start", "stop", "restart", "status", "serve", "version", "completion"}

// complete returns the completion candidates for the words typed so far,
// cobra-style: the last word is the one being completed (possibly empty),
// the words before it are the fixed context. Read-only over the state dir —
// it never spawns or kills a daemon and never writes state.
func complete(words []string) []string {
	toComplete := ""
	fixed := words
	if len(words) > 0 {
		toComplete = words[len(words)-1]
		fixed = words[:len(words)-1]
	}
	if len(fixed) == 0 {
		// The documented global flags complete before any subcommand; the
		// subcommand list cannot match a dash-prefixed word.
		if strings.HasPrefix(toComplete, "-") {
			return filterPrefix([]string{"-v", "--version"}, toComplete)
		}
		return filterPrefix(commands, toComplete)
	}
	switch fixed[0] {
	case "start", "stop", "restart", "status", "serve":
		return completeRelayArgs(fixed[0], fixed[1:], toComplete)
	case "completion":
		if len(fixed) == 1 {
			return filterPrefix([]string{"bash", "zsh"}, toComplete)
		}
		return nil
	}
	return nil
}

// completeRelayArgs completes a lifecycle command's arguments: the
// start/serve flags for a dash-prefixed word, the known local ports for the
// first positional (status additionally offers the all fan-out), the typed
// relay's recorded remote port for the second, and nothing beyond — the
// commands take exactly two positionals.
func completeRelayArgs(cmd string, args []string, toComplete string) []string {
	if strings.HasPrefix(toComplete, "-") {
		if cmd == "start" || cmd == "serve" {
			return filterPrefix([]string{"--log=true", "--log=false", "--log-days="}, toComplete)
		}
		return nil
	}
	var positionals []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			positionals = append(positionals, a)
		}
	}
	switch len(positionals) {
	case 0:
		candidates := knownRelays()
		if cmd == "status" {
			candidates = append([]string{"all"}, candidates...)
		}
		return filterPrefix(candidates, toComplete)
	case 1:
		if cmd == "status" && positionals[0] == "all" {
			return nil
		}
		remote := readState(pidPath(positionals[0])).remote
		if remote == "" {
			return nil
		}
		return filterPrefix([]string{remote}, toComplete)
	}
	return nil
}

// filterPrefix keeps the candidates the already-typed prefix matches.
func filterPrefix(candidates []string, prefix string) []string {
	var out []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// completionCmd prints the completion script for the named shell; the user
// installs it with eval "$(relay completion bash|zsh)".
func completionCmd(args []string) {
	if len(args) != 1 {
		usage()
	}
	switch args[0] {
	case "bash":
		fmt.Print(bashCompletionScript)
	case "zsh":
		fmt.Print(zshCompletionScript)
	default:
		usage()
	}
}

// The scripts feed the words after "relay" — the current, possibly empty
// word included — to the hidden __complete subcommand, which does the
// prefix filtering; the scripts only load the newline-separated candidates.
// In zsh the substitution inside the (f) split must be quoted: unquoted,
// zsh does not split it, so every candidate collapses into one element;
// quoted, multi-line output yields N elements and empty output zero, which
// keeps the count guard live.
//
// ponytail: bash and zsh are the shells in use; fish/powershell are a
// ~10-line script each the day one is needed. Ceiling: four more cases in
// completionCmd.
const bashCompletionScript = `# bash completion for relay
__relay_complete() {
    mapfile -t COMPREPLY < <(relay __complete "${COMP_WORDS[@]:1}")
}
complete -F __relay_complete relay
`

const zshCompletionScript = `#compdef relay
_relay() {
    local -a candidates
    candidates=(${(f)"$(relay __complete "${(@)words[2,-1]}")"})
    (( ${#candidates} )) && compadd -- $candidates
}
compdef _relay relay
`

func tail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := splitLines(data)
	lo := len(lines) - 5
	if lo < 0 {
		lo = 0
	}
	out := ""
	for _, l := range lines[lo:] {
		out += l + "\n"
	}
	return out
}

func splitLines(data []byte) []string {
	var lines []string
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, string(data[start:i]))
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}

// pruneLog rewrites the log keeping only lines newer than retention. Lines
// carry the standard log-package timestamp prefix; lines that don't parse
// are kept. Safe against the daemon's O_APPEND fd: truncation just moves
// EOF, and subsequent appends land after the rewritten content.
func pruneLog(path string, retention time.Duration) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-retention)
	var kept []byte
	dropped := false
	for _, line := range splitLines(data) {
		n := len(line)
		if n > 19 {
			n = 19
		}
		if ts, err := time.ParseInLocation("2006/01/02 15:04:05", line[:n], time.Local); err == nil && ts.Before(cutoff) {
			dropped = true
			continue
		}
		kept = append(kept, line...)
		kept = append(kept, '\n')
	}
	if dropped {
		os.WriteFile(path, kept, 0o644)
	}
}

// serve runs the relay in the foreground; `start` detaches a copy of it.
func serve(localPort, remotePort string, lc logConfig) {
	listenAddr := "127.0.0.1:" + localPort
	upstream := upstreamHost + ":" + remotePort
	logFile, pidFile := logPath(localPort), pidPath(localPort)

	log.SetFlags(log.LstdFlags)
	os.MkdirAll(stateDir, 0o755)
	if lc.enabled {
		retention := time.Duration(lc.days) * 24 * time.Hour
		pruneLog(logFile, retention)
		go func() {
			for range time.Tick(time.Hour) {
				pruneLog(logFile, retention)
			}
		}()
	}

	srv, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", listenAddr, err)
	}
	log.Printf("relay on %s -> %s", listenAddr, upstream)
	// Written last, after the bind and the startup log line: the pidfile's
	// existence is the proof that this daemon owns the listener and
	// finished startup, which is what start waits for before reporting
	// success. The port pair rides along so completion and status all can
	// report each relay's mapping without asking the caller.
	state := fmt.Sprintf("%d %s %s", os.Getpid(), localPort, remotePort)
	if err := os.WriteFile(pidFile, []byte(state), 0o644); err != nil {
		log.Fatalf("write %s: %v", pidFile, err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		log.Printf("signal received; shutting down")
		srv.Close()
	}()

	for {
		client, err := srv.Accept()
		if err != nil {
			// srv.Close from the signal handler lands here; shutting
			// down on this goroutine keeps the pidfile removal ordered
			// before exit instead of racing os.Exit in the handler.
			if errors.Is(err, net.ErrClosed) {
				os.Remove(pidFile)
				return
			}
			log.Fatalf("accept: %v", err)
		}
		go handle(client, upstream)
	}
}

// handle proxies one connection: dial upstream, then copy bytes both ways
// until either side closes. Each dial resolves the upstream name anew.
func handle(client net.Conn, upstream string) {
	defer client.Close()
	up, err := net.Dial("tcp", upstream)
	if err != nil {
		log.Printf("upstream fail: %v", err)
		return
	}
	defer up.Close()

	done := make(chan struct{})
	go func() {
		io.Copy(up, client)
		closeWrite(up)
		close(done)
	}()
	io.Copy(client, up)
	closeWrite(client)
	<-done
}

// closeWrite half-closes a TCP connection's send side so the peer sees EOF
// once we are done writing, without killing our receive side.
func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.CloseWrite()
	}
}
