package main

// End-to-end lifecycle tests: they build the real relay binary and drive
// start/status/stop/restart against it, detached daemon included. The
// in-process path cannot be used for this: start spawns os.Executable(),
// which under go test is the test binary, not relay.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	binDir    string
	binPath   string
	buildErr  error
)

// buildRelay compiles the real CLI binary once per test run so lifecycle
// tests exercise the actual spawn/detach flow.
func buildRelay(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "relay-test-bin-*")
		if buildErr != nil {
			return
		}
		binPath = filepath.Join(binDir, "relay")
		// Plain go build: no ldflags stamp, so the version surface
		// reports the documented source-build default "dev".
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build relay binary: %v", buildErr)
	}
	return binPath
}

// runRelay runs one CLI invocation with a timeout and returns its stdout,
// stderr and exit code.
func runRelay(t *testing.T, timeout time.Duration, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildRelay(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("relay %v timed out", args)
	}
	code := 0
	if err != nil {
		var eerr *exec.ExitError
		if !errors.As(err, &eerr) {
			t.Fatalf("run relay %v: %v", args, err)
		}
		code = eerr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// cleanupRelayState force-stops any daemon a lifecycle test may have left
// behind and removes its per-port state files. Register it before the
// first start so it runs even when the test fails midway.
func cleanupRelayState(t *testing.T, localPort string) {
	t.Cleanup(func() {
		if pid := readPID(pidPath(localPort)); pid != 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		os.Remove(pidPath(localPort))
		os.Remove(logPath(localPort))
	})
}

func TestRelayVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"-v"}, {"--version"}} {
		stdout, stderr, code := runRelay(t, 10*time.Second, args...)
		if code != 0 {
			t.Errorf("relay %v: exit %d, want 0 (stderr %q)", args, code, stderr)
		}
		if want := "dev\n"; stdout != want {
			t.Errorf("relay %v: stdout %q, want %q (plain source builds report dev)", args, stdout, want)
		}
		if stderr != "" {
			t.Errorf("relay %v: stderr %q, want empty", args, stderr)
		}
	}
}

func TestRelayUsageErrors(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		wantIn string
	}{
		{"no arguments", nil, "usage: relay"},
		{"unknown subcommand", []string{"frobnicate", "1", "2"}, "usage: relay"},
		{"invalid port", []string{"status", "not-a-port", "1"}, `invalid port "not-a-port"`},
		// Logging is daemon-side state only: stop, status and restart do
		// not take the log flags.
		{"stop takes no log flags", []string{"stop", "1", "2", "--log=false"}, "usage: relay"},
		{"status takes no log flags", []string{"status", "1", "2", "--log-days=3"}, "usage: relay"},
		{"restart takes no log flags", []string{"restart", "1", "2", "--log=false"}, "usage: relay"},
		{"non-numeric log days", []string{"start", "1", "2", "--log-days=abc"}, "log-days"},
		{"log days below minimum", []string{"start", "1", "2", "--log-days=0"}, "--log-days must be 1-"},
		{"log days above maximum", []string{"start", "1", "2", fmt.Sprintf("--log-days=%d", maxLogDays+1)}, "--log-days must be 1-"},
	}
	for _, tt := range cases {
		stdout, stderr, code := runRelay(t, 10*time.Second, tt.args...)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (out %q, err %q)", tt.name, code, stdout, stderr)
		}
		if !strings.Contains(stdout+stderr, tt.wantIn) {
			t.Errorf("%s: output misses %q (out %q, err %q)", tt.name, tt.wantIn, stdout, stderr)
		}
	}
}

