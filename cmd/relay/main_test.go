package main

// Tests for the CLI surface that is testable in process (argument parsing,
// version dispatch, status strings, stop, and the pidfile/log helpers).
// The proxy data path lives in proxy_test.go, the daemon lifecycle in
// daemon_test.go.
//
// os.Exit paths (usage, flag validation) run through a re-executed test
// process: TestMain dispatches on RELAY_TEST_EXIT_CASE so a child process
// can call the exiting function without killing the test runner, and the
// parent asserts on the child's exit code.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readDeadline bounds every blocking network read in the suite so a broken
// contract fails the test with a timeout error instead of hanging it.
const readDeadline = 5 * time.Second

func TestMain(m *testing.M) {
	if casename := os.Getenv("RELAY_TEST_EXIT_CASE"); casename != "" {
		runExitCase(casename)
		// runExitCase returned, so the code path failed to exit; the
		// parent expects 2 and sees 9 and its assertion fails loudly.
		fmt.Fprintf(os.Stderr, "relay: exit case %q did not exit\n", casename)
		os.Exit(9)
	}
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// freeLocalPort returns a port that was free at probe time, as a string,
// for use as a relay local port in tests. The kernel only hands out high
// ephemeral ports here, so these never collide with the fixed fleet ports
// the real daemons on this host use, and never with the state files under
// the shared /tmp/layer-cli that belong to them.
func freeLocalPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// startSleeper spawns a `sleep` child for tests that need a real, killable
// process.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	return cmd
}

// deadChildPID spawns a sleep child, kills it and reaps it, and returns a
// pid that is guaranteed to be dead.
func deadChildPID(t *testing.T) int {
	t.Helper()
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	syscall.Kill(pid, syscall.SIGKILL)
	cmd.Wait()
	return pid
}

// writeStateFile places content at path, creating the state dir first, and
// removes the file when the test ends.
func writeStateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", stateDir, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Cleanup(func() { os.Remove(path) })
}

// captureStdout runs fn with os.Stdout redirected and returns what it
// printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	printed := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		printed <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = saved
	return <-printed
}

// exitCase re-executes this test binary for one os.Exit code path and
// returns the child's exit code and combined output.
func exitCase(t *testing.T, name string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "RELAY_TEST_EXIT_CASE="+name)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var eerr *exec.ExitError
		if !errors.As(err, &eerr) {
			t.Fatalf("re-exec for case %q: %v", name, err)
		}
		code = eerr.ExitCode()
	}
	return code, out.String()
}

// runExitCase invokes the os.Exit(2) code path named by name. Every branch
// must terminate the process.
func runExitCase(name string) {
	switch name {
	case "ports-missing":
		parsePorts([]string{"1234"})
	case "ports-extra":
		parsePorts([]string{"1", "2", "3"})
	case "ports-not-numeric":
		parsePorts([]string{"abc", "2"})
	case "ports-zero":
		parsePorts([]string{"0", "2"})
	case "ports-too-big":
		parsePorts([]string{"65536", "2"})
	case "serve-days-zero":
		parseServeArgs([]string{"1", "2", "--log-days=0"})
	case "serve-days-over-max":
		parseServeArgs([]string{"1", "2", fmt.Sprintf("--log-days=%d", maxLogDays+1)})
	case "serve-days-not-numeric":
		parseServeArgs([]string{"1", "2", "--log-days=abc"})
	case "serve-three-positionals":
		parseServeArgs([]string{"1", "2", "3"})
	case "serve-split-flag-value":
		// the space-separated form puts the value among the positionals,
		// where the two-port check rejects it as a stray argument
		parseServeArgs([]string{"1", "2", "--log-days", "3"})
	}
}

func TestParsePorts(t *testing.T) {
	local, remote := parsePorts([]string{"1", "65535"})
	if local != "1" || remote != "65535" {
		t.Errorf("boundary ports: got %q, %q, want verbatim \"1\", \"65535\"", local, remote)
	}
}

func TestParsePortsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		wantIn string
	}{
		{"ports-missing", ""},
		{"ports-extra", ""},
		{"ports-not-numeric", `invalid port "abc"`},
		{"ports-zero", ""},
		{"ports-too-big", ""},
	}
	for _, tt := range cases {
		code, out := exitCase(t, tt.name)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (output %q)", tt.name, code, out)
		}
		if tt.wantIn != "" && !strings.Contains(out, tt.wantIn) {
			t.Errorf("%s: output %q misses %q", tt.name, out, tt.wantIn)
		}
	}
}

