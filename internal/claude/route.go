// Package claude is the Claude desktop app's route. The app reaches a
// remote machine over SSH, probes it with shell commands, uploads its helper
// over SFTP and talks JSON-RPC to that helper on an exec channel to spawn
// Claude Code and move its stream-json in and out. comlink answers the
// probe from a virtual home directory, accepts the uploads without keeping
// them, serves the helper's RPC itself and stands in for Claude Code: each
// chat the app opens is an ACP session on the upstream WebSocket.
package claude

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"regexp"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/upstream"
)

// DefaultHome is the home directory the route reports when Home is empty.
const DefaultHome = "/home/comlink"

// uid is the numeric user the route reports (id -u, ls -ln, SFTP attrs).
const uid = 1000

// Route answers the Claude app's exec and sftp requests and relays each of
// its chats to Upstream as an ACP session. One Route serves one SSH
// connection (one ProxyCommand process); its state lives as long as it.
type Route struct {
	Upstream   string              // ACP WebSocket endpoint that receives the sessions
	Credential upstream.Credential // Upstream's bearer token, resolved when a session dials; none sends no Authorization header
	Logger     *log.Logger         // diagnostics; never given the token
	Home       string              // home directory reported to the app; DefaultHome when empty
	Version    string              // comlink's version, sent to the upstream as the ACP client version
	Chats      string              // directory mapping the app's chats to upstream ACP sessions, for resuming them; empty keeps none

	once     sync.Once
	fs       *vfs
	procs    *procs
	mu       sync.Mutex
	helper   string // the helper hash the app expects, from its command lines
	instance string // server.instance_id, made on the first capabilities call
	started  time.Time
}

var (
	helperRe = regexp.MustCompile(`/\.claude/remote/srv/([0-9a-f]{40})/server`)
	cliRe    = regexp.MustCompile(`/\.claude/remote/ccd-cli/([0-9A-Za-z.\-]+)`)
)

func (r *Route) init() {
	r.once.Do(func() {
		if r.Home == "" {
			r.Home = DefaultHome
		}
		r.fs = newVFS(r.Home)
		// The app shows a chat it reopens from its transcript on the host.
		for _, t := range (chats{r.Chats}).transcripts(r.Upstream) {
			r.fs.put(r.transcriptPath(t.cwd, t.chat), t.data, 0o600)
		}
		r.procs = newProcs(r)
	})
}

// transcriptPath is where Claude Code would keep chat's transcript for cwd.
func (r *Route) transcriptPath(cwd, chat string) string {
	return r.Home + "/.claude/projects/" + projectDir(cwd) + "/" + chat + ".jsonl"
}

// helperVersion is the helper hash the app named last, or a fixed one
// before it named any.
func (r *Route) helperVersion() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.helper == "" {
		return "0000000000000000000000000000000000000000"
	}
	return r.helper
}

// learn takes the helper hash and CLI version out of a command line and
// makes the files the app would find after its own installation exist:
// then the app's probe reaches its bridge without installing anything.
func (r *Route) learn(command string) {
	for _, m := range helperRe.FindAllStringSubmatch(command, -1) {
		r.mu.Lock()
		r.helper = m[1]
		r.mu.Unlock()
		r.fs.ensure(r.Home+"/.claude/remote/srv/"+m[1]+"/server", 0o700)
	}
	for _, m := range cliRe.FindAllStringSubmatch(command, -1) {
		r.fs.ensure(r.Home+"/.claude/remote/ccd-cli/"+m[1], 0o755)
	}
}

// Exec answers one exec request on ch and returns the exit status. The
// command is interpreted against the virtual tree; nothing is run. A
// command that ends in the helper's --bridge serves the helper's RPC on ch
// until the app closes the channel.
func (r *Route) Exec(ch ssh.Channel, command string) int {
	r.init()
	r.learn(command)
	sh := &shell{route: r, fs: r.fs, cwd: r.Home, out: ch, errOut: ch.Stderr()}
	sh.bridge = func(int) (int, bool) { return r.serveBridge(ch), true }
	return sh.run(command)
}

// Subsystem serves the sftp subsystem on the virtual tree; any other name
// is refused.
func (r *Route) Subsystem(name string) (func(ssh.Channel) int, bool) {
	if name != "sftp" {
		return nil, false
	}
	r.init()
	return func(ch ssh.Channel) int { return serveSFTP(ch, r.fs) }, true
}

// randomID is n random bytes as hex.
func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
