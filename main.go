// Command layer-cli forwards TCP connections from 127.0.0.1:<local-port>
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
// The local listen port and the remote upstream port are required
// arguments on every command; there are no defaults.
//
// Usage:
//
//	layer-cli start <local-port> <remote-port>     # daemonize; idempotent — no-op if already running
//	layer-cli stop <local-port> <remote-port>      # SIGTERM the daemon, wait for exit; no-op if not running
//	layer-cli restart <local-port> <remote-port>   # stop + start; safe in either state
//	layer-cli status <local-port> <remote-port>    # report daemon and listener state
//	layer-cli serve <local-port> <remote-port>     # foreground; used by start, fine under systemd
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const (
	upstreamHost = "tenor.local"
	stateDir     = "/tmp/layer-cli"
	pidFile      = stateDir + "/layer.pid"
	logFile      = stateDir + "/layer.log"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}
	cmd, portArgs := args[0], args[1:]
	localPort, remotePort := parsePorts(portArgs)

	switch cmd {
	case "start":
		start(localPort, remotePort)
	case "stop":
		stop()
	case "restart":
		stop()
		start(localPort, remotePort)
	case "status":
		fmt.Println(status(localPort, remotePort))
	case "serve":
		serve(localPort, remotePort)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: layer-cli [start|stop|restart|status|serve] <local-port> <remote-port>")
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
			fmt.Fprintf(os.Stderr, "layer-cli: invalid port %q\n", p)
			os.Exit(2)
		}
	}
	return args[0], args[1]
}

// readPID returns the daemon's recorded pid, or 0 if there is no usable
// pidfile.
func readPID() int {
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

func clearPID() { os.Remove(pidFile) }

// start brings the daemon up. Idempotent: if a live daemon is already
// recorded it reports and exits 0 without touching anything; a stale
// pidfile is cleared and startup proceeds.
func start(localPort, remotePort string) {
	if pid := readPID(); pid != 0 {
		if alive(pid) {
			fmt.Printf("already running (pid %d); nothing to do\n", pid)
			return
		}
		clearPID()
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve self: %v\n", err)
		os.Exit(1)
	}
	logf, err := func() (*os.File, error) {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return nil, err
		}
		return os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", logFile, err)
		os.Exit(1)
	}
	defer logf.Close()

	// Detach into its own session so it survives this shell; stdio goes
	// to the log file, not the terminal. The ports ride on the serve
	// argv so the daemon forwards the mapping it was started with.
	child := exec.Command(exe, "serve", localPort, remotePort)
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdout, child.Stderr = logf, logf
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "spawn daemon: %v\n", err)
		os.Exit(1)
	}
	pid := child.Process.Pid
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", pidFile, err)
		os.Exit(1)
	}

	listenAddr := "127.0.0.1:" + localPort
	upstream := upstreamHost + ":" + remotePort

	// Report success only once the listener actually answers, so a bind
	// failure (port taken, etc.) surfaces here instead of silently
	// "succeeding".
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if !alive(pid) {
			fmt.Fprintf(os.Stderr, "daemon died at startup; last log lines:\n  %s", tail(logFile))
			clearPID()
			os.Exit(1)
		}
		if conn, err := net.DialTimeout("tcp", listenAddr, 200*time.Millisecond); err == nil {
			conn.Close()
			fmt.Printf("started (pid %d, listening on %s -> %s)\n", pid, listenAddr, upstream)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "listener not ready after 2s; check %s\n", logFile)
	os.Exit(1)
}

// stop brings the daemon down. Idempotent: reports and exits 0 when
// nothing is running; cleans up a stale pidfile if it finds one.
func stop() {
	pid := readPID()
	if pid == 0 {
		fmt.Println("not running; nothing to do")
		return
	}
	if !alive(pid) {
		fmt.Printf("stale pidfile (pid %d gone); cleaned up\n", pid)
		clearPID()
		return
	}
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if !alive(pid) {
			clearPID()
			fmt.Printf("stopped (pid %d)\n", pid)
			return
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	clearPID()
	fmt.Printf("force-killed (pid %d)\n", pid)
}

func status(localPort, remotePort string) string {
	listenAddr := "127.0.0.1:" + localPort
	upstream := upstreamHost + ":" + remotePort
	pid := readPID()
	switch {
	case pid == 0:
		return "not running"
	case !alive(pid):
		clearPID()
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

// pruneLog rewrites the log keeping only the last 24h. Lines carry the
// standard log-package timestamp prefix; lines that don't parse are kept.
// Safe against the daemon's O_APPEND fd: truncation just moves EOF, and
// subsequent appends land after the rewritten content.
func pruneLog() {
	data, err := os.ReadFile(logFile)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	var kept []byte
	dropped := false
	for _, line := range splitLines(data) {
		n := len(line)
		if n > 19 {
			n = 19
		}
		if ts, err := time.Parse("2006/01/02 15:04:05", line[:n]); err == nil && ts.Before(cutoff) {
			dropped = true
			continue
		}
		kept = append(kept, line...)
		kept = append(kept, '\n')
	}
	if dropped {
		os.WriteFile(logFile, kept, 0o644)
	}
}

// serve runs the relay in the foreground; `start` detaches a copy of it.
func serve(localPort, remotePort string) {
	listenAddr := "127.0.0.1:" + localPort
	upstream := upstreamHost + ":" + remotePort

	log.SetFlags(log.LstdFlags)
	os.MkdirAll(stateDir, 0o755)
	pruneLog()
	go func() {
		for range time.Tick(time.Hour) {
			pruneLog()
		}
	}()

	srv, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", listenAddr, err)
	}
	log.Printf("relay on %s -> %s", listenAddr, upstream)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		log.Printf("signal received; shutting down")
		srv.Close()
		os.Remove(pidFile)
		os.Exit(0)
	}()

	for {
		client, err := srv.Accept()
		if err != nil {
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
