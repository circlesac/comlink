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

// upProcess is a running `comlink up`.
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

// startUp runs `comlink up --name name --server server` with env plus args
// and returns once it has printed that the name is up. The token comes from
// $COMLINK_TOKEN unless args carry --token-file.
func startUp(t *testing.T, bin string, env []string, name, server string, args ...string) *upProcess {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"up", "--name", name, "--server", server}, args...)...)
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
	t.Fatalf("comlink up %s exited before it was ready: %v\nstdout: %s\nstderr: %s", name, err, up.ready, up.stderr.String())
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

// stop sends sig and waits; `up` must exit 0 on SIGINT and SIGTERM.
func (up *upProcess) stop(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := up.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	if err := up.cmd.Wait(); err != nil {
		t.Fatalf("comlink up exited with %v after %v, want 0\nstderr: %s", err, sig, up.stderr.String())
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

// TestUpRegistersUntilStopped: `up` writes the record, the token file and
// the ssh config file with private modes, prints what is available and the
// hint to run ssh-include, and removes exactly its files on SIGTERM with
// exit 0.
func TestUpRegistersUntilStopped(t *testing.T) {
	bin := buildComlink(t)
	env, home, state := sessionEnv(t)
	record, token, lock, conf := sessionFiles(state, "agent-a")

	up := startUp(t, bin, env, "agent-a", "wss://agent.example.com/base/")
	// The agent command names the binary as os.Executable saw it, which may
	// differ from bin by symlinks (/var vs /private/var on macOS).
	_, after, ok := strings.Cut(up.ready, "agent command ")
	exe, _, _ := strings.Cut(after, " acp --name agent-a\n")
	if gotExe, _ := filepath.EvalSymlinks(exe); !ok || gotExe == "" || gotExe != mustEvalSymlinks(t, bin) {
		t.Fatalf("stdout names executable %q, want %s:\n%s", exe, bin, up.ready)
	}
	wantReady := "agent-a is up (pid " + strconv.Itoa(up.cmd.Process.Pid) + ")\n" +
		"  server:       wss://agent.example.com/base/\n" +
		"  Codex app:    SSH connection \"agent-a\" (ssh agent-a)\n" +
		"  ACP clients:  agent command " + exe + " acp --name agent-a\n" +
		"Press Ctrl-C to stop.\n"
	if up.ready != wantReady {
		t.Fatalf("stdout:\n%s\nwant:\n%s", up.ready, wantReady)
	}
	hint := "comlink: " + filepath.Join(home, ".ssh", "config") + " does not include " + filepath.Join(state, "comlink", "ssh", "*.conf") + "; run 'comlink ssh-include' once so the Codex app can use the alias\n"

	got := readRecord(t, record)
	want := struct{ Name, Server, Codex, ACP string }{"agent-a", "wss://agent.example.com/base/", "wss://agent.example.com/base/codex", "wss://agent.example.com/base/acp"}
	if got.Name != want.Name || got.Server != want.Server || got.Codex != want.Codex || got.ACP != want.ACP || got.PID != up.cmd.Process.Pid {
		t.Fatalf("record %+v, want %+v with pid %d", got, want, up.cmd.Process.Pid)
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
	wantBlock, err := sshd.ConfigBlock("agent-a", []string{exe, "codex", "proxy", "--name", "agent-a"})
	if err != nil {
		t.Fatal(err)
	}
	if string(block) != wantBlock {
		t.Fatalf("ssh config:\n%s\nwant:\n%s", block, wantBlock)
	}

	up.stop(t, syscall.SIGTERM)
	absent(t, record, token, lock, conf)
	for _, d := range []string{filepath.Dir(record), filepath.Dir(conf)} {
		if entries, err := os.ReadDir(d); err != nil || len(entries) != 0 {
			t.Errorf("%s holds %v (%v) after stop", d, entries, err)
		}
	}
	// stderr is complete once the process has exited.
	if up.stderr.String() != hint {
		t.Fatalf("stderr %q, want only the hint", up.stderr.String())
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

// TestUpRefusesANameThatIsUp: a second `up` with the same name exits
// non-zero naming the running pid and leaves the first one's files alone.
func TestUpRefusesANameThatIsUp(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	first := startUp(t, bin, env, "agent-a", "wss://agent.example.com")
	stdout, stderr, code := runBinary(t, bin, env, "", "up", "--name", "agent-a", "--server", "wss://other.example.com")
	if code == 0 || stdout != "" {
		t.Fatalf("second up: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if want := "comlink: error: session \"agent-a\" is already up (pid " + strconv.Itoa(first.cmd.Process.Pid) + ")\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
	record, _, _, _ := sessionFiles(state, "agent-a")
	if got := readRecord(t, record); got.PID != first.cmd.Process.Pid || got.Server != "wss://agent.example.com" {
		t.Fatalf("record changed by the refused up: %+v", got)
	}
	first.stop(t, syscall.SIGTERM)
}

// TestUpReplacesAStaleRecord: when the previous `up` is gone (killed with
// SIGKILL so it could not clean up, or its files written by hand), `up`
// removes the stale files, says so, and continues.
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
		t.Fatalf("the killed up took its record with it: %v", err)
	}

	up := startUp(t, bin, env, "agent-a", "wss://agent.example.com")
	if got := readRecord(t, record); got.PID != up.cmd.Process.Pid || got.Server != "wss://agent.example.com" {
		t.Fatalf("record %+v, want the new up", got)
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

// TestListShowsLiveSessions: `list` prints each name that is up with its
// pid and server (never the token), removes stale records and says so, and
// prints nothing on stdout when nothing is up.
func TestListShowsLiveSessions(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	stdout, stderr, code := runBinary(t, bin, env, "", "list")
	if code != 0 || stdout != "" || stderr != "comlink: no sessions are up\n" {
		t.Fatalf("list with nothing up: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	b := startUp(t, bin, env, "agent-b", "wss://b.example.com/")
	a := startUp(t, bin, env, "agent-a", "wss://a.example.com")
	writeStaleSession(t, state, "agent-c")
	stalePID := readRecord(t, filepath.Join(state, "comlink", "run", "agent-c.json")).PID
	stdout, stderr, code = runBinary(t, bin, env, "", "list")
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
	stdout, _, code = runBinary(t, bin, env, "", "list")
	if want := "agent-b\tup\tpid " + strconv.Itoa(b.cmd.Process.Pid) + "\twss://b.example.com/\n"; code != 0 || stdout != want {
		t.Fatalf("list after a stop: exit %d, stdout %q, want %q", code, stdout, want)
	}
	b.stop(t, syscall.SIGTERM)
}

// TestSSHIncludeCommand: `ssh-include` creates ~/.ssh/config with the
// Include line, or puts it at the top of an existing one, backing the
// previous content up once; running it again changes nothing; `up` stops
// printing its hint afterwards.
func TestSSHIncludeCommand(t *testing.T) {
	bin := buildComlink(t)
	env, home, state := sessionEnv(t)
	config := filepath.Join(home, ".ssh", "config")
	backup := config + ".comlink-bak"
	line := "Include " + filepath.Join(state, "comlink", "ssh", "*.conf")

	stdout, stderr, code := runBinary(t, bin, env, "", "ssh-include")
	if code != 0 || stdout != "created "+config+" with \""+line+"\"\n" || stderr != "" {
		t.Fatalf("ssh-include: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\n" {
		t.Fatalf("config %q", b)
	}
	if m := mode(t, config); m != 0o600 {
		t.Fatalf("config mode %o, want 600", m)
	}
	absent(t, backup)
	stdout, _, code = runBinary(t, bin, env, "", "ssh-include")
	if code != 0 || stdout != config+" already includes "+filepath.Join(state, "comlink", "ssh", "*.conf")+"; nothing changed\n" {
		t.Fatalf("ssh-include again: exit %d, stdout %q", code, stdout)
	}

	original := "Host old\n  HostName old.example.com\n"
	if err := os.WriteFile(config, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, code = runBinary(t, bin, env, "", "ssh-include")
	if want := "added \"" + line + "\" to the top of " + config + "\nprevious content backed up to " + backup + "\n"; code != 0 || stdout != want {
		t.Fatalf("ssh-include on an existing config: exit %d, stdout %q, want %q", code, stdout, want)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\n"+original {
		t.Fatalf("config %q", b)
	}
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatalf("backup %q", b)
	}
	before, _ := os.ReadFile(config)
	stdout, _, code = runBinary(t, bin, env, "", "ssh-include")
	if after, _ := os.ReadFile(config); code != 0 || !bytes.Equal(before, after) || !strings.HasSuffix(stdout, "nothing changed\n") {
		t.Fatalf("ssh-include again: exit %d, stdout %q, config changed %v", code, stdout, !bytes.Equal(before, after))
	}
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatalf("backup changed: %q", b)
	}

	up := startUp(t, bin, env, "agent-a", "wss://agent.example.com")
	up.stop(t, syscall.SIGTERM)
	if up.stderr.String() != "" {
		t.Fatalf("up printed %q with the include in place", up.stderr.String())
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

// TestUpCodexProxyByName: while `up` runs, the Codex app's ssh reaches the
// alias through the included config and `codex proxy --name` answers the
// version probe and relays the app-server session to <server>/codex with the
// session's token; after `up` stops, ssh fails with a line naming the session.
func TestUpCodexProxyByName(t *testing.T) {
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

	cmd = sshByName(env, config, "agent-a", readFixture(t, "codex-app-proxy.txt"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, len(fixtureMarker))
	if _, err := io.ReadFull(stdout, marker); err != nil || !bytes.Equal(marker, fixtureMarker) {
		t.Fatalf("marker %q (%v), want %q\nstderr: %s", marker, err, fixtureMarker, stderr.String())
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
		t.Fatalf("round trip: typ=%v data=%q err=%v\nstderr: %s", typ, data, err, stderr.String())
	}
	if err := ws.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ssh exited with %v, want 0\nstderr: %s", err, stderr.String())
	}
	if paths, auths := server.seen(); len(paths) != 1 || paths[0] != "/prefix/codex" || auths[0] != "Bearer "+testToken {
		t.Fatalf("server saw paths %v with Authorization %q", paths, auths)
	}
	if strings.Contains(stderr.String(), testToken) {
		t.Fatal("ssh stderr contains the token")
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
	cmd.Args = append(cmd.Args[:1], append([]string{"-o", "ProxyCommand=" + quote(bin) + " codex proxy --name agent-a"}, cmd.Args[1:]...)...)
	stderr.Reset()
	cmd.Stderr = &stderr
	out, err = cmd.Output()
	if err == nil || len(out) != 0 {
		t.Fatalf("ssh after stop: %v, stdout %q", err, out)
	}
	if want := "comlink: error: session \"agent-a\" is not up: run 'comlink up --name agent-a --server URL' first\n"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q lacks %q", stderr.String(), want)
	}
}

// acpRoundTrip runs the binary as an ACP agent command and checks that one
// message is echoed back and the session ends cleanly.
func acpRoundTrip(t *testing.T, bin string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdout := bufio.NewReader(stdoutPipe)
	msg := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`
	if _, err := io.WriteString(stdin, msg+"\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := stdout.ReadString('\n'); err != nil || line != msg+"\n" {
		t.Fatalf("stdout line %q (%v), want the echo; stderr %q", line, err, stderr.String())
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(stdout); err != nil || len(rest) != 0 {
		t.Fatalf("stdout carries %q (%v) after the protocol line", rest, err)
	}
	if err := cmd.Wait(); err != nil || stderr.Len() != 0 {
		t.Fatalf("comlink %v: %v, want exit 0 and no stderr; stderr %q", args, err, stderr.String())
	}
}

// TestUpACPByName: `acp --name` relays to <server>/acp with the session's
// token while `up` runs, and fails fast naming the session after it stops.
func TestUpACPByName(t *testing.T) {
	bin := buildComlink(t)
	env, _, _ := sessionEnv(t)
	server := newServerUpstream(t)
	up := startUp(t, bin, env, "agent-a", wsURLOf(server.Server))

	acpRoundTrip(t, bin, env, "acp", "--name", "agent-a")
	if paths, auths := server.seen(); len(paths) != 1 || paths[0] != "/acp" || auths[0] != "Bearer "+testToken {
		t.Fatalf("server saw paths %v with Authorization %q", paths, auths)
	}

	up.stop(t, syscall.SIGTERM)
	stdout, stderr, code := runBinary(t, bin, env, `{"jsonrpc":"2.0","id":0,"method":"initialize"}`+"\n", "acp", "--name", "agent-a")
	if want := "comlink: error: session \"agent-a\" is not up: run 'comlink up --name agent-a --server URL' first\n"; code == 0 || stdout != "" || stderr != want {
		t.Fatalf("acp after stop: exit %d, stdout %q, stderr %q, want %q", code, stdout, stderr, want)
	}
	if paths, _ := server.seen(); len(paths) != 1 {
		t.Fatalf("the server was dialled after stop: %v", paths)
	}
}

// TestUpSeveralNames: names up at once each route to their own server, and
// a base with a path prefix or a trailing slash maps to its /codex and /acp.
func TestUpSeveralNames(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	servers := map[string]*serverUpstream{"agent-a": newServerUpstream(t), "agent-b": newServerUpstream(t), "agent-c": newServerUpstream(t)}
	bases := map[string]string{
		"agent-a": wsURLOf(servers["agent-a"].Server),
		"agent-b": wsURLOf(servers["agent-b"].Server) + "/",
		"agent-c": wsURLOf(servers["agent-c"].Server) + "/tenant/x",
	}
	wantPath := map[string]string{"agent-a": "/acp", "agent-b": "/acp", "agent-c": "/tenant/x/acp"}
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
		if got.Server != bases[name] || got.Codex != wantCodex[name] || got.ACP != strings.TrimSuffix(wantCodex[name], "codex")+"acp" {
			t.Errorf("%s record %+v, want codex %q", name, got, wantCodex[name])
		}
		acpRoundTrip(t, bin, env, "acp", "--name", name)
		paths, auths := servers[name].seen()
		if len(paths) != 1 || paths[0] != wantPath[name] || auths[0] != "Bearer "+testToken {
			t.Errorf("%s: server saw paths %v with Authorization %q, want %q", name, paths, auths, wantPath[name])
		}
	}
	stdout, _, code := runBinary(t, bin, env, "", "list")
	if code != 0 || strings.Count(stdout, "\tup\t") != 3 {
		t.Fatalf("list: exit %d, stdout %q", code, stdout)
	}
}

// TestUpRejectsBadArguments: `up` refuses an invalid name or server before
// writing anything; `codex proxy` and `acp` take exactly one of --name and
// --upstream, and --token-file only with --upstream.
func TestUpRejectsBadArguments(t *testing.T) {
	bin := buildComlink(t)
	env, _, state := sessionEnv(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"up", "--name", "agent\nHost *", "--server", "wss://agent.example.com"}, `comlink: error: invalid alias "agent\nHost *": use only letters, digits, '-', '_' and '.'`},
		{[]string{"up", "--name", "agent", "--server", "https://agent.example.com"}, `comlink: error: server URL "https://agent.example.com": want ws://host[/prefix] or wss://host[/prefix]`},
		{[]string{"up", "--name", "agent", "--server", "wss://agent.example.com", "--token-file", filepath.Join(state, "absent")}, "comlink: error: read token file: "},
		{[]string{"codex", "proxy"}, "comlink: error: give --name NAME (a session from 'comlink up') or --upstream URL"},
		{[]string{"codex", "proxy", "--name", "a", "--upstream", "ws://127.0.0.1:1"}, "comlink: error: --name and --upstream can't be used together"},
		{[]string{"codex", "proxy", "--name", "a", "--token-file", "/dev/null"}, "comlink: error: --token-file goes with --upstream; a session's token comes from 'comlink up'"},
		{[]string{"codex", "proxy", "--name", "two words"}, `comlink: error: invalid name "two words": use only letters, digits, '-', '_' and '.'`},
		{[]string{"acp"}, "comlink: error: give --name NAME (a session from 'comlink up') or --upstream URL"},
		{[]string{"acp", "--name", "a", "--upstream", "ws://127.0.0.1:1"}, "comlink: error: --name and --upstream can't be used together"},
		{[]string{"acp", "--name", "a", "--token-file", "/dev/null"}, "comlink: error: --token-file goes with --upstream; a session's token comes from 'comlink up'"},
		{[]string{"acp", "--name", "../a"}, `comlink: error: invalid name "../a": use only letters, digits, '-', '_' and '.'`},
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
}