func TestParseServeArgs(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantLocal   string
		wantRemote  string
		wantEnabled bool
		wantDays    int
	}{
		{"defaults", []string{"9000", "11434"}, "9000", "11434", true, defaultLogDays},
		{"log off inline", []string{"9000", "11434", "--log=false"}, "9000", "11434", false, defaultLogDays},
		{"flags before positionals", []string{"--log=false", "9000", "11434"}, "9000", "11434", false, defaultLogDays},
		{"flag between positionals", []string{"9000", "--log-days=3", "11434"}, "9000", "11434", true, 3},
		{"both flags", []string{"--log=false", "9000", "11434", "--log-days=1"}, "9000", "11434", false, 1},
		{"days lower boundary", []string{"9000", "11434", "--log-days=1"}, "9000", "11434", true, 1},
		{"days upper boundary", []string{"9000", "11434", fmt.Sprintf("--log-days=%d", maxLogDays)}, "9000", "11434", true, maxLogDays},
	}
	for _, tt := range cases {
		local, remote, lc := parseServeArgs(tt.args)
		if local != tt.wantLocal || remote != tt.wantRemote {
			t.Errorf("%s: ports %q,%q, want %q,%q", tt.name, local, remote, tt.wantLocal, tt.wantRemote)
		}
		if lc.enabled != tt.wantEnabled || lc.days != tt.wantDays {
			t.Errorf("%s: log config %+v, want enabled=%v days=%d", tt.name, lc, tt.wantEnabled, tt.wantDays)
		}
	}
}

func TestParseServeArgsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		wantIn string
	}{
		{"serve-days-zero", "--log-days must be 1-"},
		{"serve-days-over-max", "--log-days must be 1-"},
		{"serve-days-not-numeric", "log-days"},
		{"serve-three-positionals", ""},
		{"serve-split-flag-value", ""},
	}
	for _, tt := range cases {
		code, out := exitCase(t, tt.name)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (output %q)", tt.name, code, out)
		}
		if tt.wantIn != "" && !strings.Contains(out, tt.wantIn) {
			t.Errorf("%s: output %q misses %q", tt.name, out, tt.wantIn)
		}
	}
}

func TestStateFilePaths(t *testing.T) {
	if got, want := pidPath("8080"), "/tmp/layer-cli/relay-8080.pid"; got != want {
		t.Errorf("pidPath = %q, want %q", got, want)
	}
	if got, want := logPath("8080"), "/tmp/layer-cli/relay-8080.log"; got != want {
		t.Errorf("logPath = %q, want %q", got, want)
	}
}

func TestReadPID(t *testing.T) {
	path := pidPath(freeLocalPort(t))
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"garbage", "not-a-pid", 0},
		{"empty file", "", 0},
		{"zero", "0", 0},
		{"one", "1", 0},
		{"negative", "-5", 0},
		{"valid", "4321", 4321},
	}
	for _, tt := range cases {
		writeStateFile(t, path, tt.content)
		if got := readPID(path); got != tt.want {
			t.Errorf("%s: readPID = %d, want %d", tt.name, got, tt.want)
		}
	}
	os.Remove(path)
	if got := readPID(path); got != 0 {
		t.Errorf("missing file: readPID = %d, want 0", got)
	}
}

func TestAlive(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Error("alive(self) = false, want true")
	}
	if alive(0) || alive(-1) {
		t.Error("alive of a non-pid = true, want false")
	}
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	if !alive(pid) {
		t.Errorf("alive(%d) = false for a freshly started child", pid)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	cmd.Wait()
	if alive(pid) {
		t.Errorf("alive(%d) = true after kill and reap", pid)
	}
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\nb\n", []string{"a", "b"}},
		{"\n\n", []string{"", ""}},
	}
	for _, tt := range cases {
		got := splitLines([]byte(tt.in))
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitLines(%q) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestTail(t *testing.T) {
	if got := tail(filepath.Join(t.TempDir(), "absent.log")); got != "" {
		t.Errorf("tail of a missing file = %q, want empty", got)
	}
	dir := t.TempDir()
	short := filepath.Join(dir, "short.log")
	writeStateFile(t, short, "l1\nl2\nl3")
	if got, want := tail(short), "l1\nl2\nl3\n"; got != want {
		t.Errorf("tail of 3 lines = %q, want %q", got, want)
	}
	var b strings.Builder
	for i := 1; i <= 7; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	long := filepath.Join(dir, "long.log")
	writeStateFile(t, long, b.String())
	if got, want := tail(long), "line3\nline4\nline5\nline6\nline7\n"; got != want {
		t.Errorf("tail of 7 lines = %q, want last 5 %q", got, want)
	}
}

func TestPruneLog(t *testing.T) {
	const tsLayout = "2006/01/02 15:04:05"
	path := filepath.Join(t.TempDir(), "relay.log")
	stale := "2020/01/02 15:04:05 relay on 127.0.0.1:1 -> tenor.local:1\n" +
		"2020/01/02 15:04:05\n"
	fresh := time.Now().Format(tsLayout) + " fresh line\n"
	untimed := "upstream fail: connection refused\n"
	writeStateFile(t, path, stale+fresh+untimed)

	pruneLog(path, 24*time.Hour)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pruned log: %v", err)
	}
	if want := fresh + untimed; string(data) != want {
		t.Errorf("pruned log = %q, want %q", data, want)
	}

	// Nothing stale: the file must come out unchanged.
	pruneLog(path, 365*24*time.Hour)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("reread log: %v", err)
	}
	if string(data) != fresh+untimed {
		t.Errorf("fully kept log = %q, want %q", data, fresh+untimed)
	}

	// Absent file: a no-op that must not create anything.
	missing := filepath.Join(t.TempDir(), "absent.log")
	pruneLog(missing, 24*time.Hour)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("pruneLog created %s", missing)
	}
}