func TestRelayLifecycle(t *testing.T) {
	local := freeLocalPort(t)
	remote := freeLocalPort(t)
	cleanupRelayState(t, local)

	stdout, stderr, code := runRelay(t, 30*time.Second, "start", local, remote)
	if code != 0 {
		t.Fatalf("start: exit %d (out %q, err %q)", code, stdout, stderr)
	}
	pid := readPID(pidPath(local))
	if pid == 0 {
		t.Fatalf("start: no usable pidfile after success")
	}
	if want := fmt.Sprintf("started (pid %d, listening on 127.0.0.1:%s -> tenor.local:%s)\n", pid, local, remote); stdout != want {
		t.Errorf("start: stdout %q, want %q", stdout, want)
	}
	logData, err := os.ReadFile(logPath(local))
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if !strings.Contains(string(logData), fmt.Sprintf("relay on 127.0.0.1:%s -> tenor.local:%s", local, remote)) {
		t.Errorf("daemon log %q misses the startup line", logData)
	}

	stdout, _, code = runRelay(t, 15*time.Second, "status", local, remote)
	if code != 0 {
		t.Errorf("status: exit %d, want 0", code)
	}
	if want := fmt.Sprintf("running (pid %d), listening on 127.0.0.1:%s -> tenor.local:%s\n", pid, local, remote); stdout != want {
		t.Errorf("status: stdout %q, want %q", stdout, want)
	}

	stdout, _, code = runRelay(t, 15*time.Second, "start", local, remote)
	if code != 0 {
		t.Errorf("idempotent start: exit %d, want 0", code)
	}
	if want := fmt.Sprintf("already running (pid %d); nothing to do\n", pid); stdout != want {
		t.Errorf("idempotent start: stdout %q, want %q", stdout, want)
	}

	stdout, _, code = runRelay(t, 15*time.Second, "stop", local, remote)
	if code != 0 {
		t.Errorf("stop: exit %d, want 0", code)
	}
	if want := fmt.Sprintf("stopped (pid %d)\n", pid); stdout != want {
		t.Errorf("stop: stdout %q, want %q", stdout, want)
	}

	stdout, _, _ = runRelay(t, 15*time.Second, "status", local, remote)
	if want := "not running\n"; stdout != want {
		t.Errorf("status after stop: stdout %q, want %q", stdout, want)
	}

	stdout, _, _ = runRelay(t, 15*time.Second, "stop", local, remote)
	if want := "not running; nothing to do\n"; stdout != want {
		t.Errorf("second stop: stdout %q, want %q", stdout, want)
	}
}

