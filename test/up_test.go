package test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/circlesac/comlink/internal/session"
	"github.com/circlesac/comlink/internal/sshd"
	"github.com/circlesac/comlink/internal/upstream"
)

// serverUpstream stands in for a comlink server: it echoes text messages on
// any path whose last segment is codex or acp, requires the bearer token
// and records the path and Authorization of every request.
type serverUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	auths []string
}

func newServerUpstream(t *testing.T) *serverUpstream {
	t.Helper()
	u := &serverUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		if last := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]; last != "codex" && last != "acp" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(upstream.ReadLimit)
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *serverUpstream) seen() (paths, auths []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...), append([]string(nil), u.auths...)
}

// sessionEnv is a process environment with a fresh HOME and XDG_STATE_HOME
// and no COMLINK_TOKEN. Both directories are returned.
func sessionEnv(t *testing.T) (env []string, home, state string) {
	t.Helper()
	home, state = t.TempDir(), t.TempDir()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "HOME" && k != "XDG_STATE_HOME" && k != "COMLINK_TOKEN" {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+home, "XDG_STATE_HOME="+state), home, state
}

// upProcess is a running `comlink codex` session.
type upProcess struct {
	cmd    *exec.Cmd
	ready  string // stdout up to and including the ready line
	stderr syncBuffer
	stdout chan string // the lines after the ready line, closed at EOF
}

// syncBuffer collects a child's stderr. Unlike lockedBuffer it does not
// embed bytes.Buffer: exec would reach its ReadFrom around the mutex.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startUp runs `comlink codex --name name --server server` (without --name
// when name is empty) with env plus args and returns once it has printed
// that the name is up. The token comes from $COMLINK_TOKEN unless args carry
// --token-file.
func startUp(t *testing.T, bin string, env []string, name, server string, args ...string) *upProcess {
	t.Helper()
	argv := []string{"codex", "--server", server}
	if name != "" {
		argv = append(argv, "--name", name)
	}
	cmd := exec.Command(bin, append(argv, args...)...)
	cmd.Env = append(append([]string(nil), env...), "COMLINK_TOKEN="+testToken)
	up := &upProcess{cmd: cmd, stdout: make(chan string, 64)}
	cmd.Stderr = &up.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for line := range lines {
		up.ready += line + "\n"
		if line == "Press Ctrl-C to stop." {
			go func() {
				for line := range lines {
					up.stdout <- line
				}
				close(up.stdout)
			}()
			return up
		}
	}
	err = cmd.Wait()
	t.Fatalf("comlink %v exited before it was ready: %v\nstdout: %s\nstderr: %s", args, err, up.ready, up.stderr.String())
	return nil
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// stop sends sig and waits; a session must exit 0 on SIGINT and SIGTERM.
func (up *upProcess) stop(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := up.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	if err := up.cmd.Wait(); err != nil {
		t.Fatalf("comlink codex exited with %v after %v, want 0\nstderr: %s", err, sig, up.stderr.String())
	}
	for range up.stdout { // nothing more is printed; drain to EOF
	}
}

// runBinary runs the built binary with env and returns stdout, stderr and
// the exit code.
func runBinary(t *testing.T, bin string, env []string, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err := cmd.Run()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	if strings.Contains(outBuf.String(), testToken) || strings.Contains(errBuf.String(), testToken) {
		t.Fatalf("comlink %v printed the token", args)
	}
	return outBuf.String(), errBuf.String(), code
}

// sessionDirs returns the names in the run and ssh directories of a state
// directory.
func sessionDirs(t *testing.T, state string) (run, ssh []string) {
	t.Helper()
	for _, d := range []struct {
		dir   string
		names *[]string
	}{{filepath.Join(state, "comlink", "run"), &run}, {filepath.Join(state, "comlink", "ssh"), &ssh}} {
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			*d.names = append(*d.names, e.Name())
		}
	}
	return run, ssh
}