func TestVersionCommand(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()
	for _, argv := range [][]string{
		{"relay", "version"},
		{"relay", "-v"},
		{"relay", "--version"},
	} {
		os.Args = argv
		out := captureStdout(t, func() { main() })
		if want := version + "\n"; out != want {
			t.Errorf("relay %s printed %q, want %q", argv[1], out, want)
		}
	}
}

func TestStatus(t *testing.T) {
	t.Run("no pidfile", func(t *testing.T) {
		port := freeLocalPort(t)
		if got, want := status(port, "1"), "not running"; got != want {
			t.Errorf("status = %q, want %q", got, want)
		}
	})

	t.Run("stale pidfile is cleaned up", func(t *testing.T) {
		port := freeLocalPort(t)
		pid := deadChildPID(t)
		writeStateFile(t, pidPath(port), strconv.Itoa(pid))
		got := status(port, "1")
		want := fmt.Sprintf("stale pidfile (pid %d gone); cleaned up", pid)
		if got != want {
			t.Errorf("status = %q, want %q", got, want)
		}
		if _, err := os.Stat(pidPath(port)); !os.IsNotExist(err) {
			t.Errorf("stale pidfile still present after status")
		}
	})

	t.Run("alive but not listening", func(t *testing.T) {
		port := freeLocalPort(t)
		writeStateFile(t, pidPath(port), strconv.Itoa(os.Getpid()))
		got := status(port, "1")
		want := fmt.Sprintf("running (pid %d) — NOT LISTENING on 127.0.0.1:%s (check %s)",
			os.Getpid(), port, logPath(port))
		if got != want {
			t.Errorf("status = %q, want %q", got, want)
		}
	})

	t.Run("alive and listening", func(t *testing.T) {
		port := freeLocalPort(t)
		remote := freeLocalPort(t)
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		writeStateFile(t, pidPath(port), strconv.Itoa(os.Getpid()))
		got := status(port, remote)
		want := fmt.Sprintf("running (pid %d), listening on 127.0.0.1:%s -> tenor.local:%s",
			os.Getpid(), port, remote)
		if got != want {
			t.Errorf("status = %q, want %q", got, want)
		}
	})
}

func TestStop(t *testing.T) {
	t.Run("not running", func(t *testing.T) {
		port := freeLocalPort(t)
		out := captureStdout(t, func() { stop(port) })
		if want := "not running; nothing to do\n"; out != want {
			t.Errorf("stop printed %q, want %q", out, want)
		}
	})

	t.Run("stale pidfile", func(t *testing.T) {
		port := freeLocalPort(t)
		pid := deadChildPID(t)
		writeStateFile(t, pidPath(port), strconv.Itoa(pid))
		out := captureStdout(t, func() { stop(port) })
		if want := fmt.Sprintf("stale pidfile (pid %d gone); cleaned up\n", pid); out != want {
			t.Errorf("stop printed %q, want %q", out, want)
		}
		if _, err := os.Stat(pidPath(port)); !os.IsNotExist(err) {
			t.Errorf("stale pidfile not removed")
		}
	})

	t.Run("running daemon is SIGTERMed and pidfile cleared", func(t *testing.T) {
		port := freeLocalPort(t)
		cmd := startSleeper(t)
		pid := cmd.Process.Pid
		// Reap concurrently: a killed child stays a zombie until Wait
		// returns, and alive() reports zombies as alive, which would
		// drive stop into its SIGKILL escalation for no reason.
		waitErr := make(chan error, 1)
		go func() { waitErr <- cmd.Wait() }()
		writeStateFile(t, pidPath(port), strconv.Itoa(pid))

		out := captureStdout(t, func() { stop(port) })
		if want := fmt.Sprintf("stopped (pid %d)\n", pid); out != want {
			t.Errorf("stop printed %q, want %q", out, want)
		}
		select {
		case err := <-waitErr:
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("sleep exit: %v", err)
			}
			ws, ok := ee.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
				t.Fatalf("sleep was not terminated by SIGTERM: %v", ee.ProcessState)
			}
		case <-time.After(readDeadline):
			t.Fatalf("sleep did not exit after stop")
		}
		if got := readPID(pidPath(port)); got != 0 {
			t.Errorf("pidfile still holds %d after stop", got)
		}
	})
}
