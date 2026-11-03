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
// on start, stop, restart, status and serve; there are no defaults. State
// (pidfile, daemon log) is per local port, so relays on different ports
// coexist on one host and are stopped, restarted and logged independently.
// start and serve additionally accept --log (default true) and --log-days
// (default 7) for the daemon's file log; start forwards them on the
// daemon's argv. Logging is daemon-side state only: stop, status, restart
// and version do not take the flags. relay version (or a global -v /
// --version) prints the release version stamped at build time; source
// builds report "dev".
//
// Usage:
//
//	relay start <local-port> <remote-port> [--log=true|false] [--log-days=N]
//	relay stop <local-port> <remote-port>      # SIGTERM the daemon, wait for exit; no-op if not running
//	relay restart <local-port> <remote-port>   # stop + start; safe in either state
//	relay status <local-port> <remote-port>    # report daemon and listener state
//	relay serve <local-port> <remote-port> [--log=true|false] [--log-days=N]
//	relay version                              # print the release version
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
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is stamped at release time with -ldflags "-X main.version=vX.Y.Z";
// source builds report "dev".
var version = "dev"

const (
	upstreamHost = "tenor.local"

	// ponytail: stateDir keeps the historical /tmp/layer-cli path because
	// that is where the host's existing relay state lives; renaming it
	// would orphan the pidfiles the running fleet was started with.
	// Ceiling: rename together with the next fleet migration window.
	stateDir = "/tmp/layer-cli"

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
		localPort, remotePort := parsePorts(rest)
		fmt.Println(status(localPort, remotePort))
	case "serve":
		localPort, remotePort, lc := parseServeArgs(rest)
		serve(localPort, remotePort, lc)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relay [start|stop|restart|status|serve|version] <local-port> <remote-port>")
	fmt.Fprintln(os.Stderr, "       start/serve flags: --log=true|false --log-days=N (defaults: true, 7)")
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

// readPID returns the daemon's recorded pid, or 0 if there is no usable
// pidfile.
func readPID(pidFile string) int {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
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

func status(localPort, remotePort string) string {
	listenAddr := "127.0.0.1:" + localPort
	upstream := upstreamHost + ":" + remotePort
	pidFile, logFile := pidPath(localPort), logPath(localPort)
	pid := readPID(pidFile)
	switch {
	case pid == 0:
		return "not running"
	case !alive(pid):
		clearPID(pidFile)
		return fmt.Sprintf("stale pidfile (pid %d gone); cleaned up", pid)
	}
	state := fmt.Sprintf("running (pid %d)", pid)
	conn, err := net.DialTimeout("tcp", listenAddr, 500*time.Millisecond)
	if err != nil {
		return state + " — NOT LISTENING on " + listenAddr + " (check " + logFile + ")"
	}
	conn.Close()
	return state + ", listening on " + listenAddr + " -> " + upstream
}

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
	// success.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
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
