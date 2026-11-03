package main

// Tests for the completion surface and the status-all fan-out: the hidden
// __complete machinery and its cobra-style word split, the shell completion
// scripts, the pidfile's port-pair record they read, and statusAll's
// per-relay lines and exit rule. Completion and the fan-out read the state
// dir, so these tests point stateDir at a scratch dir — the shared host
// state dir belongs to the fleet and must never be listed, probed or
// cleaned up by a test. The exit paths (completion usage, statusAll's
// exit 1) run through the re-executed test process via the
// RELAY_TEST_EXIT_CASE harness in main_test.go.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// useScratchStateDir points stateDir at a fresh scratch dir for the test's
// duration and returns its path, so completion and fan-out tests list and
// probe only state they wrote themselves.
func useScratchStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	saved := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = saved })
	return dir
}

func TestReadState(t *testing.T) {
	useScratchStateDir(t)
	path := pidPath("8080")
	cases := []struct {
		name       string
		content    string
		wantPID    int
		wantRemote string
	}{
		// serve writes "<pid> <local> <remote>"; completion and status all
		// take the mapping from the last field.
		{"current format", "4321 8080 11434", 4321, "11434"},
		{"current format with newline", "4321 8080 11434\n", 4321, "11434"},
		// Old fleets carry single-number pidfiles; they still load, with no
		// mapping to offer.
		{"legacy single number", "4321", 4321, ""},
		{"pid plus one field", "4321 8080", 4321, ""},
		{"garbage", "not-a-pid 8080 11434", 0, ""},
		{"empty file", "", 0, ""},
		{"zero pid", "0 8080 11434", 0, ""},
		{"negative pid", "-5 8080 11434", 0, ""},
	}
	for _, tt := range cases {
		writeStateFile(t, path, tt.content)
		st := readState(path)
		if st.pid != tt.wantPID || st.remote != tt.wantRemote {
			t.Errorf("%s: readState = {%d %q}, want pid %d remote %q", tt.name, st.pid, st.remote, tt.wantPID, tt.wantRemote)
		}
	}
	if st := readState(pidPath("9999")); st != (relayState{}) {
		t.Errorf("missing file: readState = %+v, want the zero state", st)
	}
}

func TestKnownRelays(t *testing.T) {
	dir := useScratchStateDir(t)
	for name, content := range map[string]string{
		"relay-9.pid":      "1 9 2",
		"relay-10.pid":     "1 10 2",
		"relay-10000.pid":  "1 10000 2",
		"relay-abc.pid":    "1 abc 2",
		"relay-.pid":       "1  2",
		"relay-8080.log":   "log text",
		"relay.pid":        "1 2 3",
		"notes.txt":        "unrelated",
	} {
		writeStateFile(t, filepath.Join(dir, name), content)
	}
	// Sorted as strings: the port rides on the filename, and even a stale,
	// legacy-format or garbage pidfile is listed — only the name matters.
	if got, want := knownRelays(), []string{"10", "10000", "9"}; !reflect.DeepEqual(got, want) {
		t.Errorf("knownRelays = %#v, want %#v", got, want)
	}

	// No state dir: nothing known.
	stateDir = filepath.Join(dir, "absent")
	if got := knownRelays(); got != nil {
		t.Errorf("knownRelays without a state dir = %#v, want nil", got)
	}
}

