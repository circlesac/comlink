package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/circlesac/comlink/internal/session"
	"github.com/circlesac/comlink/internal/upstream"
)

// TestServerEndpoint: a comlink server serves the Codex app-server
// protocol at <server>/codex and ACP at <server>/acp, whatever the base's
// path prefix or trailing slash.
func TestServerEndpoint(t *testing.T) {
	for _, tc := range []struct{ server, codex, acp string }{
		{"wss://agent.example.com", "wss://agent.example.com/codex", "wss://agent.example.com/acp"},
		{"wss://agent.example.com/", "wss://agent.example.com/codex", "wss://agent.example.com/acp"},
		{"ws://127.0.0.1:8787/prefix", "ws://127.0.0.1:8787/prefix/codex", "ws://127.0.0.1:8787/prefix/acp"},
		{"ws://127.0.0.1:8787/prefix/", "ws://127.0.0.1:8787/prefix/codex", "ws://127.0.0.1:8787/prefix/acp"},
		{"wss://agent.example.com/a/b//", "wss://agent.example.com/a/b/codex", "wss://agent.example.com/a/b/acp"},
		{"wss://agent.example.com/base?x=1", "wss://agent.example.com/base/codex?x=1", "wss://agent.example.com/base/acp?x=1"},
	} {
		codex, err := upstream.Endpoint(tc.server, "codex")
		if err != nil || codex != tc.codex {
			t.Errorf("Endpoint(%q, codex) = %q, %v; want %q", tc.server, codex, err, tc.codex)
		}
		acp, err := upstream.Endpoint(tc.server, "acp")
		if err != nil || acp != tc.acp {
			t.Errorf("Endpoint(%q, acp) = %q, %v; want %q", tc.server, acp, err, tc.acp)
		}
	}
	for _, server := range []string{"", "https://agent.example.com", "agent.example.com/codex", "ws://", "ws://[::1", "wss:///path"} {
		if endpoint, err := upstream.Endpoint(server, "codex"); err == nil {
			t.Errorf("Endpoint(%q) = %q, want an error", server, endpoint)
		}
	}
	// An error never repeats the userinfo or query of the URL.
	for server, want := range map[string]string{
		"https://user:secret@agent.example.com/x?token=secret#secret": `server URL "https://agent.example.com/x": want ws://host[/prefix] or wss://host[/prefix]`,
		"ws://user:secret@[::1/?token=secret":                         "server URL: missing ']' in host",
	} {
		if _, err := upstream.Endpoint(server, "codex"); err == nil || err.Error() != want {
			t.Errorf("Endpoint(%q) = %v, want %q", server, err, want)
		}
	}
}

// TestUpstreamRedact: a URL is printed without its userinfo, query and
// fragment, and one that does not parse not at all.
func TestUpstreamRedact(t *testing.T) {
	for raw, want := range map[string]string{
		"wss://agent.example.com/base/":                       "wss://agent.example.com/base/",
		"wss://user:secret@agent.example.com/base/?token=x#y": "wss://agent.example.com/base/",
		"ws://secret@127.0.0.1:8787?":                         "ws://127.0.0.1:8787",
		"ws://user:secret@[::1/":                              "<invalid upstream URL>",
	} {
		if got := upstream.Redact(raw); got != want {
			t.Errorf("Redact(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSessionValidName(t *testing.T) {
	for _, name := range []string{"example-agent", "a", "Agent_2", "agent.example.com", "0-9_.-"} {
		if !session.ValidName(name) {
			t.Errorf("ValidName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "two words", "agent\nHost *", "*", "agent?", "!agent", "agent/x", "../agent", "ägent", "-agent", "-"} {
		if session.ValidName(name) {
			t.Errorf("ValidName(%q) = true", name)
		}
	}
}

// readRecord parses a record file.
func readRecord(t *testing.T, path string) session.Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), testToken) {
		t.Fatalf("record %s contains the token: %s", path, b)
	}
	var r session.Record
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("record %s: %v\n%s", path, err, b)
	}
	return r
}

// mode returns the permission bits of path.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// absent fails when any of paths exists.
func absent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists (%v), want it gone", p, err)
		}
	}
}

// sessionFiles are the paths a session's files have under a state directory.
func sessionFiles(dir, name string) (record, token, lock, conf string) {
	run := filepath.Join(dir, "comlink", "run")
	return filepath.Join(run, name+".json"), filepath.Join(run, name+".token"), filepath.Join(run, name+".lock"),
		filepath.Join(dir, "comlink", "ssh", name+".conf")
}