// TestUpRegistersUntilStopped: `codex --name NAME --server URL` writes the
// record, the token file and the ssh config file with private modes and
// nothing else (no temporary files left), prints the alias and that it
// created ~/.ssh/config with the Include line, and removes exactly its files
// on SIGTERM with exit 0, keeping the line.
func TestUpRegistersUntilStopped(t *testing.T) {
	bin := buildComlink(t)
	env, home, state := sessionEnv(t)
	record, token, lock, conf := sessionFiles(state, "agent-a")

	up := startUp(t, bin, env, "agent-a", "wss://agent.example.com/base/")
	wantReady := "agent-a is up (pid " + strconv.Itoa(up.cmd.Process.Pid) + ")\n" +
		"  server:     wss://agent.example.com/base/\n" +
		"  Codex app:  SSH connection \"agent-a\" (ssh agent-a)\n" +
		"Press Ctrl-C to stop.\n"
	if up.ready != wantReady {
		t.Fatalf("stdout:\n%s\nwant:\n%s", up.ready, wantReady)
	}
	line := "Include " + filepath.Join(state, "comlink", "ssh", "*.conf")
	added := "comlink: created " + filepath.Join(home, ".ssh", "config") + " with \"" + line + "\"\n"

	got := readRecord(t, record)
	if want := (session.Record{Name: "agent-a", Server: "wss://agent.example.com/base/", Codex: "wss://agent.example.com/base/codex", PID: up.cmd.Process.Pid}); got != want {
		t.Fatalf("record %+v, want %+v", got, want)
	}
	if b, _ := os.ReadFile(record); strings.Contains(string(b), `"acp"`) {
		t.Fatalf("record carries an ACP endpoint: %s", b)
	}
	for _, path := range []string{record, token, lock, conf} {
		if m := mode(t, path); m != 0o600 {
			t.Errorf("%s mode %o, want 600", path, m)
		}
	}
	for _, d := range []string{filepath.Dir(record), filepath.Dir(conf)} {
		if m := mode(t, d); m != 0o700 {
			t.Errorf("%s mode %o, want 700", d, m)
		}
	}
	if run, ssh := sessionDirs(t, state); strings.Join(run, " ") != "agent-a.json agent-a.lock agent-a.token" || strings.Join(ssh, " ") != "agent-a.conf" {
		t.Fatalf("run holds %v and ssh holds %v, want only the session's files", run, ssh)
	}
	if b, _ := os.ReadFile(token); string(b) != testToken {
		t.Fatalf("token file %q", b)
	}
	block, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(block), testToken) {
		t.Fatal("ssh config contains the token")
	}
	// The ProxyCommand names the binary as os.Executable saw it, which may
	// differ from bin by symlinks (/var vs /private/var on macOS).
	if string(block) != configBlock(t, "agent-a", bin, "codex", "--stdio", "--name", "agent-a") &&
		string(block) != configBlock(t, "agent-a", mustEvalSymlinks(t, bin), "codex", "--stdio", "--name", "agent-a") {
		t.Fatalf("ssh config:\n%s\nwant the block running %s codex --stdio --name agent-a", block, bin)
	}

	up.stop(t, syscall.SIGTERM)
	absent(t, record, token, lock, conf)
	if run, ssh := sessionDirs(t, state); len(run)+len(ssh) != 0 {
		t.Errorf("run holds %v and ssh holds %v after stop", run, ssh)
	}
	// stderr is complete once the process has exited.
	if up.stderr.String() != added {
		t.Fatalf("stderr %q, want only %q", up.stderr.String(), added)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".ssh", "config")); string(b) != line+"\n" {
		t.Fatalf("~/.ssh/config after the session %q, want the Include line kept", b)
	}

	// SIGINT ends it the same way, and a token file is read like elsewhere.
	tokenFile := writeTokenFile(t, 0o600)
	up = startUp(t, bin, env, "agent-b", "ws://127.0.0.1:1", "--token-file", tokenFile)
	if b, _ := os.ReadFile(filepath.Join(state, "comlink", "run", "agent-b.token")); string(b) != testToken {
		t.Fatalf("token file %q", b)
	}
	up.stop(t, os.Interrupt)
	absent(t, filepath.Join(state, "comlink", "run", "agent-b.json"))
}