func TestComplete(t *testing.T) {
	useScratchStateDir(t)
	for port, content := range map[string]string{
		"8080": "4321 8080 11434",
		"9000": "4321 9000 5555",
		"7000": "999", // legacy pidfile: no mapping to offer
	} {
		writeStateFile(t, pidPath(port), content)
	}

	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		// First word: the subcommands. The hidden __complete is never
		// offered, and dash-prefixed words complete the global flags.
		{"nothing typed", []string{""}, []string{"start", "stop", "restart", "status", "serve", "version", "completion"}},
		{"no words at all", nil, []string{"start", "stop", "restart", "status", "serve", "version", "completion"}},
		{"subcommand prefix", []string{"st"}, []string{"start", "stop", "status"}},
		{"unique subcommand prefix", []string{"ver"}, []string{"version"}},
		{"hidden subcommand never offered", []string{"__complete"}, nil},
		{"unknown subcommand prefix", []string{"frobnicate"}, nil},
		{"global flags", []string{"-"}, []string{"-v", "--version"}},
		{"global flag double dash", []string{"--"}, []string{"--version"}},
		{"complete subcommand word", []string{"status"}, []string{"status"}},

		// First positional: the known local ports, with the all fan-out
		// offered for status ahead of them.
		{"status first positional", []string{"status", ""}, []string{"all", "7000", "8080", "9000"}},
		{"status all prefix", []string{"status", "a"}, []string{"all"}},
		{"status port prefix", []string{"status", "8"}, []string{"8080"}},
		{"status no match", []string{"status", "x"}, nil},
		{"start first positional", []string{"start", ""}, []string{"7000", "8080", "9000"}},
		{"flags are not positionals", []string{"start", "--log=false", ""}, []string{"7000", "8080", "9000"}},

		// Second positional: the typed relay's recorded remote port —
		// including after a flag — and nothing for a legacy pidfile.
		{"stop remote", []string{"stop", "8080", ""}, []string{"11434"}},
		{"restart remote prefix", []string{"restart", "9000", "5"}, []string{"5555"}},
		{"status remote", []string{"status", "8080", ""}, []string{"11434"}},
		{"remote after flag", []string{"start", "--log=false", "8080", ""}, []string{"11434"}},
		{"legacy pidfile has no remote", []string{"start", "7000", ""}, nil},
		{"unknown relay has no remote", []string{"stop", "1234", ""}, nil},
		{"status all takes no second positional", []string{"status", "all", ""}, nil},
		{"beyond two positionals", []string{"start", "8080", "11434", ""}, nil},

		// start/serve take the log flags; the other lifecycle commands
		// take none.
		{"start flags", []string{"start", "--"}, []string{"--log=true", "--log=false", "--log-days="}},
		{"serve flag prefix", []string{"serve", "--log"}, []string{"--log=true", "--log=false", "--log-days="}},
		{"start days prefix", []string{"start", "--log-d"}, []string{"--log-days="}},
		{"stop takes no flags", []string{"stop", "--"}, nil},
		{"restart takes no flags", []string{"restart", "--l"}, nil},
		{"status takes no flags", []string{"status", "--"}, nil},

		// completion takes one shell name; other commands take nothing.
		{"completion shells", []string{"completion", ""}, []string{"bash", "zsh"}},
		{"completion shell prefix", []string{"completion", "b"}, []string{"bash"}},
		{"completion after shell", []string{"completion", "zsh", ""}, nil},
		{"version takes no arguments", []string{"version", ""}, nil},
	}
	for _, tt := range cases {
		if got := complete(tt.words); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: complete(%q) = %#v, want %#v", tt.name, tt.words, got, tt.want)
		}
	}
}

func TestCompleteDispatch(t *testing.T) {
	useScratchStateDir(t)
	writeStateFile(t, pidPath("8080"), "4321 8080 11434")
	writeStateFile(t, pidPath("9000"), "4321 9000 5555")

	saved := os.Args
	defer func() { os.Args = saved }()
	for _, tt := range []struct {
		argv []string
		want string
	}{
		// One candidate per line: the format the scripts load with mapfile
		// and the (f) split.
		{[]string{"relay", "__complete", "status", ""}, "all\n8080\n9000\n"},
		{[]string{"relay", "__complete", "start", "8080", ""}, "11434\n"},
		{[]string{"relay", "__complete", "frobnicate", ""}, ""},
	} {
		os.Args = tt.argv
		if out := captureStdout(t, func() { main() }); out != tt.want {
			t.Errorf("relay %q printed %q, want %q", tt.argv[1:], out, tt.want)
		}
	}
}

func TestCompletionScripts(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()

	// Dispatch prints exactly the named shell's script.
	os.Args = []string{"relay", "completion", "bash"}
	if out := captureStdout(t, func() { main() }); out != bashCompletionScript {
		t.Errorf("completion bash printed %q, want the bash script", out)
	}
	os.Args = []string{"relay", "completion", "zsh"}
	if out := captureStdout(t, func() { main() }); out != zshCompletionScript {
		t.Errorf("completion zsh printed %q, want the zsh script", out)
	}

	// The scripts' contract: feed every typed word — current one included —
	// to the hidden __complete and load its newline-separated output. In
	// zsh the substitution inside the (f) split must stay quoted or every
	// candidate collapses into one element, and the count guard keeps an
	// empty result from offering the word list.
	if !strings.Contains(bashCompletionScript, `mapfile -t COMPREPLY < <(relay __complete "${COMP_WORDS[@]:1}")`) {
		t.Errorf("bash script does not feed the typed words to __complete: %q", bashCompletionScript)
	}
	if !strings.Contains(bashCompletionScript, "complete -F __relay_complete relay") {
		t.Errorf("bash script does not register the completion function: %q", bashCompletionScript)
	}
	for _, frag := range []string{
		`candidates=(${(f)"$(relay __complete "${(@)words[2,-1]}")"})`,
		"(( ${#candidates} ))",
		"compadd --",
		"compdef _relay relay",
	} {
		if !strings.Contains(zshCompletionScript, frag) {
			t.Errorf("zsh script misses %q: %q", frag, zshCompletionScript)
		}
	}
}

func TestCompletionUsageErrors(t *testing.T) {
	cases := []struct {
		name   string
		wantIn string
	}{
		{"completion-no-args", "usage: relay"},
		{"completion-unknown-shell", "usage: relay"},
		{"completion-extra-arg", "usage: relay"},
	}
	for _, tt := range cases {
		code, out := exitCase(t, tt.name)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (output %q)", tt.name, code, out)
		}
		if !strings.Contains(out, tt.wantIn) {
			t.Errorf("%s: output %q misses %q", tt.name, out, tt.wantIn)
		}
	}
}