// deadPID is the pid of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// writeStaleSession writes the files a crashed session leaves behind for name:
// a record with a dead pid, a token file, an ssh config file and a lock file
// nobody holds.
func writeStaleSession(t *testing.T, dir, name string) {
	t.Helper()
	record, token, lock, conf := sessionFiles(dir, name)
	for _, d := range []string{filepath.Dir(record), filepath.Dir(conf)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(session.Record{Name: name, Server: "ws://stale.example.com", Codex: "ws://stale.example.com/codex", PID: deadPID(t)})
	for path, content := range map[string]string{record: string(b), token: "stale-token", lock: "", conf: "Host stale\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSessionStore drives the registry in process: Start creates the
// record, the token file and the ssh config file with private modes and no
// token outside the token file; Load finds the session while it is up and
// a second Start on the name is refused; Close removes everything and Load
// then reports the name as not up.
func TestSessionStore(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	record, token, lock, conf := sessionFiles(dir, "agent-a")

	sess, err := store.Start("agent-a", "wss://agent.example.com/base/", testToken, "Host agent-a\n  ProxyCommand x\n")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Replaced != nil {
		t.Fatalf("Replaced = %+v on a fresh name", sess.Replaced)
	}
	want := session.Record{Name: "agent-a", Server: "wss://agent.example.com/base/", Codex: "wss://agent.example.com/base/codex", PID: os.Getpid()}
	if sess.Record != want {
		t.Fatalf("session record %+v, want %+v", sess.Record, want)
	}
	if got := readRecord(t, record); got != want {
		t.Fatalf("record on disk %+v, want %+v", got, want)
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
	if b, _ := os.ReadFile(conf); string(b) != "Host agent-a\n  ProxyCommand x\n" {
		t.Fatalf("ssh config %q", b)
	}
	// Each file was renamed into place; no temporary file is left beside it.
	if run, ssh := sessionDirs(t, dir); strings.Join(run, " ") != "agent-a.json agent-a.lock agent-a.token" || strings.Join(ssh, " ") != "agent-a.conf" {
		t.Fatalf("run holds %v and ssh holds %v, want only the session's files", run, ssh)
	}

	got, gotToken, err := store.Load("agent-a")
	if err != nil || got != want || gotToken != testToken {
		t.Fatalf("Load = %+v, token ok %v, %v", got, gotToken == testToken, err)
	}
	if _, err := store.Start("agent-a", "wss://other.example.com", "", ""); err == nil || err.Error() != `session "agent-a" is already up (pid `+strconv.Itoa(os.Getpid())+`)` {
		t.Fatalf("second Start: %v", err)
	}
	if got := readRecord(t, record); got != want {
		t.Fatalf("record changed by the refused Start: %+v", got)
	}
	live, stale, err := store.List()
	if err != nil || len(stale) != 0 || len(live) != 1 || live[0] != want {
		t.Fatalf("List = %+v, %+v, %v", live, stale, err)
	}

	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	absent(t, record, token, lock, conf)
	var notUp *session.NotUpError
	if _, _, err := store.Load("agent-a"); !errors.As(err, &notUp) || notUp.Name != "agent-a" {
		t.Fatalf("Load after Close: %v", err)
	}
	if want := `session "agent-a" is not up: run 'comlink codex --name agent-a --server URL' first`; notUp.Error() != want {
		t.Fatalf("error %q, want %q", notUp, want)
	}
	if live, stale, err := store.List(); err != nil || len(live)+len(stale) != 0 {
		t.Fatalf("List after Close = %+v, %+v, %v", live, stale, err)
	}
	if _, _, err := store.Load("never"); !errors.As(err, &notUp) {
		t.Fatalf("Load of an unknown name: %v", err)
	}

	// A second session with the same name can start once the first is closed.
	again, err := store.Start("agent-a", "ws://127.0.0.1:1", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, gotToken, err := store.Load("agent-a"); err != nil || gotToken != "" {
		t.Fatalf("Load without a token = %q, %v", gotToken, err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSessionStoreUpWithoutRecord: a name whose lock is held is up even when
// its record is missing or unreadable (a deleted record, or the moment
// inside Close between removing the record and releasing the lock). A
// second Start is refused without a pid, since there is none to name, and
// leaves the live session's files alone.
func TestSessionStoreUpWithoutRecord(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	record, token, lock, conf := sessionFiles(dir, "agent-a")

	sess, err := store.Start("agent-a", "wss://agent.example.com", testToken, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	for _, tc := range []struct {
		what   string
		record func() error
	}{
		{"missing", func() error { return os.Remove(record) }},
		{"unreadable", func() error { return os.WriteFile(record, []byte("{not json"), 0o600) }},
	} {
		if err := tc.record(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Start("agent-a", "wss://other.example.com", "", "y"); err == nil || err.Error() != `session "agent-a" is already up` {
			t.Fatalf("Start with the record %s: %v", tc.what, err)
		}
		for _, path := range []string{token, lock, conf} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("record %s: the refused Start removed %s: %v", tc.what, path, err)
			}
		}
	}

	// Close removes the files before it releases the lock: a session that
	// is held but has lost its token file is not up for Load either.
	b, _ := json.Marshal(session.Record{Name: "agent-a", Server: "wss://agent.example.com", Codex: "wss://agent.example.com/codex", PID: os.Getpid()})
	if err := os.WriteFile(record, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	var notUp *session.NotUpError
	if _, _, err := store.Load("agent-a"); !errors.As(err, &notUp) || notUp.Name != "agent-a" {
		t.Fatalf("Load of a held name without its token file: %v", err)
	}
	if _, _, err := store.Resolve("agent-a"); !errors.As(err, &notUp) {
		t.Fatalf("Resolve of a held name without its token file: %v", err)
	}
}

// TestSessionStoreRefusesBadInput: an invalid name or server URL is refused
// before anything is written.
func TestSessionStoreRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	for _, tc := range []struct{ name, server, want string }{
		{"two words", "wss://agent.example.com", `invalid name "two words": use only letters, digits, '-', '_' and '.', not starting with '-'`},
		{"../up", "wss://agent.example.com", `invalid name "../up": use only letters, digits, '-', '_' and '.', not starting with '-'`},
		{"-agent", "wss://agent.example.com", `invalid name "-agent": use only letters, digits, '-', '_' and '.', not starting with '-'`},
		{"agent", "https://agent.example.com", `server URL "https://agent.example.com": want ws://host[/prefix] or wss://host[/prefix]`},
		{"agent", "ws://[::1", "server URL: missing ']' in host"},
	} {
		sess, err := store.Start(tc.name, tc.server, testToken, "x")
		if err == nil || err.Error() != tc.want {
			t.Errorf("Start(%q, %q) = %v, %v; want %q", tc.name, tc.server, sess, err, tc.want)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("state directory holds %v (%v) after refused starts", entries, err)
	}
	if _, _, err := store.Load("two words"); err == nil || !strings.HasPrefix(err.Error(), "invalid name") {
		t.Fatalf("Load of an invalid name: %v", err)
	}

	// A file that cannot be written fails Start, which then removes what it
	// wrote and releases the name.
	if os.Getuid() != 0 {
		ssh := store.SSHDir()
		if err := os.MkdirAll(ssh, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ssh, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ssh, 0o700) })
		if sess, err := store.Start("agent", "wss://agent.example.com", testToken, "x"); err == nil || !strings.HasPrefix(err.Error(), "write ssh config: ") {
			t.Fatalf("Start with a read-only ssh directory = %v, %v", sess, err)
		}
		record, token, lock, conf := sessionFiles(dir, "agent")
		absent(t, record, token, lock, conf)
		if entries, err := os.ReadDir(ssh); err != nil || len(entries) != 0 {
			t.Fatalf("ssh directory holds %v (%v) after the failed write", entries, err)
		}
		if err := os.Chmod(ssh, 0o700); err != nil {
			t.Fatal(err)
		}
		sess, err := store.Start("agent", "wss://agent.example.com", testToken, "x")
		if err != nil {
			t.Fatalf("Start after the failure: %v", err)
		}
		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSessionStaleRecords: a record whose session process is gone (its lock is
// not held) is stale. List removes it and reports it; Start replaces it and
// reports it as Replaced; Load reports the name as not up and leaves it.
func TestSessionStaleRecords(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	writeStaleSession(t, dir, "gone")
	writeStaleSession(t, dir, "replaced")
	record, token, lock, conf := sessionFiles(dir, "gone")
	run := filepath.Dir(record)
	// Not sessions: a record being renamed into place, a broken one and a
	// stray file. They are left alone.
	for name, content := range map[string]string{"half.json.tmp": "{", "broken.json": "not json", "notes.txt": "x"} {
		if err := os.WriteFile(filepath.Join(run, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var notUp *session.NotUpError
	if _, _, err := store.Load("gone"); !errors.As(err, &notUp) {
		t.Fatalf("Load of a stale name: %v", err)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("Load removed the stale record: %v", err)
	}

	live, stale, err := store.List()
	if err != nil || len(live) != 0 || len(stale) != 2 || stale[0].Name != "gone" || stale[1].Name != "replaced" {
		t.Fatalf("List = %+v, %+v, %v", live, stale, err)
	}
	absent(t, record, token, conf)
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("List removed the lock file, which only its holder removes: %v", err)
	}
	for _, name := range []string{"half.json.tmp", "broken.json", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(run, name)); err != nil {
			t.Errorf("List removed %s: %v", name, err)
		}
	}

	writeStaleSession(t, dir, "replaced")
	stalePID := readRecord(t, filepath.Join(run, "replaced.json")).PID
	sess, err := store.Start("replaced", "wss://new.example.com", testToken, "Host replaced\n")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if sess.Replaced == nil || sess.Replaced.PID != stalePID || sess.Replaced.Server != "ws://stale.example.com" {
		t.Fatalf("Replaced = %+v, want the stale record with pid %d", sess.Replaced, stalePID)
	}
	rec, tok, _, cnf := sessionFiles(dir, "replaced")
	if got := readRecord(t, rec); got.PID != os.Getpid() || got.Server != "wss://new.example.com" {
		t.Fatalf("record after replacement %+v", got)
	}
	if b, _ := os.ReadFile(tok); string(b) != testToken {
		t.Fatalf("token file after replacement %q", b)
	}
	if b, _ := os.ReadFile(cnf); string(b) != "Host replaced\n" {
		t.Fatalf("ssh config after replacement %q", b)
	}

	// Without a run directory there is nothing to list.
	empty := &session.Store{Dir: filepath.Join(t.TempDir(), "comlink")}
	if live, stale, err := empty.List(); err != nil || live != nil || stale != nil {
		t.Fatalf("List on an empty store = %+v, %+v, %v", live, stale, err)
	}
}

// TestSessionOpen: the store is $XDG_STATE_HOME/comlink, else
// ~/.local/state/comlink, and an error without either.
func TestSessionOpen(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state-x")
	store, err := session.Open()
	if err != nil || store.Dir != "/tmp/state-x/comlink" || store.SSHDir() != "/tmp/state-x/comlink/ssh" {
		t.Fatalf("Open = %+v, %v", store, err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/home-x")
	store, err = session.Open()
	if err != nil || store.Dir != "/tmp/home-x/.local/state/comlink" {
		t.Fatalf("Open = %+v, %v", store, err)
	}
	if path, err := session.SSHConfigPath(); err != nil || path != "/tmp/home-x/.ssh/config" {
		t.Fatalf("SSHConfigPath = %q, %v", path, err)
	}
	t.Setenv("HOME", "")
	if store, err := session.Open(); err == nil || !strings.HasPrefix(err.Error(), "locate state directory: XDG_STATE_HOME is unset and the home directory is unknown: ") {
		t.Fatalf("Open without a home = %+v, %v", store, err)
	}
	if path, err := session.SSHConfigPath(); err == nil || !strings.HasPrefix(err.Error(), "locate ~/.ssh/config: ") {
		t.Fatalf("SSHConfigPath without a home = %q, %v", path, err)
	}
}

// upOnce runs Up for agent-a with a context that is already done, so it
// registers, prints and removes its files at once, and returns its stderr.
func upOnce(store *session.Store, sshConfig string) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr bytes.Buffer
	err := store.Up(ctx, "agent-a", "wss://agent.example.com", testToken, "/opt/comlink", sshConfig, io.Discard, &stderr)
	return stderr.String(), err
}

// TestSessionUpInclude: Up puts the Include line at the top of
// ~/.ssh/config, creating the file private when missing, backing the
// previous content up once and otherwise preserving it byte for byte, and
// says so in one stderr line; with the line already there at top level
// (before any Host or Match block) in any spelling ssh accepts it writes and
// prints nothing. A config it cannot change stops
// it before any session file is written.
func TestSessionUpInclude(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	pattern := filepath.Join(dir, "comlink", "ssh", "*.conf")
	line := "Include " + pattern
	config := filepath.Join(dir, "home", ".ssh", "config")
	backup := config + ".comlink-bak"
	up := func(t *testing.T) string {
		t.Helper()
		stderr, err := upOnce(store, config)
		if err != nil {
			t.Fatalf("Up: %v", err)
		}
		return stderr
	}

	if got, want := up(t), "comlink: created "+config+" with \""+line+"\"\n"; got != want {
		t.Fatalf("Up without a config printed %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\n" {
		t.Fatalf("created config %q", b)
	}
	if m := mode(t, config); m != 0o600 {
		t.Fatalf("created config mode %o, want 600", m)
	}
	if m := mode(t, filepath.Dir(config)); m != 0o700 {
		t.Fatalf("created .ssh mode %o, want 700", m)
	}
	absent(t, backup)
	if got := up(t); got != "" {
		t.Fatalf("Up again printed %q", got)
	}

	// An existing file: the line is prepended, the rest is byte-identical,
	// the mode is kept and the backup holds the previous content.
	original := "# keep\r\n\nHost *\n\tServerAliveInterval 30 \n\n  Host old\n    HostName old.example.com"
	if err := os.WriteFile(config, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := up(t), "comlink: added \""+line+"\" to the top of "+config+"; previous content backed up to "+backup+"\n"; got != want {
		t.Fatalf("Up on an existing config printed %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\n"+original {
		t.Fatalf("config after Up %q", b)
	}
	if m := mode(t, config); m != 0o644 {
		t.Fatalf("config mode %o, want the original 644", m)
	}
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatalf("backup %q, want the previous content", b)
	}
	if got := up(t); got != "" {
		t.Fatalf("Up again printed %q", got)
	}
	// The user removed the line: it is added back, and the first backup is kept.
	if err := os.WriteFile(config, []byte("Host x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := up(t), "comlink: added \""+line+"\" to the top of "+config+"\n"; got != want {
		t.Fatalf("Up after removal printed %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(config); string(b) != line+"\nHost x\n" {
		t.Fatalf("config after re-adding %q", b)
	}
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatalf("backup overwritten: %q", b)
	}

	// Spellings ssh accepts for the same line count as present at top level;
	// anything else, including the line under a Host or Match block, gets
	// the line added on top.
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
		if got := up(t); got != "" {
			t.Errorf("Up with %q printed %q", content, got)
		}
		if b, _ := os.ReadFile(config); string(b) != content {
			t.Errorf("Up changed %q to %q", content, b)
		}
	}
	for _, content := range []string{
		"# Include " + pattern + "\n",
		"Host a\nInclude " + pattern + "\n",
		"match all\n  Include " + pattern + "\n",
		"Include " + pattern + "x\n",
		"Include " + filepath.Join(dir, "other", "*.conf") + "\n",
		"IncludeX " + pattern + "\n",
		"Include\n",
	} {
		if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, want := up(t), "comlink: added \""+line+"\" to the top of "+config+"\n"; got != want {
			t.Errorf("Up with %q printed %q, want %q", content, got, want)
		}
		if b, _ := os.ReadFile(config); string(b) != line+"\n"+content {
			t.Errorf("Up changed %q to %q", content, b)
		}
	}

	// With the state directory under the home directory, the line spelled
	// with ~ or relative to ~/.ssh, as ssh resolves them, is the same line.
	homeStore := &session.Store{Dir: filepath.Join(dir, "home", ".local", "state", "comlink")}
	for _, content := range []string{
		"Include ~/.local/state/comlink/ssh/*.conf\n",
		"Include \"~/.local/state/comlink/ssh/*.conf\"\nHost a\n",
		"Include ../.local/state/comlink/ssh/*.conf\n",
		"Include extra.conf ../.local/state/comlink/ssh/*.conf\n",
	} {
		if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := upOnce(homeStore, config); err != nil || got != "" {
			t.Errorf("Up with %q: printed %q, %v", content, got, err)
		}
		if b, _ := os.ReadFile(config); string(b) != content {
			t.Errorf("Up changed %q to %q", content, b)
		}
	}
	for _, content := range []string{
		"Include ~/state/comlink/ssh/*.conf\n",
		"Include .local/state/comlink/ssh/*.conf\n",
		"Include ~nobody/.local/state/comlink/ssh/*.conf\n",
		"Host a\nInclude ~/.local/state/comlink/ssh/*.conf\n",
	} {
		if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		homeLine := "Include " + filepath.Join(homeStore.SSHDir(), "*.conf")
		if got, err := upOnce(homeStore, config); err != nil || got != "comlink: added \""+homeLine+"\" to the top of "+config+"\n" {
			t.Errorf("Up with %q: printed %q, %v", content, got, err)
		}
		if b, _ := os.ReadFile(config); string(b) != homeLine+"\n"+content {
			t.Errorf("Up changed %q to %q", content, b)
		}
	}

	// A state directory whose path holds whitespace gets a quoted pattern,
	// which the next session recognises.
	spaced := &session.Store{Dir: filepath.Join(dir, "with space", "comlink")}
	spacedConfig := filepath.Join(dir, "spaced", "config")
	spacedLine := `Include "` + filepath.Join(dir, "with space", "comlink", "ssh", "*.conf") + `"`
	if got, err := upOnce(spaced, spacedConfig); err != nil || got != "comlink: created "+spacedConfig+" with "+strconv.Quote(spacedLine)+"\n" {
		t.Fatalf("Up with whitespace in the state directory: %q, %v", got, err)
	}
	if b, _ := os.ReadFile(spacedConfig); string(b) != spacedLine+"\n" {
		t.Fatalf("config %q, want %q", b, spacedLine)
	}
	if got, err := upOnce(spaced, spacedConfig); err != nil || got != "" {
		t.Fatalf("Up again with whitespace in the state directory: %q, %v", got, err)
	}

	// A config that cannot be read or changed is an error and no session
	// file is written.
	refused := filepath.Join(dir, "refused")
	refusedStore := &session.Store{Dir: filepath.Join(refused, "comlink")}
	unreadable := filepath.Join(refused, "unreadable")
	if err := os.MkdirAll(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := upOnce(refusedStore, unreadable); err == nil {
		t.Fatal("Up with a directory as the config: nil error")
	}
	absent(t, refusedStore.Dir)
	if os.Getuid() != 0 {
		blockedDir := filepath.Join(refused, "blocked")
		blocked := filepath.Join(blockedDir, "config")
		if err := os.Mkdir(blockedDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blocked, []byte("Host y\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(blockedDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blockedDir, 0o700) })
		if _, err := upOnce(refusedStore, blocked); err == nil || !strings.HasPrefix(err.Error(), "write backup ") {
			t.Fatalf("Up with a config in a read-only directory: %v", err)
		}
		if b, _ := os.ReadFile(blocked); string(b) != "Host y\n" {
			t.Fatalf("config changed although the backup failed: %q", b)
		}
		absent(t, refusedStore.Dir)
	}
}

// TestSessionUpIncludeSymlink: when ~/.ssh/config is a symbolic link (or a
// chain of them, absolute or relative), Up gives the file it finally names
// the Include line and its backup, the links stay links to the same place,
// and the rest of the file is byte-identical with its mode kept. A link to a
// file that does not exist yet creates that file; a link loop is an error
// before any session file is written.
func TestSessionUpIncludeSymlink(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	line := "Include " + filepath.Join(dir, "comlink", "ssh", "*.conf")
	up := func(t *testing.T, path string) string {
		t.Helper()
		stderr, err := upOnce(store, path)
		if err != nil {
			t.Fatalf("Up(%s): %v", path, err)
		}
		return stderr
	}
	symlink := func(t *testing.T, dest, link string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dest, link); err != nil {
			t.Fatal(err)
		}
	}
	// stillLink fails unless link is a symbolic link whose content is dest.
	stillLink := func(t *testing.T, link, dest string) {
		t.Helper()
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a symlink: %v, %v", link, info, err)
		}
		if got, err := os.Readlink(link); err != nil || got != dest {
			t.Fatalf("%s points to %q (%v), want %q", link, got, err, dest)
		}
	}
	original := "# dotfiles\r\nHost *\n\tServerAliveInterval 30 \n\nHost old\n  HostName old.example.com"

	t.Run("absolute", func(t *testing.T) {
		target := filepath.Join(dir, "abs", "dotfiles", "ssh_config")
		config := filepath.Join(dir, "abs", "home", ".ssh", "config")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(original), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o640); err != nil {
			t.Fatal(err)
		}
		symlink(t, target, config)
		if got, want := up(t, config), "comlink: added \""+line+"\" to the top of "+config+" -> "+target+"; previous content backed up to "+target+".comlink-bak\n"; got != want {
			t.Fatalf("Up printed %q, want %q", got, want)
		}
		stillLink(t, config, target)
		if b, _ := os.ReadFile(target); string(b) != line+"\n"+original {
			t.Fatalf("target %q", b)
		}
		if m := mode(t, target); m != 0o640 {
			t.Fatalf("target mode %o, want the original 640", m)
		}
		if b, _ := os.ReadFile(target + ".comlink-bak"); string(b) != original {
			t.Fatalf("backup %q", b)
		}
		absent(t, config+".comlink-bak")
		if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 2 {
			t.Fatalf("%s holds %v, want the target and its backup only", filepath.Dir(target), entries)
		}
		if got := up(t, config); got != "" {
			t.Fatalf("Up again printed %q", got)
		}
		stillLink(t, config, target)
	})

	t.Run("relative chain", func(t *testing.T) {
		base := filepath.Join(dir, "rel")
		realConfig := filepath.Join(base, "dotfiles", "config")
		if err := os.MkdirAll(filepath.Dir(realConfig), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(realConfig, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(base, "ssh", "config")
		hop := filepath.Join(base, "ssh", "hop")
		symlink(t, "../dotfiles/config", hop)
		symlink(t, "hop", config)
		target := filepath.Join(mustEvalSymlinks(t, base), "dotfiles", "config")
		if got, want := up(t, config), "comlink: added \""+line+"\" to the top of "+config+" -> "+target+"; previous content backed up to "+target+".comlink-bak\n"; got != want {
			t.Fatalf("Up printed %q, want %q", got, want)
		}
		stillLink(t, config, "hop")
		stillLink(t, hop, "../dotfiles/config")
		if b, _ := os.ReadFile(realConfig); string(b) != line+"\n"+original {
			t.Fatalf("target %q", b)
		}
	})

	t.Run("dangling", func(t *testing.T) {
		target := filepath.Join(dir, "dangling", "dotfiles", "new", "config")
		config := filepath.Join(dir, "dangling", "home", ".ssh", "config")
		symlink(t, target, config)
		if got, want := up(t, config), "comlink: created "+config+" -> "+target+" with \""+line+"\"\n"; got != want {
			t.Fatalf("Up printed %q, want %q", got, want)
		}
		stillLink(t, config, target)
		if b, _ := os.ReadFile(target); string(b) != line+"\n" {
			t.Fatalf("target %q", b)
		}
		if m := mode(t, target); m != 0o600 {
			t.Fatalf("created target mode %o, want 600", m)
		}
		if m := mode(t, filepath.Dir(target)); m != 0o700 {
			t.Fatalf("created target directory mode %o, want 700", m)
		}
	})

	t.Run("loop", func(t *testing.T) {
		loopStore := &session.Store{Dir: filepath.Join(dir, "loop", "comlink")}
		config := filepath.Join(dir, "loop", "config")
		symlink(t, "config", config)
		if _, err := upOnce(loopStore, config); err == nil || !strings.HasSuffix(err.Error(), ": too many levels of symbolic links") {
			t.Fatalf("Up on a link loop: %v", err)
		}
		stillLink(t, config, "config")
		absent(t, loopStore.Dir)
	})
}

// TestSessionUp drives Up in process: it adds the Include line, prints what
// is available, holds the name until the context is done, then removes its
// files and leaves the line. With the line in place it prints nothing about
// it, and a stale record is reported as replaced.
func TestSessionUp(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	sshConfig := filepath.Join(dir, "home", ".ssh", "config")
	record, token, lock, conf := sessionFiles(dir, "agent-a")

	start := func(t *testing.T, name string) (ready string, stderr *bytes.Buffer, stop func() error) {
		t.Helper()
		outR, outW := io.Pipe()
		var errBuf lockedBuffer
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			err := store.Up(ctx, name, "wss://agent.example.com/base", testToken, "/opt/comlink", sshConfig, outW, &errBuf)
			_ = outW.Close()
			done <- err
		}()
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			ready += sc.Text() + "\n"
			if sc.Text() == "Press Ctrl-C to stop." {
				break
			}
		}
		if ready == "" || !strings.HasSuffix(ready, "Press Ctrl-C to stop.\n") {
			cancel()
			t.Fatalf("Up ended before it was ready: %v\nstdout %q", <-done, ready)
		}
		var errOut bytes.Buffer
		errOut.WriteString(errBuf.String())
		return ready, &errOut, func() error {
			cancel()
			rest, _ := io.ReadAll(outR)
			if len(rest) != 0 {
				t.Errorf("Up printed %q after the ready line", rest)
			}
			return <-done
		}
	}

	ready, stderr, stop := start(t, "agent-a")
	want := "agent-a is up (pid " + strconv.Itoa(os.Getpid()) + ")\n" +
		"  server:     wss://agent.example.com/base\n" +
		"  Codex app:  SSH connection \"agent-a\" (ssh agent-a)\n" +
		"Press Ctrl-C to stop.\n"
	if ready != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", ready, want)
	}
	line := "Include " + filepath.Join(dir, "comlink", "ssh", "*.conf")
	if want := "comlink: created " + sshConfig + " with \"" + line + "\"\n"; stderr.String() != want {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
	}
	block, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if wantBlock := configBlock(t, "agent-a", "/opt/comlink", "codex", "--stdio", "--name", "agent-a"); string(block) != wantBlock {
		t.Fatalf("ssh config:\n%s\nwant:\n%s", block, wantBlock)
	}
	if got := readRecord(t, record); got.Codex != "wss://agent.example.com/base/codex" || got.PID != os.Getpid() {
		t.Fatalf("record %+v", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("Up returned %v after the context ended", err)
	}
	absent(t, record, token, lock, conf)
	if b, _ := os.ReadFile(sshConfig); string(b) != line+"\n" {
		t.Fatalf("ssh config after the session %q, want the Include line kept", b)
	}

	writeStaleSession(t, dir, "agent-a")
	stalePID := readRecord(t, record).PID
	_, stderr, stop = start(t, "agent-a")
	if want := "comlink: removed stale session \"agent-a\" (pid " + strconv.Itoa(stalePID) + " is gone)\n"; stderr.String() != want {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	// Refusals: a bad alias or server before anything is written (not even
	// the Include line), and a name that is up.
	untouched := filepath.Join(dir, "untouched", "config")
	if err := store.Up(context.Background(), "agent\nHost *", "wss://agent.example.com", "", "/opt/comlink", untouched, io.Discard, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "invalid alias ") {
		t.Fatalf("Up with a bad alias: %v", err)
	}
	if err := store.Up(context.Background(), "agent-c", "https://agent.example.com", "", "/opt/comlink", untouched, io.Discard, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "server URL ") {
		t.Fatalf("Up with a bad server: %v", err)
	}
	absent(t, untouched)
	held, err := store.Start("agent-b", "wss://agent.example.com", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := store.Up(context.Background(), "agent-b", "wss://agent.example.com", "", "/opt/comlink", sshConfig, io.Discard, io.Discard); err == nil || err.Error() != `session "agent-b" is already up (pid `+strconv.Itoa(os.Getpid())+`)` {
		t.Fatalf("Up on a name that is up: %v", err)
	}
}

// TestSessionUpDefaultName: without a name Up names the session after the
// first label of the server's host, lowercased, says so on stdout and uses
// it for the record and the alias. For an IP address, localhost (or
// *.localhost), a first label "localhost" or a label that is not a valid
// alias the name is comlink and the name line says why. A name that is up
// is skipped for the next suffix, and the name line says so too.
func TestSessionUpDefaultName(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	sshConfig := filepath.Join(dir, "home", ".ssh", "config")
	pid := strconv.Itoa(os.Getpid())
	for _, tc := range []struct{ server, name, line string }{
		{"wss://padawan.example.com/x", "padawan", "padawan, the first label of the server's host (--name sets another)"},
		{"wss://padawan-app.foo.workers.dev", "padawan-app", "padawan-app, the first label of the server's host (--name sets another)"},
		{"ws://Worker:8787", "worker", "worker, the first label of the server's host (--name sets another)"},
		{"wss://PADAWAN.Example.COM:8443/tenant/x/", "padawan", "padawan, the first label of the server's host (--name sets another)"},
		{"wss://padawan./", "padawan", "padawan, the first label of the server's host (--name sets another)"},
		{"ws://127.0.0.1:8787", "comlink", "comlink (the server host 127.0.0.1 is an IP address; --name sets another)"},
		{"wss://[::1]:8443/x", "comlink", "comlink (the server host ::1 is an IP address; --name sets another)"},
		{"ws://[fe80::1%25en0]:8787", "comlink", "comlink (the server host fe80::1%en0 is an IP address; --name sets another)"},
		{"ws://localhost:8787", "comlink", "comlink (the server host localhost is localhost; --name sets another)"},
		{"ws://LocalHost.", "comlink", "comlink (the server host localhost is localhost; --name sets another)"},
		{"ws://x.localhost:8787", "comlink", "comlink (the server host x.localhost is localhost; --name sets another)"},
		{"wss://localhost.example.com", "comlink", `comlink (the first label "localhost" of the server host localhost.example.com would capture your own ssh to localhost; --name sets another)`},
		{"wss://.example.com", "comlink", `comlink (the first label "" of the server host .example.com is not a valid alias; --name sets another)`},
		{"wss://a~b.example.com", "comlink", `comlink (the first label "a~b" of the server host a~b.example.com is not a valid alias; --name sets another)`},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout bytes.Buffer
		if err := store.Up(ctx, "", tc.server, testToken, "/opt/comlink", sshConfig, &stdout, io.Discard); err != nil {
			t.Fatalf("Up(%q) without a name: %v", tc.server, err)
		}
		want := tc.name + " is up (pid " + pid + ")\n" +
			"  name:       " + tc.line + "\n" +
			"  server:     " + tc.server + "\n" +
			"  Codex app:  SSH connection \"" + tc.name + "\" (ssh " + tc.name + ")\n" +
			"Press Ctrl-C to stop.\n"
		if stdout.String() != want {
			t.Errorf("Up(%q) stdout:\n%s\nwant:\n%s", tc.server, stdout.String(), want)
		}
	}

	// An explicit name is used as is and gets no name line.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	if err := store.Up(ctx, "Agent-X", "wss://padawan.example.com", testToken, "/opt/comlink", sshConfig, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "Agent-X is up (pid ") || strings.Contains(stdout.String(), "name:") {
		t.Fatalf("Up with --name stdout:\n%s", stdout.String())
	}

	// Names that are up are skipped, for a label and for comlink alike.
	for _, name := range []string{"padawan", "padawan-2", "comlink"} {
		sess, err := store.Start(name, "wss://padawan.example.com", testToken, "x")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sess.Close() }()
	}
	for _, tc := range []struct{ server, name, line string }{
		{"wss://padawan.example.com", "padawan-3", "padawan-3 (padawan is already up, padawan-2 is already up; --name sets another)"},
		{"ws://127.0.0.1:8787", "comlink-2", "comlink-2 (the server host 127.0.0.1 is an IP address, comlink is already up; --name sets another)"},
	} {
		stdout.Reset()
		if err := store.Up(ctx, "", tc.server, testToken, "/opt/comlink", sshConfig, &stdout, io.Discard); err != nil {
			t.Fatalf("Up(%q) without a name: %v", tc.server, err)
		}
		if want := tc.name + " is up (pid " + pid + ")\n  name:       " + tc.line + "\n"; !strings.HasPrefix(stdout.String(), want) {
			t.Errorf("Up(%q) stdout:\n%s\nwant prefix:\n%s", tc.server, stdout.String(), want)
		}
	}

	// A bad server is still reported as one, and nothing is written.
	untouched := t.TempDir()
	refusedStore := &session.Store{Dir: filepath.Join(untouched, "comlink")}
	if err := refusedStore.Up(context.Background(), "", "https://padawan.example.com", "", "/opt/comlink", filepath.Join(untouched, "config"), io.Discard, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "server URL ") {
		t.Fatalf("Up with a bad server and no name: %v", err)
	}
	if entries, err := os.ReadDir(untouched); err != nil || len(entries) != 0 {
		t.Fatalf("%s holds %v (%v) after a refused session", untouched, entries, err)
	}
}

// TestSessionUpExistingSSHHost: before writing anything Up reads the ssh
// config it edits, and the files that config Includes, for a Host line with
// a pattern that matches the name (and not a random one too, as Host *
// does). A derived name with such a line is skipped for devbox-2 and the
// name line says why; an explicit name gets one warning line naming the Host
// line and comes up. When the next suffix matches too (a pattern such as
// Host dev*) Up fails with one line asking for --name and writes nothing.
// Words are split as ssh's argv_split does, the lowercased name is matched
// against patterns as written, and an Include is followed only where it
// applies to the name (top level, a Host line that matches it, a Match
// block). A name that no specific Host line matches comes up without a
// word: Host * with options ssh would percent-expand for the name, a
// negated name, an upper-case pattern, a Match block (not evaluated), a
// missing, looping or ~user Include or one under another Host line, and
// comlink's own stale session file. %d in a config, file or location is
// the test directory; files are relative to ~/.ssh unless absolute.
func TestSessionUpExistingSSHHost(t *testing.T) {
	const (
		none = iota
		skip
		refuse
	)
	for _, tc := range []struct {
		desc, config string
		files        map[string]string
		want         int
		where        string // the Host line the warning names
	}{
		{"a Host line", "Host other\n  User x\n\nHost devbox\n  User someone\n", nil, skip, "%d/home/.ssh/config:5"},
		{"one of several patterns", "Host other devbox\n", nil, skip, "%d/home/.ssh/config:2"},
		{"an upper-case keyword and =", "HOST=devbox\n", nil, skip, "%d/home/.ssh/config:2"},
		{"an upper-case pattern", "Host DEVBOX\n", nil, none, ""},
		{"a ? wildcard", "Host dev?ox\n", nil, skip, "%d/home/.ssh/config:2"},
		{"a quoted pattern and a comment", "  Host \"other\" \"devbox\" # mine\n", nil, skip, "%d/home/.ssh/config:2"},
		{"a single-quoted pattern", "Host 'devbox'\n", nil, skip, "%d/home/.ssh/config:2"},
		{"quotes and escapes that keep words together", "Host 'other devbox' other\\ devbox dev\\box \"devbox\\\" x\"\n", nil, none, ""},
		{"an absolute Include", "Include %d/extra.conf\n", map[string]string{"%d/extra.conf": "Host devbox\n"}, skip, "%d/extra.conf:1"},
		{"a relative Include", "Include extra.conf\n", map[string]string{"extra.conf": "# extra\nHost devbox\n"}, skip, "%d/home/.ssh/extra.conf:2"},
		{"a glob Include under a Match block", "Match all\n  Include conf.d/*.conf\n", map[string]string{"conf.d/a.conf": "Host x\n", "conf.d/b.conf": "Host devbox\n"}, skip, "%d/home/.ssh/conf.d/b.conf:1"},
		{"a ~ Include", "Include = \"~/.ssh/conf.d/devbox.conf\"\n", map[string]string{"conf.d/devbox.conf": "Host devbox\n"}, skip, "%d/home/.ssh/conf.d/devbox.conf:1"},
		{"an Include loop", "Include config\nHost devbox\n", nil, skip, "%d/home/.ssh/config:3"},
		{"an Include under Host *", "Host *\n  Include extra.conf\n", map[string]string{"extra.conf": "Host devbox\n"}, skip, "%d/home/.ssh/extra.conf:1"},
		{"an Include under the name's Host line", "Host devbox\n  Include extra.conf\n", map[string]string{"extra.conf": "Host devbox*\n"}, skip, "%d/home/.ssh/config:2"},
		{"an Include under another Host line", "Host work-*\n  Include extra.conf\n", map[string]string{"extra.conf": "Host devbox*\n"}, none, ""},
		{"a Host pattern", "Host dev*\n  Port 2222\n", nil, refuse, "%d/home/.ssh/config:2"},
		{"an Included Host pattern", "Include extra.conf\n", map[string]string{"extra.conf": "Host devbox*\n"}, refuse, "%d/home/.ssh/extra.conf:1"},
		{"Host * only", "Host *\n  User someone\n", nil, none, ""},
		{"Host * with percent expansion", "Host *\n  ControlPath ~/.ssh/cm-%r@%h:%p\n  HostName %h.example.com\n  UserKnownHostsFile ~/.ssh/kh_%h\n", nil, none, ""},
		{"Host * except another", "Host * !other\n", nil, none, ""},
		{"a negated name", "Host dev* !devbox\n", nil, skip, ""},
		{"a Match block", "Match originalhost devbox\n  User someone\n", nil, none, ""},
		{"a commented Host line", "# Host devbox\nHost\n", nil, none, ""},
		{"missing and odd Includes", "Include nothing/*.conf %d/absent ~nobody/devbox.conf %d\n", nil, none, ""},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			expand := func(s string) string { return strings.ReplaceAll(s, "%d", dir) }
			store := &session.Store{Dir: filepath.Join(dir, "comlink")}
			config := filepath.Join(dir, "home", ".ssh", "config")
			for name, content := range tc.files {
				path := expand(name)
				if !filepath.IsAbs(path) {
					path = filepath.Join(filepath.Dir(config), path)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The Include line is there already, so Up prints only about the
			// name and the line numbers stay put.
			content := "Include " + filepath.Join(dir, "comlink", "ssh", "*.conf") + "\n" + expand(tc.config)
			if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			// A cancelled context: a session that comes up returns at once.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stdout, stderr bytes.Buffer
			err := store.Up(ctx, "", "wss://devbox.example.com", testToken, "/opt/comlink", config, &stdout, &stderr)
			switch tc.want {
			case refuse:
				if want := `"devbox" and "devbox-2" both have ssh configuration, so a pattern in it likely matches every devbox-N; pass --name NAME`; err == nil || err.Error() != want {
					t.Fatalf("Up without a name = %v, want %q", err, want)
				}
				if stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("refused Up printed %q and %q", stdout.String(), stderr.String())
				}
				absent(t, store.Dir, config+".comlink-bak")
			case skip:
				if err != nil {
					t.Fatalf("Up without a name: %v", err)
				}
				name, line := "devbox-2", "devbox-2 (devbox has ssh configuration; --name sets another)"
				if tc.where == "" { // only devbox-2 matches: devbox is not configuration
					name, line = "devbox", "devbox, the first label of the server's host (--name sets another)"
				}
				if want := name + " is up (pid " + strconv.Itoa(os.Getpid()) + ")\n  name:       " + line + "\n"; !strings.HasPrefix(stdout.String(), want) || stderr.Len() != 0 {
					t.Fatalf("Up without a name printed:\n%s\n%q\nwant prefix:\n%s", stdout.String(), stderr.String(), want)
				}
			case none:
				if err != nil || !strings.HasPrefix(stdout.String(), "devbox is up (pid ") || stderr.Len() != 0 {
					t.Fatalf("Up without a name: %v, printed %q and %q", err, stdout.String(), stderr.String())
				}
			}
			if b, _ := os.ReadFile(config); string(b) != content {
				t.Fatalf("Up changed the config to %q", b)
			}

			stdout.Reset()
			stderr.Reset()
			if err := store.Up(ctx, "devbox", "wss://devbox.example.com", testToken, "/opt/comlink", config, &stdout, &stderr); err != nil {
				t.Fatalf("Up --name devbox: %v", err)
			}
			want := ""
			if tc.where != "" {
				want = `comlink: warning: "devbox" already has ssh configuration (the Host line at ` + expand(tc.where) + ` matches it); this session's Host block takes precedence while it runs` + "\n"
			}
			if stderr.String() != want {
				t.Fatalf("Up --name devbox printed %q, want %q", stderr.String(), want)
			}
			if !strings.HasPrefix(stdout.String(), "devbox is up (pid ") {
				t.Fatalf("Up --name devbox stdout %q", stdout.String())
			}
		})
	}

	// comlink's own session files are not the user's configuration, even a
	// stale one for the name, also through an Include spelled with ~.
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "home", "state", "comlink")}
	config := filepath.Join(dir, "home", ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("Include "+filepath.Join(store.SSHDir(), "*.conf")+"\nInclude ~/state/comlink/ssh/*\nHost other\n  User someone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.SSHDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.SSHDir(), "devbox.conf"), []byte("Host devbox\n  User stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ server, name string }{{"wss://padawan.example.com", "padawan"}, {"wss://devbox.example.com", "devbox"}} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		if err := store.Up(ctx, "", tc.server, testToken, "/opt/comlink", config, &stdout, &stderr); err != nil {
			t.Fatalf("Up(%q) without a name: %v", tc.server, err)
		}
		if stderr.Len() != 0 || !strings.HasPrefix(stdout.String(), tc.name+" is up (pid ") {
			t.Fatalf("Up(%q) printed %q and %q", tc.server, stdout.String(), stderr.String())
		}
	}
}

// TestSessionUpExplicitNameCase: ssh matches Host patterns against the
// host exactly as typed, so an explicit Agent-X is configured by
// Host Agent-X and not by Host agent-x.
func TestSessionUpExplicitNameCase(t *testing.T) {
	for _, tc := range []struct{ pattern, warning string }{
		{"Agent-X", "configured"},
		{"Agent-*", "configured"},
		{"agent-x", ""},
		{"AGENT-X", ""},
	} {
		dir := t.TempDir()
		store := &session.Store{Dir: filepath.Join(dir, "comlink")}
		config := filepath.Join(dir, "home", ".ssh", "config")
		if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte("Include "+filepath.Join(store.SSHDir(), "*.conf")+"\nHost "+tc.pattern+"\n  User someone\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		if err := store.Up(ctx, "Agent-X", "wss://agent.example.com", testToken, "/opt/comlink", config, &stdout, &stderr); err != nil {
			t.Fatalf("Host %s: Up --name Agent-X: %v", tc.pattern, err)
		}
		want := ""
		if tc.warning != "" {
			want = `comlink: warning: "Agent-X" already has ssh configuration (the Host line at ` + config + `:2 matches it); this session's Host block takes precedence while it runs` + "\n"
		}
		if stderr.String() != want || !strings.HasPrefix(stdout.String(), "Agent-X is up (pid ") {
			t.Errorf("Host %s: Up --name Agent-X printed %q and %q, want stderr %q", tc.pattern, stdout.String(), stderr.String(), want)
		}
	}
}

// TestSessionUpComlinkHostPattern: a Host pattern for names that start
// with comlink, the base of a server without a usable host label, is the
// user's configuration like any other, however comlink tells patterns that
// match every host apart. Under Host comlink* both comlink and comlink-2
// are configured, so Up refuses; under Host comlink-* comlink is free,
// and with comlink up the next two candidates are configured, so Up
// refuses after skipping comlink; an explicit comlink-x is warned about.
func TestSessionUpComlinkHostPattern(t *testing.T) {
	for _, tc := range []struct {
		desc, pattern, up, err string
	}{
		{"Host comlink*", "comlink*", "", `"comlink" and "comlink-2" both have ssh configuration, so a pattern in it likely matches every comlink-N; pass --name NAME`},
		{"Host comlink-*", "comlink-*", "", ""},
		{"Host comlink-* with comlink up", "comlink-*", "comlink", `"comlink-2" and "comlink-3" both have ssh configuration, so a pattern in it likely matches every comlink-N; pass --name NAME`},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			dir := t.TempDir()
			store := &session.Store{Dir: filepath.Join(dir, "comlink")}
			config := filepath.Join(dir, "home", ".ssh", "config")
			if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte("Include "+filepath.Join(store.SSHDir(), "*.conf")+"\nHost "+tc.pattern+"\n  User someone\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.up != "" {
				sess, err := store.Start(tc.up, "ws://127.0.0.1:8787", testToken, "x")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = sess.Close() }()
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stdout, stderr bytes.Buffer
			err := store.Up(ctx, "", "ws://127.0.0.1:8787", testToken, "/opt/comlink", config, &stdout, &stderr)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err || stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("Up = %v, printed %q and %q; want %q", err, stdout.String(), stderr.String(), tc.err)
				}
				for _, name := range []string{"comlink-2", "comlink-3"} {
					record, token, _, conf := sessionFiles(store.Dir, name)
					absent(t, record, token, conf)
				}
				return
			}
			want := "comlink is up (pid " + strconv.Itoa(os.Getpid()) + ")\n  name:       comlink (the server host 127.0.0.1 is an IP address; --name sets another)\n"
			if err != nil || !strings.HasPrefix(stdout.String(), want) || stderr.Len() != 0 {
				t.Fatalf("Up = %v, printed:\n%s\n%q\nwant prefix:\n%s", err, stdout.String(), stderr.String(), want)
			}

			stdout.Reset()
			if err := store.Up(ctx, "comlink-x", "ws://127.0.0.1:8787", testToken, "/opt/comlink", config, &stdout, &stderr); err != nil {
				t.Fatalf("Up --name comlink-x: %v", err)
			}
			if want := `comlink: warning: "comlink-x" already has ssh configuration (the Host line at ` + config + `:2 matches it); this session's Host block takes precedence while it runs` + "\n"; stderr.String() != want {
				t.Fatalf("Up --name comlink-x printed %q, want %q", stderr.String(), want)
			}
		})
	}
}

// TestSessionUpConfiguredNameThatIsUp: an explicit name that the user's ssh
// configuration applies to and that is already up is refused with the
// already-up error alone; the warning that the session's Host block takes
// precedence is not printed, because no session comes up.
func TestSessionUpConfiguredNameThatIsUp(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	config := filepath.Join(dir, "home", ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("Include "+filepath.Join(store.SSHDir(), "*.conf")+"\nHost devbox\n  User someone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Start("devbox", "wss://devbox.example.com", testToken, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	var stdout, stderr bytes.Buffer
	err = store.Up(context.Background(), "devbox", "wss://devbox.example.com", testToken, "/opt/comlink", config, &stdout, &stderr)
	if want := fmt.Sprintf("session %q is already up (pid %d)", "devbox", os.Getpid()); err == nil || err.Error() != want {
		t.Fatalf("Up --name devbox = %v, want %q", err, want)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("refused Up printed %q and %q", stdout.String(), stderr.String())
	}
}

// TestSessionResolveAndPrintList: Resolve returns the session's Codex
// endpoint with the token, and PrintList prints the live sessions, the
// removed stale ones and the empty case.
func TestSessionResolveAndPrintList(t *testing.T) {
	dir := t.TempDir()
	store := &session.Store{Dir: filepath.Join(dir, "comlink")}
	var stdout, stderr bytes.Buffer
	if err := store.PrintList(&stdout, &stderr); err != nil || stdout.Len() != 0 || stderr.String() != "comlink: no sessions are up\n" {
		t.Fatalf("PrintList with nothing up: %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}

	b, err := store.Start("agent-b", "wss://b.example.com/x/", testToken, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	a, err := store.Start("agent-a", "wss://a.example.com", testToken, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	writeStaleSession(t, dir, "agent-c")
	stalePID := readRecord(t, filepath.Join(dir, "comlink", "run", "agent-c.json")).PID

	for _, tc := range []struct{ name, want string }{
		{"agent-a", "wss://a.example.com/codex"},
		{"agent-b", "wss://b.example.com/x/codex"},
	} {
		if url, token, err := store.Resolve(tc.name); err != nil || url != tc.want || token != testToken {
			t.Errorf("Resolve(%q) = %q, token ok %v, %v; want %q", tc.name, url, token == testToken, err, tc.want)
		}
	}
	var notUp *session.NotUpError
	if _, _, err := store.Resolve("agent-c"); !errors.As(err, &notUp) {
		t.Fatalf("Resolve of a stale name: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := store.PrintList(&stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	if want := "agent-a\tup\tpid " + pid + "\twss://a.example.com\nagent-b\tup\tpid " + pid + "\twss://b.example.com/x/\n"; stdout.String() != want {
		t.Fatalf("PrintList stdout %q, want %q", stdout.String(), want)
	}
	if want := "comlink: removed stale session \"agent-c\" (pid " + strconv.Itoa(stalePID) + " is gone)\n"; stderr.String() != want {
		t.Fatalf("PrintList stderr %q, want %q", stderr.String(), want)
	}
	if strings.Contains(stdout.String(), testToken) {
		t.Fatal("PrintList printed the token")
	}
	// A run directory that cannot be read is an error.
	if os.Getuid() != 0 {
		run := filepath.Join(dir, "comlink", "run")
		if err := os.Chmod(run, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(run, 0o700) })
		if err := store.PrintList(io.Discard, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "read "+run+": ") {
			t.Fatalf("PrintList on an unreadable run directory: %v", err)
		}
		if err := os.Chmod(run, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSessionFlagsCheck: without --stdio a session needs both --name and
// --server and refuses --report-version; with it, exactly one of --name and
// --server, and --token-file only with --server. CheckUnset refuses any of
// them for a subcommand.
func TestSessionFlagsCheck(t *testing.T) {
	for _, tc := range []struct {
		flags session.Flags
		want  string
	}{
		{session.Flags{Name: "a", Server: "wss://h"}, ""},
		{session.Flags{Name: "a", Server: "wss://h", TokenFile: "/t"}, ""},
		{session.Flags{}, "missing flags: --server=URL"},
		{session.Flags{Name: "a"}, "missing flags: --server=URL"},
		{session.Flags{TokenFile: "/t"}, "missing flags: --server=URL"},
		{session.Flags{Server: "wss://h"}, ""},
		{session.Flags{Server: "wss://h", TokenFile: "/t"}, ""},
		{session.Flags{Name: "a", Server: "wss://h", ReportVersion: "1"}, "--report-version goes with --stdio"},
		{session.Flags{Stdio: true, Name: "a"}, ""},
		{session.Flags{Stdio: true, Name: "a", ReportVersion: "1"}, ""},
		{session.Flags{Stdio: true, Server: "wss://h", TokenFile: "/t", ReportVersion: "1"}, ""},
		{session.Flags{Stdio: true}, "--stdio needs --name NAME (a session from 'comlink codex --name NAME') or --server URL"},
		{session.Flags{Stdio: true, TokenFile: "/t"}, "--stdio needs --name NAME (a session from 'comlink codex --name NAME') or --server URL"},
		{session.Flags{Stdio: true, Name: "a", Server: "wss://h"}, "--stdio takes --name or --server, not both"},
		{session.Flags{Stdio: true, Name: "a", TokenFile: "/t"}, "--token-file goes with --server; a session's token comes from 'comlink codex --name NAME'"},
	} {
		got := ""
		if err := tc.flags.Check(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("Check(%+v) = %q, want %q", tc.flags, got, tc.want)
		}
	}

	if err := (session.Flags{}).CheckUnset("list"); err != nil {
		t.Fatalf("CheckUnset with nothing set: %v", err)
	}
	want := "'comlink codex list' takes no flags; --stdio, --name, --server, --token-file and --report-version go with 'comlink codex' itself"
	for _, f := range []session.Flags{{Stdio: true}, {Name: "a"}, {Server: "wss://h"}, {TokenFile: "/t"}, {ReportVersion: "1"}} {
		if err := f.CheckUnset("list"); err == nil || err.Error() != want {
			t.Errorf("CheckUnset(%+v) = %v, want %q", f, err, want)
		}
	}
}

// TestSessionFlagsEndpoint: --stdio --server dials <server>/codex with the
// token from --token-file or $COMLINK_TOKEN and does not need the state
// directory; --stdio --name uses the running session's endpoint and token
// and refuses a name that is not up.
func TestSessionFlagsEndpoint(t *testing.T) {
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("COMLINK_TOKEN", testToken)
	if url, token, err := (session.Flags{Stdio: true, Server: "wss://agent.example.com/base/"}).Endpoint(logger); err != nil || url != "wss://agent.example.com/base/codex" || token != testToken {
		t.Fatalf("Endpoint(--server) = %q, token ok %v, %v", url, token == testToken, err)
	}
	t.Setenv("COMLINK_TOKEN", "")
	if url, token, err := (session.Flags{Stdio: true, Server: "ws://127.0.0.1:1", TokenFile: writeTokenFile(t, 0o600)}).Endpoint(logger); err != nil || url != "ws://127.0.0.1:1/codex" || token != testToken {
		t.Fatalf("Endpoint(--server --token-file) = %q, token ok %v, %v", url, token == testToken, err)
	}
	if _, _, err := (session.Flags{Stdio: true, Server: "https://agent.example.com"}).Endpoint(logger); err == nil || err.Error() != `server URL "https://agent.example.com": want ws://host[/prefix] or wss://host[/prefix]` {
		t.Fatalf("Endpoint(https) = %v", err)
	}
	if _, _, err := (session.Flags{Stdio: true, Server: "ws://127.0.0.1:1", TokenFile: filepath.Join(t.TempDir(), "absent")}).Endpoint(logger); err == nil || !strings.HasPrefix(err.Error(), "read token file: ") {
		t.Fatalf("Endpoint with a missing token file = %v", err)
	}
	if _, _, err := (session.Flags{Stdio: true, Name: "agent-a"}).Endpoint(logger); err == nil || !strings.HasPrefix(err.Error(), "locate state directory: ") {
		t.Fatalf("Endpoint(--name) without a state directory = %v", err)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("log %q", logBuf.String())
	}

	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	store, err := session.Open()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Start("agent-a", "wss://agent.example.com/x", testToken, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if url, token, err := (session.Flags{Stdio: true, Name: "agent-a"}).Endpoint(logger); err != nil || url != "wss://agent.example.com/x/codex" || token != testToken {
		t.Fatalf("Endpoint(--name) = %q, token ok %v, %v", url, token == testToken, err)
	}
	var notUp *session.NotUpError
	if _, _, err := (session.Flags{Stdio: true, Name: "agent-b"}).Endpoint(logger); !errors.As(err, &notUp) {
		t.Fatalf("Endpoint(--name) of a name that is not up = %v", err)
	}
}