// TestUpRefusesANameThatIsUp: a second session with the same name exits
// non-zero naming the running pid and leaves the first one's files alone.
func TestUpRefusesANameThatIsUp(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	first := startUp(t, bin, env, "agent-a", "wss://agent.example.com")
	stdout, stderr, code := runBinary(t, bin, env, "", "codex", "--name", "agent-a", "--server", "wss://other.example.com")
	if code == 0 || stdout != "" {
		t.Fatalf("second session: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if want := "comlink: error: session \"agent-a\" is already up (pid " + strconv.Itoa(first.cmd.Process.Pid) + ")\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	record, _, _, _ := sessionFiles(state, "agent-a")
	if got := readRecord(t, record); got.PID != first.cmd.Process.Pid || got.Server != "wss://agent.example.com" {
		t.Fatalf("record changed by the refused session: %+v", got)
	}
	first.stop(t, syscall.SIGTERM)
}

// TestUpReplacesAStaleRecord: when the previous session process is gone
// (killed with SIGKILL so it could not clean up, or its files written by
// hand), a new one removes the stale files, says so, and continues.
func TestUpReplacesAStaleRecord(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	record, token, _, conf := sessionFiles(state, "agent-a")

	crashed := startUp(t, bin, env, "agent-a", "wss://crashed.example.com")
	if err := crashed.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = crashed.cmd.Wait()
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the killed session took its record with it: %v", err)
	}

	up := startUp(t, bin, env, "agent-a", "wss://agent.example.com")
	if got := readRecord(t, record); got.PID != up.cmd.Process.Pid || got.Server != "wss://agent.example.com" {
		t.Fatalf("record %+v, want the new session", got)
	}
	up.stop(t, syscall.SIGTERM)
	if want := "comlink: removed stale session \"agent-a\" (pid " + strconv.Itoa(crashed.cmd.Process.Pid) + " is gone)\n"; !strings.Contains(up.stderr.String(), want) {
		t.Fatalf("stderr %q lacks %q", up.stderr.String(), want)
	}
	absent(t, record, token, conf)

	writeStaleSession(t, state, "agent-a")
	stalePID := readRecord(t, record).PID
	up = startUp(t, bin, env, "agent-a", "wss://agent.example.com")
	if b, _ := os.ReadFile(token); string(b) != testToken {
		t.Fatalf("token file %q, want the new token", b)
	}
	if b, _ := os.ReadFile(conf); !strings.HasPrefix(string(b), "Host agent-a\n") {
		t.Fatalf("ssh config %q, want the new block", b)
	}
	up.stop(t, syscall.SIGTERM)
	if want := "comlink: removed stale session \"agent-a\" (pid " + strconv.Itoa(stalePID) + " is gone)\n"; !strings.Contains(up.stderr.String(), want) {
		t.Fatalf("stderr %q lacks %q", up.stderr.String(), want)
	}
}

// TestListShowsLiveSessions: `codex list` prints each name that is up with
// its pid and server (never the token), removes stale records and says so,
// and prints nothing on stdout when nothing is up.
func TestListShowsLiveSessions(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	stdout, stderr, code := runBinary(t, bin, env, "", "codex", "list")
	if code != 0 || stdout != "" || stderr != "comlink: no sessions are up\n" {
		t.Fatalf("list with nothing up: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	b := startUp(t, bin, env, "agent-b", "wss://b.example.com/")
	a := startUp(t, bin, env, "agent-a", "wss://a.example.com")
	writeStaleSession(t, state, "agent-c")
	stalePID := readRecord(t, filepath.Join(state, "comlink", "run", "agent-c.json")).PID
	stdout, stderr, code = runBinary(t, bin, env, "", "codex", "list")
	want := "agent-a\tup\tpid " + strconv.Itoa(a.cmd.Process.Pid) + "\twss://a.example.com\n" +
		"agent-b\tup\tpid " + strconv.Itoa(b.cmd.Process.Pid) + "\twss://b.example.com/\n"
	if code != 0 || stdout != want {
		t.Fatalf("list: exit %d, stdout %q, want %q; stderr %q", code, stdout, want, stderr)
	}
	if want := "comlink: removed stale session \"agent-c\" (pid " + strconv.Itoa(stalePID) + " is gone)\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	absent(t, filepath.Join(state, "comlink", "run", "agent-c.json"), filepath.Join(state, "comlink", "run", "agent-c.token"), filepath.Join(state, "comlink", "ssh", "agent-c.conf"))

	a.stop(t, syscall.SIGTERM)
	stdout, _, code = runBinary(t, bin, env, "", "codex", "list")
	if want := "agent-b\tup\tpid " + strconv.Itoa(b.cmd.Process.Pid) + "\twss://b.example.com/\n"; code != 0 || stdout != want {
		t.Fatalf("list after a stop: exit %d, stdout %q, want %q", code, stdout, want)
	}
	b.stop(t, syscall.SIGTERM)
}

// TestUpAddsInclude: a session puts the Include line at the top of
// ~/.ssh/config before it registers anything, creating the file when it is
// missing, backing the previous content up once and saying so in one stderr
// line; the line stays after the session ends. When the line is already
// there at top level (before any Host or Match block) in any spelling ssh
// accepts, nothing is written or printed; under a block it does not count. A
// ~/.ssh/config that is a symlink stays one: the file it points to gets the
// line and the backup. A config that cannot be changed stops the session
// before any session file exists.
func TestUpAddsInclude(t *testing.T) {
	bin := buildComlink(t)
	env, home, state := sessionEnv(t)
	config := filepath.Join(home, ".ssh", "config")
	backup := config + ".comlink-bak"
	pattern := filepath.Join(state, "comlink", "ssh", "*.conf")
	line := "Include " + pattern
	// session runs a session to its ready line, stops it and returns its stderr.
	session := func(t *testing.T) string {
		t.Helper()
		up := startUp(t, bin, env, "agent-a", "wss://agent.example.com")
		up.stop(t, syscall.SIGTERM)
		return up.stderr.String()
	}

	if got, want := session(t), "comlink: created "+config+" with \""+line+"\"\n"; got != want {
		t.Fatalf("session without ~/.ssh/config printed %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\n" {
		t.Fatalf("config %q", b)
	}
	if m := mode(t, config); m != 0o600 {
		t.Fatalf("config mode %o, want 600", m)
	}
	if m := mode(t, filepath.Dir(config)); m != 0o700 {
		t.Fatalf("~/.ssh mode %o, want 700", m)
	}
	absent(t, backup)
	if got := session(t); got != "" {
		t.Fatalf("session with the line in place printed %q", got)
	}

	// An existing config: the line goes on top, the rest is byte-identical,
	// the mode is kept and the backup is written once.
	original := "# keep\r\n\nHost *\n\tServerAliveInterval 30 \n\n  Host old\n    HostName old.example.com"
	if err := os.WriteFile(config, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0o640); err != nil {
		t.Fatal(err)
	}
	if got, want := session(t), "comlink: added \""+line+"\" to the top of "+config+"; previous content backed up to "+backup+"\n"; got != want {
		t.Fatalf("session on an existing config printed %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\n"+original {
		t.Fatalf("config %q, want the line on top of the previous content", b)
	}
	if m := mode(t, config); m != 0o640 {
		t.Fatalf("config mode %o, want the original 640", m)
	}
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatalf("backup %q", b)
	}
	// The user removed the line: it is added back and the first backup kept.
	if err := os.WriteFile(config, []byte("Host x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := session(t), "comlink: added \""+line+"\" to the top of "+config+"\n"; got != want {
		t.Fatalf("session after the line was removed printed %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatalf("backup overwritten: %q", b)
	}

	// Spellings ssh accepts at top level count as present: nothing is
	// written or printed.
	for _, content := range []string{
		"include " + pattern + "\n",
		"  Include\t" + pattern + "  \n",
		"Include=" + pattern + "\n",
		"Include = " + pattern + "\n",
		`Include "` + pattern + `"` + "\n",
		"# top\nInclude " + pattern + "\nHost a\n",
	} {
		if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := session(t); got != "" {
			t.Errorf("session with %q printed %q", content, got)
		}
		if b, _ := os.ReadFile(config); string(b) != content {
			t.Errorf("config %q changed to %q", content, b)
		}
	}
	// Under a Host block the line applies only to that block, so it does not
	// count: the line goes on top.
	nested := "Host a\nInclude " + pattern + "\n"
	if err := os.WriteFile(config, []byte(nested), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := session(t), "comlink: added \""+line+"\" to the top of "+config+"\n"; got != want {
		t.Fatalf("session with %q printed %q, want %q", nested, got, want)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\n"+nested {
		t.Fatalf("config %q, want the line on top of %q", b, nested)
	}

	// ~/.ssh/config as a symlink into a dotfiles checkout.
	target := filepath.Join(t.TempDir(), "dotfiles", "ssh_config")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, config); err != nil {
		t.Fatal(err)
	}
	if got, want := session(t), "comlink: added \""+line+"\" to the top of "+config+" -> "+target+"; previous content backed up to "+target+".comlink-bak\n"; got != want {
		t.Fatalf("session through a symlink printed %q, want %q", got, want)
	}
	if dest, err := os.Readlink(config); err != nil || dest != target {
		t.Fatalf("~/.ssh/config is no longer the symlink to %s: %q, %v", target, dest, err)
	}
	if b, _ := os.ReadFile(target); string(b) != line+"\n"+original {
		t.Fatalf("target %q", b)
	}
	if m := mode(t, target); m != 0o644 {
		t.Fatalf("target mode %o, want the original 644", m)
	}
	if b, _ := os.ReadFile(target + ".comlink-bak"); string(b) != original {
		t.Fatalf("target backup %q", b)
	}

	// Configs that cannot be changed: the session fails with one line and
	// leaves no session file.
	refused := func(t *testing.T, wantSuffix string) {
		t.Helper()
		stdout, stderr, code := runBinary(t, bin, append(env, "COMLINK_TOKEN="+testToken), "", "codex", "--name", "agent-a", "--server", "wss://agent.example.com")
		if code == 0 || stdout != "" || strings.Count(stderr, "\n") != 1 ||
			!strings.HasPrefix(stderr, "comlink: error: ") || !strings.HasSuffix(stderr, wantSuffix+"\n") {
			t.Fatalf("session: exit %d, stdout %q, stderr %q, want one error line ending in %q", code, stdout, stderr, wantSuffix)
		}
		if run, ssh := sessionDirs(t, state); len(run)+len(ssh) != 0 {
			t.Fatalf("run holds %v and ssh holds %v after a refused session", run, ssh)
		}
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config", config); err != nil {
		t.Fatal(err)
	}
	refused(t, ": too many levels of symbolic links")
	if dest, err := os.Readlink(config); err != nil || dest != "config" {
		t.Fatalf("looping ~/.ssh/config changed: %q, %v", dest, err)
	}
	if os.Getuid() != 0 {
		// The backup cannot be written beside a config in a read-only directory.
		if err := os.Remove(config); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte("Host y\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(config), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(config), 0o700) })
		refused(t, ": permission denied")
		if b, _ := os.ReadFile(config); string(b) != "Host y\n" {
			t.Fatalf("config changed although the session was refused: %q", b)
		}
	}
}

// sshConfigFor writes the ssh client config the Codex app would use once
// ~/.ssh/config includes comlink's directory: the include line, a fresh
// known_hosts and the app's options.
func sshConfigFor(t *testing.T, state string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	content := "UserKnownHostsFile " + filepath.Join(dir, "known_hosts") + "\n" +
		"BatchMode yes\n" +
		"ExitOnForwardFailure yes\n" +
		"Include " + filepath.Join(state, "comlink", "ssh", "*.conf") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sshByName runs the real ssh client against alias through the included
// session config, with the remote forward the Codex app asks for.
func sshByName(env []string, config, alias, command string) *exec.Cmd {
	cmd := exec.Command("ssh", "-F", config, "-R", "127.0.0.1:0:127.0.0.1:9", alias, "--", command)
	cmd.Env = env
	return cmd
}

// codexRelayByName opens the app-server session on alias through real ssh
// with the included config, as the Codex app does, and checks that one
// message comes back and ssh exits 0 after a normal close.
func codexRelayByName(t *testing.T, env []string, config, alias string) {
	t.Helper()
	cmd := sshByName(env, config, alias, readFixture(t, "codex-app-proxy.txt"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, len(fixtureMarker))
	if _, err := io.ReadFull(stdout, marker); err != nil || !bytes.Equal(marker, fixtureMarker) {
		t.Fatalf("%s: marker %q (%v), want %q\nstderr: %s", alias, marker, err, fixtureMarker, stderr.String())
	}
	var conn net.Conn = &sshd.StdioConn{In: stdout, Out: stdin}
	ws := dialOverConn(t, conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := `{"jsonrpc":"2.0","id":7,"method":"thread/list"}`
	if err := ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	if typ, data, err := ws.Read(ctx); err != nil || typ != websocket.MessageText || string(data) != msg {
		t.Fatalf("%s: round trip: typ=%v data=%q err=%v\nstderr: %s", alias, typ, data, err, stderr.String())
	}
	if err := ws.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ssh %s exited with %v, want 0\nstderr: %s", alias, err, stderr.String())
	}
	if strings.Contains(stderr.String(), testToken) {
		t.Fatal("ssh stderr contains the token")
	}
}

// TestUpCodexStdioByName: while the session runs, the Codex app's ssh
// reaches the alias through the included config and `codex --stdio --name`
// answers the version probe and relays the app-server session to
// <server>/codex with the session's token; after the session stops, ssh
// fails with a line naming the session.
func TestUpCodexStdioByName(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	server := newServerUpstream(t)
	config := sshConfigFor(t, state)
	up := startUp(t, bin, env, "agent-a", wsURLOf(server.Server)+"/prefix/")

	cmd := sshByName(env, config, "agent-a", readFixture(t, "codex-app-version.txt"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || string(out) != "codex-cli 0.165.0\n" {
		t.Fatalf("ssh agent-a: %v, stdout %q\nstderr: %s", err, out, stderr.String())
	}
	if paths, _ := server.seen(); len(paths) != 0 {
		t.Fatalf("the version probe reached the server: %v", paths)
	}

	codexRelayByName(t, env, config, "agent-a")
	if paths, auths := server.seen(); len(paths) != 1 || paths[0] != "/prefix/codex" || auths[0] != "Bearer "+testToken {
		t.Fatalf("server saw paths %v with Authorization %q", paths, auths)
	}

	// After stop the alias is gone from the included directory, so ssh no
	// longer knows the host; a ProxyCommand that still names the session
	// (the block as the app last read it) fails with comlink's line.
	up.stop(t, syscall.SIGTERM)
	cmd = sshByName(env, config, "agent-a", readFixture(t, "codex-app-version.txt"))
	stderr.Reset()
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err == nil || len(out) != 0 || !strings.Contains(stderr.String(), "agent-a") {
		t.Fatalf("ssh after stop: %v, stdout %q, stderr %q", err, out, stderr.String())
	}
	cmd = sshByName(env, config, "comlink@agent-a", readFixture(t, "codex-app-version.txt"))
	cmd.Args = append(cmd.Args[:1], append([]string{"-o", "ProxyCommand=" + quote(bin) + " codex --stdio --name agent-a"}, cmd.Args[1:]...)...)
	stderr.Reset()
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err == nil || len(out) != 0 {
		t.Fatalf("ssh after stop: %v, stdout %q", err, out)
	}
	if want := "comlink: error: session \"agent-a\" is not up: run 'comlink codex --name agent-a --server URL' first\n"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q lacks %q", stderr.String(), want)
	}
	if paths, _ := server.seen(); len(paths) != 1 {
		t.Fatalf("the server was dialled after stop: %v", paths)
	}
}

// TestUpSeveralNames: names up at once each route to their own server
// through real ssh, and a base with a path prefix or a trailing slash maps
// to its /codex.
func TestUpSeveralNames(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not on PATH")
	}
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	config := sshConfigFor(t, state)
	servers := map[string]*serverUpstream{"agent-a": newServerUpstream(t), "agent-b": newServerUpstream(t), "agent-c": newServerUpstream(t)}
	bases := map[string]string{
		"agent-a": wsURLOf(servers["agent-a"].Server),
		"agent-b": wsURLOf(servers["agent-b"].Server) + "/",
		"agent-c": wsURLOf(servers["agent-c"].Server) + "/tenant/x",
	}
	wantPath := map[string]string{"agent-a": "/codex", "agent-b": "/codex", "agent-c": "/tenant/x/codex"}
	wantCodex := map[string]string{
		"agent-a": bases["agent-a"] + "/codex",
		"agent-b": bases["agent-b"] + "codex",
		"agent-c": bases["agent-c"] + "/codex",
	}
	for _, name := range []string{"agent-a", "agent-b", "agent-c"} {
		up := startUp(t, bin, env, name, bases[name])
		defer up.stop(t, syscall.SIGTERM)
	}
	for _, name := range []string{"agent-a", "agent-b", "agent-c"} {
		got := readRecord(t, filepath.Join(state, "comlink", "run", name+".json"))
		if got.Server != bases[name] || got.Codex != wantCodex[name] {
			t.Errorf("%s record %+v, want codex %q", name, got, wantCodex[name])
		}
		codexRelayByName(t, env, config, name)
		paths, auths := servers[name].seen()
		if len(paths) != 1 || paths[0] != wantPath[name] || auths[0] != "Bearer "+testToken {
			t.Errorf("%s: server saw paths %v with Authorization %q, want %q", name, paths, auths, wantPath[name])
		}
	}
	stdout, _, code := runBinary(t, bin, env, "", "codex", "list")
	if code != 0 || strings.Count(stdout, "\tup\t") != 3 {
		t.Fatalf("list: exit %d, stdout %q", code, stdout)
	}
}

// TestUpDefaultName: without --name the session is named after the first
// label of the server's host, lowercased, and says so. Further sessions on
// the same server without --name take padawan-2, padawan-3 and say why;
// all are listed, and once padawan stops a new session takes padawan again.
// A server whose host gives no name (an IP address) gets comlink.
func TestUpDefaultName(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	server := "wss://Padawan.Example.com:8443/tenant/x"

	up := startUp(t, bin, env, "", server)
	pid := strconv.Itoa(up.cmd.Process.Pid)
	want := "padawan is up (pid " + pid + ")\n" +
		"  name:       padawan, the first label of the server's host (--name sets another)\n" +
		"  server:     " + server + "\n" +
		"  Codex app:  SSH connection \"padawan\" (ssh padawan)\n" +
		"Press Ctrl-C to stop.\n"
	if up.ready != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", up.ready, want)
	}
	record, _, _, conf := sessionFiles(state, "padawan")
	if got := readRecord(t, record); got.Name != "padawan" || got.Codex != server+"/codex" {
		t.Fatalf("record %+v", got)
	}
	if b, err := os.ReadFile(conf); err != nil || !strings.HasPrefix(string(b), "Host padawan\n") || !strings.Contains(string(b), " codex --stdio --name padawan\n") {
		t.Fatalf("ssh config %q (%v)", b, err)
	}

	second := startUp(t, bin, env, "", server)
	if want := "padawan-2 is up (pid " + strconv.Itoa(second.cmd.Process.Pid) + ")\n" +
		"  name:       padawan-2 (padawan is already up; --name sets another)\n"; !strings.HasPrefix(second.ready, want) {
		t.Fatalf("second session without --name:\n%s\nwant prefix:\n%s", second.ready, want)
	}
	third := startUp(t, bin, env, "", server)
	if want := "padawan-3 is up (pid " + strconv.Itoa(third.cmd.Process.Pid) + ")\n" +
		"  name:       padawan-3 (padawan is already up, padawan-2 is already up; --name sets another)\n"; !strings.HasPrefix(third.ready, want) {
		t.Fatalf("third session without --name:\n%s\nwant prefix:\n%s", third.ready, want)
	}
	stdout, _, code := runBinary(t, bin, env, "", "codex", "list")
	if code != 0 || !strings.HasPrefix(stdout, "padawan\tup\t") || !strings.Contains(stdout, "\npadawan-2\tup\t") || !strings.Contains(stdout, "\npadawan-3\tup\t") {
		t.Fatalf("list: exit %d, stdout %q", code, stdout)
	}
	up.stop(t, os.Interrupt)
	again := startUp(t, bin, env, "", server)
	if !strings.HasPrefix(again.ready, "padawan is up (pid ") || !strings.Contains(again.ready, "  name:       padawan, the first label") {
		t.Fatalf("session after padawan stopped:\n%s", again.ready)
	}

	ip := startUp(t, bin, env, "", "ws://127.0.0.1:8787")
	if want := "comlink is up (pid " + strconv.Itoa(ip.cmd.Process.Pid) + ")\n" +
		"  name:       comlink (the server host 127.0.0.1 is an IP address; --name sets another)\n"; !strings.HasPrefix(ip.ready, want) {
		t.Fatalf("session for an IP address:\n%s\nwant prefix:\n%s", ip.ready, want)
	}
	for _, p := range []*upProcess{ip, again, third, second} {
		p.stop(t, syscall.SIGTERM)
	}
	if run, ssh := sessionDirs(t, state); len(run) != 0 || len(ssh) != 0 {
		t.Fatalf("run holds %v and ssh holds %v after all sessions stopped", run, ssh)
	}
}

// TestUpExistingSSHHost: with a ~/.ssh/config that has a Host block for
// the derived name, a session without --name takes padawan-2, says why and
// writes nothing for padawan; with --name padawan it warns once, naming the
// Host line, and comes up. A pattern that matches every suffix
// (Host padawan*) fails with one line asking for --name and writes nothing.
func TestUpExistingSSHHost(t *testing.T) {
	bin := buildComlink(t)
	env, home, state := sessionEnv(t)
	config := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}

	wildcard := "Host padawan*\n  User someone\n"
	if err := os.WriteFile(config, []byte(wildcard), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runBinary(t, bin, append(env, "COMLINK_TOKEN="+testToken), "", "codex", "--server", "wss://padawan.example.com")
	want := `comlink: error: "padawan" and "padawan-2" both have ssh configuration, so a pattern in it likely matches every padawan-N; pass --name NAME` + "\n"
	if code == 0 || stdout != "" || stderr != want {
		t.Fatalf("session without --name under Host padawan*: exit %d, stdout %q, stderr %q, want %q", code, stdout, stderr, want)
	}
	if b, _ := os.ReadFile(config); string(b) != wildcard {
		t.Fatalf("~/.ssh/config changed to %q", b)
	}
	if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
		t.Fatalf("state holds %v (%v) after a refused session", entries, err)
	}
	absent(t, config+".comlink-bak")

	if err := os.WriteFile(config, []byte("Host padawan\n  User someone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := startUp(t, bin, env, "", "wss://padawan.example.com")
	if want := "padawan-2 is up (pid " + strconv.Itoa(up.cmd.Process.Pid) + ")\n" +
		"  name:       padawan-2 (padawan has ssh configuration; --name sets another)\n"; !strings.HasPrefix(up.ready, want) {
		t.Fatalf("session without --name under Host padawan:\n%s\nwant prefix:\n%s", up.ready, want)
	}
	if strings.Contains(up.stderr.String(), "warning") {
		t.Fatalf("derived session warned: %q", up.stderr.String())
	}
	if _, ssh := sessionDirs(t, state); len(ssh) != 1 || ssh[0] != "padawan-2.conf" {
		t.Fatalf("ssh holds %v, want only padawan-2.conf", ssh)
	}
	absent(t, filepath.Join(state, "comlink", "run", "padawan.json"))
	up.stop(t, syscall.SIGTERM)

	up = startUp(t, bin, env, "padawan", "wss://padawan.example.com")
	up.stop(t, syscall.SIGTERM)
	if !strings.HasPrefix(up.stderr.String(), `comlink: warning: "padawan" already has ssh configuration (the Host line at `+config+`:2 matches it); this session's Host block takes precedence while it runs`+"\n") ||
		strings.Count(up.stderr.String(), "warning") != 1 {
		t.Fatalf("session with --name padawan printed %q", up.stderr.String())
	}
}

// TestCodexHelp: `comlink codex --help` lists the flags of the session and
// of --stdio and its subcommands, and its usage line and the top-level
// command list say that a subcommand is optional.
func TestCodexHelp(t *testing.T) {
	bin := buildComlink(t)
	env, _, _ := sessionEnv(t)
	env = append(env, "COLUMNS=1000") // one line per flag, so the phrases below are not split
	stdout, stderr, code := runBinary(t, bin, env, "", "codex", "--help")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "Usage: comlink codex [<command>] [flags]\n") {
		t.Fatalf("codex --help: exit %d, stderr %q, stdout:\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"--stdio ", "via a Host block's ProxyCommand; you normally never run it yourself.",
		"--name=NAME ", "--server=URL ", "--token-file=PATH ", "--report-version=VERSION ",
		"codex list",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("codex --help lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "proxy") || strings.Contains(stdout, "codex up") || strings.Contains(stdout, "ssh-include") || strings.Contains(stdout, "required") {
		t.Errorf("codex --help names a removed command or a required flag:\n%s", stdout)
	}
	stdout, _, code = runBinary(t, bin, env, "", "--help")
	if code != 0 || !strings.Contains(stdout, "\n  codex [<command>] [flags]\n") {
		t.Fatalf("--help: exit %d, stdout:\n%s", code, stdout)
	}
}

// TestUpRejectsBadArguments: a session refuses an invalid name or server
// before writing anything and needs --server; --report-version needs --stdio; `codex --stdio` takes exactly one of
// --name and --server, and --token-file only with --server; list refuses
// the flags of `codex` itself; `acp` takes --server and nothing that names a
// session; the removed commands and flags are unknown. None of them writes
// to the state directory or ~/.ssh.
func TestUpRejectsBadArguments(t *testing.T) {
	bin := buildComlink(t)
	env, home, state := sessionEnv(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"codex", "--name", "agent\nHost *", "--server", "wss://agent.example.com"}, `comlink: error: invalid alias "agent\nHost *": use only letters, digits, '-', '_' and '.'`},
		{[]string{"codex", "--name", "agent", "--server", "https://agent.example.com"}, `comlink: error: server URL "https://agent.example.com": want ws://host[/prefix] or wss://host[/prefix]`},
		{[]string{"codex", "--name", "agent", "--server", "wss://agent.example.com", "--token-file", filepath.Join(state, "absent")}, "comlink: error: read token file: "},
		{[]string{"codex", "--name", "agent"}, "comlink: error: missing flags: --server=URL"},
		{[]string{"codex"}, "comlink: error: missing flags: --server=URL"},
		{[]string{"codex", "--token-file", "/dev/null"}, "comlink: error: missing flags: --server=URL"},
		{[]string{"codex", "--name", "a", "--server", "ws://127.0.0.1:1", "--report-version", "0.200.1"}, "comlink: error: --report-version goes with --stdio"},
		{[]string{"codex", "--stdio"}, "comlink: error: --stdio needs --name NAME (a session from 'comlink codex --name NAME') or --server URL"},
		{[]string{"codex", "--stdio", "--token-file", "/dev/null"}, "comlink: error: --stdio needs --name NAME (a session from 'comlink codex --name NAME') or --server URL"},
		{[]string{"codex", "--stdio", "--name", "a", "--server", "ws://127.0.0.1:1"}, "comlink: error: --stdio takes --name or --server, not both"},
		{[]string{"codex", "--stdio", "--name", "a", "--token-file", "/dev/null"}, "comlink: error: --token-file goes with --server; a session's token comes from 'comlink codex --name NAME'"},
		{[]string{"codex", "--stdio", "--name", "two words"}, `comlink: error: invalid name "two words": use only letters, digits, '-', '_' and '.'`},
		{[]string{"codex", "--stdio", "--server", "https://agent.example.com"}, `comlink: error: server URL "https://agent.example.com": want ws://host[/prefix] or wss://host[/prefix]`},
		{[]string{"codex", "--stdio", "--upstream", "ws://127.0.0.1:1"}, "comlink: error: unknown flag --upstream"},
		{[]string{"codex", "list", "--name", "a"}, "comlink: error: 'comlink codex list' takes no flags; --stdio, --name, --server, --token-file and --report-version go with 'comlink codex' itself"},
		{[]string{"codex", "ssh-include"}, "comlink: error: unexpected argument ssh-include"},
		{[]string{"codex", "proxy", "--name", "a"}, "comlink: error: unexpected argument proxy"},
		{[]string{"codex", "up", "--name", "a", "--server", "ws://127.0.0.1:1"}, "comlink: error: unexpected argument up"},
		{[]string{"acp"}, "comlink: error: missing flags: --server=URL"},
		{[]string{"acp", "--name", "a"}, "comlink: error: unknown flag --name"},
		{[]string{"acp", "--upstream", "ws://127.0.0.1:1/acp"}, "comlink: error: unknown flag --upstream"},
		{[]string{"acp", "--server", "https://agent.example.com"}, `comlink: error: server URL "https://agent.example.com": want ws://host[/prefix] or wss://host[/prefix]`},
		{[]string{"acp", "--server", "ws://[::1"}, `comlink: error: server URL: parse "ws://[::1": missing ']' in host`},
		{[]string{"up", "--name", "a", "--server", "ws://127.0.0.1:1"}, "comlink: error: unexpected argument up"},
		{[]string{"list"}, "comlink: error: unexpected argument list"},
		{[]string{"ssh-include"}, "comlink: error: unexpected argument ssh-include"},
	} {
		// kong prints the usage on stdout for a flag error; nothing else goes there.
		stdout, stderr, code := runBinary(t, bin, env, "", tc.args...)
		if code == 0 || (stdout != "" && !strings.HasPrefix(stdout, "Usage: comlink ")) || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q, want %q", tc.args, code, stdout, stderr, tc.want)
		}
	}
	if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
		t.Fatalf("state directory holds %v (%v) after refused commands", entries, err)
	}
	absent(t, filepath.Join(home, ".ssh"))
}