func TestStatusAll(t *testing.T) {
	t.Run("empty state dir prints nothing", func(t *testing.T) {
		useScratchStateDir(t)
		if out := captureStdout(t, statusAll); out != "" {
			t.Errorf("statusAll printed %q, want nothing", out)
		}
	})

	t.Run("one port-prefixed line per relay", func(t *testing.T) {
		useScratchStateDir(t)
		local := freeLocalPort(t)
		ln, err := net.Listen("tcp", "127.0.0.1:"+local)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		writeStateFile(t, pidPath(local), fmt.Sprintf("%d %s 11434", os.Getpid(), local))
		dead := deadChildPID(t)
		writeStateFile(t, pidPath("9000"), strconv.Itoa(dead))
		// A pidfile with an unusable pid: listed, reported not running,
		// and left in place for its owner to clean up.
		writeStateFile(t, pidPath("7000"), "garbage")

		out := captureStdout(t, statusAll)
		got := strings.Split(out, "\n")
		sort.Strings(got)
		want := []string{
			"7000: not running",
			fmt.Sprintf("%s: running (pid %d), listening on 127.0.0.1:%s -> tenor.local:11434", local, os.Getpid(), local),
			fmt.Sprintf("9000: stale pidfile (pid %d gone); cleaned up", dead),
			"",
		}
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("statusAll printed %q, want the lines %#v (order follows knownRelays' sort)", out, want)
		}
		if _, err := os.Stat(pidPath("9000")); !os.IsNotExist(err) {
			t.Errorf("stale pidfile not removed by the fan-out probe")
		}
		if _, err := os.Stat(pidPath("7000")); err != nil {
			t.Errorf("unusable pidfile removed by the fan-out probe: %v", err)
		}
	})
}

func TestStatusAllBrokenExitsOne(t *testing.T) {
	// A relay whose daemon is alive but whose listener is gone is the one
	// failing state: the fan-out exit rule reports it and exits 1. The
	// exit happens in a re-executed test process (RELAY_TEST_EXIT_CASE
	// harness); the parent prepares the scratch state dir so the child
	// probes state nobody else owns. Nothing listens on the port, and the
	// parent's pid is alive from the child's perspective.
	dir := t.TempDir()
	port := freeLocalPort(t)
	writeStateFile(t, filepath.Join(dir, "relay-"+port+".pid"), fmt.Sprintf("%d %s 2", os.Getpid(), port))

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"RELAY_TEST_EXIT_CASE=status-all-broken",
		"RELAY_TEST_STATE_DIR="+dir,
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var eerr *exec.ExitError
		if !errors.As(err, &eerr) {
			t.Fatalf("re-exec for status-all-broken: %v", err)
		}
		code = eerr.ExitCode()
	}
	if code != 1 {
		t.Errorf("status all with a broken relay: exit %d, want 1 (output %q)", code, out.String())
	}
	want := fmt.Sprintf("%s: running (pid %d) — NOT LISTENING on 127.0.0.1:%s (check %s)\n",
		port, os.Getpid(), port, filepath.Join(dir, "relay-"+port+".log"))
	if out.String() != want {
		t.Errorf("status all with a broken relay printed %q, want %q", out.String(), want)
	}
}

func TestStatusAllDispatch(t *testing.T) {
	// `status all` must take the fan-out branch, not the two-port parser
	// (which would reject "all" as an invalid port and exit 2, killing
	// this test process).
	useScratchStateDir(t)
	saved := os.Args
	defer func() { os.Args = saved }()
	os.Args = []string{"relay", "status", "all"}
	if out := captureStdout(t, func() { main() }); out != "" {
		t.Errorf("status all over an empty state dir printed %q, want nothing", out)
	}
}

func TestRelayPidfileCompletionRoundTrip(t *testing.T) {
	// End to end: the real daemon records its port pair in the pidfile and
	// the real binary offers that mapping as the second-positional
	// candidate. This reads only the test's own relay state.
	local := freeLocalPort(t)
	remote := freeLocalPort(t)
	cleanupRelayState(t, local)

	stdout, stderr, code := runRelay(t, 30*time.Second, "start", local, remote)
	if code != 0 {
		t.Fatalf("start: exit %d (out %q, err %q)", code, stdout, stderr)
	}
	st := readState(pidPath(local))
	if st.pid == 0 {
		t.Fatalf("start: no usable pidfile")
	}
	if st.remote != remote {
		t.Errorf("pidfile remote = %q, want %q (completion and status all read the mapping from here)", st.remote, remote)
	}

	stdout, _, code = runRelay(t, 15*time.Second, "__complete", "start", local, "")
	if code != 0 {
		t.Errorf("__complete: exit %d, want 0", code)
	}
	if want := remote + "\n"; stdout != want {
		t.Errorf("__complete second positional printed %q, want %q", stdout, want)
	}

	if _, _, code = runRelay(t, 15*time.Second, "stop", local, remote); code != 0 {
		t.Errorf("stop: exit %d, want 0", code)
	}
	stdout, _, _ = runRelay(t, 15*time.Second, "__complete", "start", local, "")
	if stdout != "" {
		t.Errorf("__complete after stop printed %q, want nothing (the pidfile is gone)", stdout)
	}
}