func TestRelayStartWithoutLog(t *testing.T) {
	local := freeLocalPort(t)
	remote := freeLocalPort(t)
	cleanupRelayState(t, local)

	stdout, stderr, code := runRelay(t, 30*time.Second, "start", local, remote, "--log=false")
	if code != 0 {
		t.Fatalf("start --log=false: exit %d (out %q, err %q)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "started (pid ") {
		t.Errorf("start --log=false: stdout %q misses the started line", stdout)
	}
	if _, err := os.Stat(logPath(local)); !os.IsNotExist(err) {
		t.Errorf("daemon log file exists despite --log=false")
	}
	if _, _, code := runRelay(t, 15*time.Second, "stop", local, remote); code != 0 {
		t.Errorf("stop: exit %d, want 0", code)
	}
}

func TestRelayStartForwardsLogDays(t *testing.T) {
	local := freeLocalPort(t)
	remote := freeLocalPort(t)
	cleanupRelayState(t, local)
	writeStateFile(t, logPath(local), "2020/01/02 15:04:05 ancient line\n")

	stdout, stderr, code := runRelay(t, 30*time.Second, "start", local, remote, "--log-days=1")
	if code != 0 {
		t.Fatalf("start --log-days=1: exit %d (out %q, err %q)", code, stdout, stderr)
	}
	logData, err := os.ReadFile(logPath(local))
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if strings.Contains(string(logData), "ancient line") {
		t.Errorf("daemon kept a line beyond its 1-day retention: %q", logData)
	}
	if !strings.Contains(string(logData), "relay on ") {
		t.Errorf("daemon log %q misses the startup line", logData)
	}
	if _, _, code := runRelay(t, 15*time.Second, "stop", local, remote); code != 0 {
		t.Errorf("stop: exit %d, want 0", code)
	}
}

func TestRelayRestart(t *testing.T) {
	local := freeLocalPort(t)
	remote := freeLocalPort(t)
	cleanupRelayState(t, local)

	stdout, stderr, code := runRelay(t, 30*time.Second, "restart", local, remote)
	if code != 0 {
		t.Fatalf("restart while stopped: exit %d (out %q, err %q)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "started (pid ") {
		t.Errorf("restart while stopped: stdout %q misses the started line", stdout)
	}
	pid1 := readPID(pidPath(local))
	if pid1 == 0 {
		t.Fatalf("restart while stopped: no usable pidfile")
	}

	stdout, _, _ = runRelay(t, 15*time.Second, "status", local, remote)
	if want := fmt.Sprintf("running (pid %d), listening on 127.0.0.1:%s -> tenor.local:%s\n", pid1, local, remote); stdout != want {
		t.Errorf("status: stdout %q, want %q", stdout, want)
	}

	stdout, stderr, code = runRelay(t, 30*time.Second, "restart", local, remote)
	if code != 0 {
		t.Fatalf("restart while running: exit %d (out %q, err %q)", code, stdout, stderr)
	}
	pid2 := readPID(pidPath(local))
	if pid2 == 0 {
		t.Fatalf("restart while running: no usable pidfile")
	}
	if pid2 == pid1 {
		t.Errorf("restart kept pid %d instead of starting a new daemon", pid2)
	}
	if alive(pid1) {
		t.Errorf("old daemon pid %d still alive after restart", pid1)
	}

	stdout, _, _ = runRelay(t, 15*time.Second, "stop", local, remote)
	if !strings.Contains(stdout, fmt.Sprintf("stopped (pid %d)", pid2)) {
		t.Errorf("stop: stdout %q misses stopped (pid %d)", stdout, pid2)
	}
}

func TestRelayStartBindFailure(t *testing.T) {
	local := freeLocalPort(t)
	remote := freeLocalPort(t)
	cleanupRelayState(t, local)

	// A foreign holder of the local port: losing the bind must surface as
	// a start failure, not as a false success, and must leave no pidfile
	// behind.
	holder, err := net.Listen("tcp", "127.0.0.1:"+local)
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer holder.Close()

	stdout, stderr, code := runRelay(t, 30*time.Second, "start", local, remote)
	// The daemon's bind fails, so no pidfile is ever written and start
	// reports failure instead of a false success: the reaped child closes
	// start's childDone within its first poll tick, and the death report
	// carries the log tail naming the bind error.
	if code != 1 {
		t.Errorf("start against a held port: exit %d, want 1 (out %q, err %q)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "daemon died at startup") {
		t.Errorf("start against a held port: stderr %q misses the daemon-death report", stderr)
	}
	if !strings.Contains(stderr, "address already in use") {
		t.Errorf("start against a held port: stderr %q misses the bind failure", stderr)
	}
	logData, err := os.ReadFile(logPath(local))
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if !strings.Contains(string(logData), "address already in use") {
		t.Errorf("daemon log %q misses the bind failure", logData)
	}
	if _, err := os.Stat(pidPath(local)); !os.IsNotExist(err) {
		t.Errorf("pidfile left behind after failed start")
	}
	stdout, _, _ = runRelay(t, 15*time.Second, "status", local, remote)
	if want := "not running\n"; stdout != want {
		t.Errorf("status after failed start: stdout %q, want %q", stdout, want)
	}
}

func TestRelayStartRecoversStalePidfile(t *testing.T) {
	local := freeLocalPort(t)
	remote := freeLocalPort(t)
	cleanupRelayState(t, local)

	dead := deadChildPID(t)
	writeStateFile(t, pidPath(local), strconv.Itoa(dead))

	stdout, stderr, code := runRelay(t, 30*time.Second, "start", local, remote)
	if code != 0 {
		t.Fatalf("start over a stale pidfile: exit %d (out %q, err %q)", code, stdout, stderr)
	}
	pid := readPID(pidPath(local))
	if pid == 0 || pid == dead {
		t.Errorf("pidfile after start = %d, want the fresh daemon pid (stale was %d)", pid, dead)
	}
	if stdout, _, code = runRelay(t, 15*time.Second, "stop", local, remote); code != 0 {
		t.Errorf("stop: exit %d, want 0", code)
	}
}
