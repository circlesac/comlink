package test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/claude"
	"github.com/circlesac/comlink/internal/upstream"
)

// The Claude app's captured commands (test/testdata/claude-app-*.txt) name
// this helper hash, CLI version and run id.
const (
	claudeHelper = "b3081b512da230f24d74a2082a678246a89685ee"
	claudeCLI    = "/home/comlink/.claude/remote/ccd-cli/2.1.317-42a2f87f038a"
	claudeRun    = "/home/comlink/.claude/remote/run/02b2e3e9"
)

// startClaude runs the server with the Claude route and returns the client
// plus the route's log, which must never contain the token.
func startClaude(t *testing.T, route *claude.Route) (*ssh.Client, *lockedBuffer) {
	t.Helper()
	var logBuf lockedBuffer
	route.Logger = log.New(&logBuf, "comlink: ", 0)
	client := startServer(t, route)
	t.Cleanup(func() {
		if strings.Contains(logBuf.String(), testToken) {
			t.Error("server log contains the upstream token")
		}
	})
	return client, &logBuf
}

// agentUpstream is a fake comlink server's /acp endpoint: it requires the
// bearer token, answers initialize and session/new, and runs each
// session/prompt through the scenario the test set.
type agentUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	auths    []string
	inits    []json.RawMessage
	prompts  []string
	cancels  int
	scenario func(s *acpConvo, prompt string)
	// session/load: offered when loadSession is set, refused when loadFails is
	news        int
	loads       []string
	loadSession bool
	loadFails   bool
}

// acpConvo is one upstream connection's side of the ACP conversation.
type acpConvo struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
	mu   sync.Mutex
	next int
	// replies to the upstream's own requests (permission), by id
	pending   map[string]chan json.RawMessage
	cancelled chan struct{} // closed when the app cancels the session
}

func (s *acpConvo) write(v any) {
	b, _ := json.Marshal(v)
	if err := s.conn.Write(s.ctx, websocket.MessageText, b); err != nil {
		s.t.Logf("upstream write: %v", err)
	}
}

// update sends a session/update notification.
func (s *acpConvo) update(update map[string]any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "sess-1", "update": update}})
}

func (s *acpConvo) text(text string) {
	s.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
}

// askPermission sends session/request_permission and returns the chosen
// option id.
func (s *acpConvo) askPermission(title string, options ...string) string {
	s.mu.Lock()
	s.next++
	id := fmt.Sprintf("up-%d", s.next)
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	var opts []map[string]any
	for _, o := range options {
		opts = append(opts, map[string]any{"optionId": o, "name": o, "kind": o})
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/request_permission", "params": map[string]any{"sessionId": "sess-1", "toolCall": map[string]any{"toolCallId": "call-1", "title": title, "kind": "execute", "rawInput": map[string]any{"command": "echo hi"}}, "options": opts}})
	select {
	case raw := <-ch:
		var r struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		_ = json.Unmarshal(raw, &r)
		if r.Outcome.Outcome != "selected" {
			return r.Outcome.Outcome
		}
		return r.Outcome.OptionID
	case <-time.After(10 * time.Second):
		s.t.Error("no permission reply within 10s")
		return ""
	}
}

func newAgentUpstream(t *testing.T) *agentUpstream {
	t.Helper()
	u := &agentUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		if r.URL.Path != "/acp" {
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
		s := &acpConvo{t: t, conn: c, ctx: r.Context(), pending: map[string]chan json.RawMessage{}, cancelled: make(chan struct{})}
		var promptID json.RawMessage
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(data, &m); err != nil {
				t.Errorf("upstream got a message that is not JSON: %s", data)
				continue
			}
			switch m.Method {
			case "initialize":
				u.mu.Lock()
				u.inits = append(u.inits, m.Params)
				u.mu.Unlock()
				u.mu.Lock()
				caps := map[string]any{"loadSession": u.loadSession}
				u.mu.Unlock()
				s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1, "agentCapabilities": caps}})
			case "session/new":
				u.mu.Lock()
				u.news++
				id := fmt.Sprintf("sess-%d", u.news)
				u.mu.Unlock()
				s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessionId": id}})
			case "session/load":
				var p struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(m.Params, &p)
				u.mu.Lock()
				u.loads = append(u.loads, p.SessionID)
				fails := u.loadFails
				u.mu.Unlock()
				if fails {
					s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32004, "message": "session_not_available"}})
					continue
				}
				// An agent replays the session's history before it answers.
				s.text("earlier answer")
				s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
			case "session/prompt":
				var p struct {
					Prompt []struct {
						Text string `json:"text"`
					} `json:"prompt"`
				}
				_ = json.Unmarshal(m.Params, &p)
				text := ""
				if len(p.Prompt) > 0 {
					text = p.Prompt[0].Text
				}
				u.mu.Lock()
				u.prompts = append(u.prompts, text)
				scenario := u.scenario
				u.mu.Unlock()
				promptID = m.ID
				go func(id json.RawMessage) {
					if scenario != nil {
						scenario(s, text)
					}
					s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
				}(promptID)
			case "session/cancel":
				u.mu.Lock()
				u.cancels++
				u.mu.Unlock()
				close(s.cancelled)
			case "":
				// A reply to one of the upstream's own requests.
				s.mu.Lock()
				ch := s.pending[string(bytes.Trim(m.ID, `"`))]
				delete(s.pending, string(bytes.Trim(m.ID, `"`)))
				s.mu.Unlock()
				if ch != nil {
					ch <- m.Result
				}
			default:
				s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			}
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *agentUpstream) wsURL() string { return "ws" + strings.TrimPrefix(u.URL, "http") + "/acp" }

// bridge is an exec channel running the helper's --bridge: requests go in
// as lines, replies and stream records come out as lines.
type bridge struct {
	t     *testing.T
	sess  *ssh.Session
	stdin io.WriteCloser
	br    *bufio.Reader
	next  int
	// stream records read while waiting for a reply, in order
	records []streamRecord
	wait    chan error
}

type streamRecord struct {
	ProcessID string `json:"processId"`
	Stream    string `json:"stream"`
	Seq       int    `json:"seq"`
	Data      string `json:"data"`
	ExitCode  int    `json:"exitCode"`
	End       struct {
		At  int `json:"at"`
		Len int `json:"len"`
	} `json:"end"`
}

// openBridge runs exec on a new session and returns the bridge once exec's
// own output, up to and including the line last, has been read.
func openBridge(t *testing.T, client *ssh.Client, exec, last string) *bridge {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	sess.Stderr = io.Discard
	if err := sess.Start(exec); err != nil {
		t.Fatal(err)
	}
	b := &bridge{t: t, sess: sess, stdin: stdin, br: bufio.NewReaderSize(stdout, 1<<20), wait: make(chan error, 1)}
	go func() { b.wait <- sess.Wait() }()
	t.Cleanup(func() { _ = sess.Close() })
	if last != "" {
		for {
			line := b.line()
			if line == last {
				break
			}
		}
	}
	return b
}

// line reads one output line without its newline; it fails the test after
// 10 seconds.
func (b *bridge) line() string {
	b.t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := b.br.ReadString('\n')
		ch <- res{strings.TrimSuffix(line, "\n"), err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			b.t.Fatalf("read bridge output: %v (got %q)", r.err, r.line)
		}
		return r.line
	case <-time.After(10 * time.Second):
		b.t.Fatal("no bridge output within 10s")
		return ""
	}
}

// call sends one request and returns its result; stream records read
// meanwhile are kept in records. The reply's end marker is checked against
// the line.
func (b *bridge) call(method string, params any) json.RawMessage {
	b.t.Helper()
	b.next++
	id := fmt.Sprintf("req-%d", b.next)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "auth": "token-from-sftp"}
	if params != nil {
		req["params"] = params
	}
	line, _ := json.Marshal(req)
	if _, err := b.stdin.Write(append(line, '\n')); err != nil {
		b.t.Fatalf("write request: %v", err)
	}
	for {
		line := b.line()
		var reply struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Type string `json:"type"`
			End  *struct {
				At  int `json:"at"`
				Len int `json:"len"`
			} `json:"end"`
		}
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			b.t.Fatalf("bridge line is not JSON: %q", line)
		}
		if reply.End == nil {
			b.t.Fatalf("bridge line has no end marker: %q", line)
		}
		if want := len(line) - len(fmt.Sprintf(`,"end":{"at":%d,"len":%d}}`, reply.End.At, reply.End.Len)); reply.End.Len != want {
			b.t.Fatalf("end.len = %d, want %d for %q", reply.End.Len, want, line)
		}
		if reply.Type == "stream" {
			var rec streamRecord
			_ = json.Unmarshal([]byte(line), &rec)
			b.records = append(b.records, rec)
			continue
		}
		if reply.ID != id {
			b.t.Fatalf("reply %q to another request: %s", reply.ID, line)
		}
		if reply.Error != nil {
			b.t.Fatalf("%s: error %d %s", method, reply.Error.Code, reply.Error.Message)
		}
		return reply.Result
	}
}

// callErr sends one request that must fail and returns the error message.
func (b *bridge) callErr(method string, params any) string {
	b.t.Helper()
	b.next++
	id := fmt.Sprintf("req-%d", b.next)
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := b.stdin.Write(append(line, '\n')); err != nil {
		b.t.Fatalf("write request: %v", err)
	}
	for {
		var reply struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		line := b.line()
		_ = json.Unmarshal([]byte(line), &reply)
		if reply.Type == "stream" {
			continue
		}
		if reply.ID != id || reply.Error == nil {
			b.t.Fatalf("want an error reply to %s, got %s", method, line)
		}
		return reply.Error.Message
	}
}

// record reads stream records until one of process id on stream (any when
// empty) arrives.
func (b *bridge) record(id, stream string) streamRecord {
	b.t.Helper()
	for {
		if len(b.records) > 0 {
			rec := b.records[0]
			b.records = b.records[1:]
			if rec.ProcessID == id && (stream == "" || rec.Stream == stream) {
				return rec
			}
			continue
		}
		var rec streamRecord
		line := b.line()
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.ProcessID == "" {
			b.t.Fatalf("want a stream record, got %q", line)
		}
		b.records = append(b.records, rec)
	}
}

// stdin feeds data to process id and returns the applied count.
func (b *bridge) stdin_(id string, data string) int {
	b.t.Helper()
	var r struct {
		Success bool `json:"success"`
		Applied int  `json:"applied"`
	}
	_ = json.Unmarshal(b.call("process.stdin", map[string]any{"id": id, "data": base64.StdEncoding.EncodeToString([]byte(data))}), &r)
	if !r.Success {
		b.t.Fatal("process.stdin did not succeed")
	}
	return r.Applied
}

// cliReader decodes the stream-json lines of the virtual Claude Code
// process id out of the bridge's stdout records.
type cliReader struct {
	b    *bridge
	id   string
	buf  []byte
	seqs []int
}

// next returns the next stream-json message of the process.
func (r *cliReader) next() map[string]any {
	r.b.t.Helper()
	for {
		if i := bytes.IndexByte(r.buf, '\n'); i >= 0 {
			line := r.buf[:i]
			r.buf = r.buf[i+1:]
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				r.b.t.Fatalf("process wrote a line that is not JSON: %q", line)
			}
			return m
		}
		rec := r.b.record(r.id, "stdout")
		data, err := base64.StdEncoding.DecodeString(rec.Data)
		if err != nil {
			r.b.t.Fatalf("stream data is not base64: %q", rec.Data)
		}
		r.seqs = append(r.seqs, rec.Seq)
		r.buf = append(r.buf, data...)
	}
}

// expect reads messages until one of type (and subtype, when given) and
// returns it; any other message is checked to be one the app understands.
func (r *cliReader) expect(typ, subtype string) map[string]any {
	r.b.t.Helper()
	for {
		m := r.next()
		if m["session_id"] == nil {
			r.b.t.Fatalf("message without session_id: %v", m)
		}
		short, _ := json.Marshal(m)
		if len(short) > 300 {
			short = short[:300]
		}
		r.b.t.Logf("process %s: %s", r.id, short)
		if m["type"] == typ && (subtype == "" || m["subtype"] == subtype || stateOf(m) == subtype) {
			return m
		}
	}
}

func stateOf(m map[string]any) string {
	if s, ok := m["state"].(string); ok {
		return s
	}
	if e, ok := m["event"].(map[string]any); ok {
		s, _ := e["type"].(string)
		return s
	}
	return ""
}

func TestClaudeProbeReachesTheBridge(t *testing.T) {
	client, _ := startClaude(t, &claude.Route{Upstream: unusedUpstream})
	b := openBridge(t, client, readFixture(t, "claude-app-probe.txt"), "")
	// The probe's own output, up to the bridge taking over.
	var lines []string
	for {
		line := b.line()
		lines = append(lines, line)
		if line == "__PRBEND__" {
			break
		}
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"__PRBG__\n__PRB1__\n__PRB2__\n/home/comlink\n__PRB3__\nLinux x86_64\n__PRB4__\n1000\n", "__PRB5__\n/home/comlink\n__PRB6__\n__ENV_RAN__\n__PRB7__\n", "__CCD_TREE__\n" + claudeRun + "\n__CCD_TREE__\n__CCD_TREE__\n" + claudeRun + "/tree.", "total 0\n", "__PRB8__\nclaude-ssh " + claudeHelper + " (built ", "__PRB9__\n__PRB10__\n__PRBEND__"} {
		if !strings.Contains(got, want) {
			t.Errorf("probe output lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"__CLI_BAD__", "__NOEXEC__", "__CCD_TREE_NO_CD__"} {
		if strings.Contains(got, bad) {
			t.Errorf("probe output has %s:\n%s", bad, got)
		}
	}
	// The mktemp file exists for the app's SFTP check.
	var tree string
	for i, line := range lines {
		if line == "__CCD_TREE__" && i+1 < len(lines) && strings.HasPrefix(lines[i+1], claudeRun+"/tree.") {
			tree = lines[i+1]
		}
	}
	if tree == "" {
		t.Fatalf("no mktemp path in:\n%s", got)
	}
	// What follows is the bridge: server.capabilities answers.
	var caps struct {
		Version  string   `json:"version"`
		Methods  []string `json:"methods"`
		Features []string `json:"features"`
		Instance string   `json:"instanceId"`
	}
	if err := json.Unmarshal(b.call("server.capabilities", nil), &caps); err != nil || caps.Version != claudeHelper || len(caps.Instance) != 32 {
		t.Fatalf("capabilities: %+v, %v", caps, err)
	}
	for _, m := range []string{"process.spawn", "process.stdin", "process.reattach", "files.read", "files.stat", "files.list", "git.info", "plugins.prune"} {
		if !contains(caps.Methods, m) {
			t.Errorf("capabilities lack %s: %v", m, caps.Methods)
		}
	}
	if !contains(caps.Features, "server.line_end") || !contains(caps.Features, "process.stdin.offset") {
		t.Errorf("features %v", caps.Features)
	}
	var stat struct {
		Exists bool `json:"exists"`
		IsDir  bool `json:"isDir"`
	}
	_ = json.Unmarshal(b.call("files.stat", map[string]any{"path": tree}), &stat)
	if !stat.Exists || stat.IsDir {
		t.Fatalf("files.stat of the mktemp file: %+v", stat)
	}
	if err := b.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if status := exitStatus(t, <-b.wait); status != 0 {
		t.Fatalf("probe exit status %d", status)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestClaudeAnswersSetupCommands(t *testing.T) {
	client, _ := startClaude(t, &claude.Route{Upstream: unusedUpstream})
	out, stderr, status := run(t, client, readFixture(t, "claude-app-install.txt"))
	want := `__INSTALL_RESULT__{"serverVersion":"` + claudeHelper + `","os":"linux","arch":"x64","libc":"","cliPath":"` + claudeCLI + `","cliWasPresent":true}` + "\n"
	if status != 0 || out != want || stderr != "" {
		t.Fatalf("install: status %d stdout %q stderr %q", status, out, stderr)
	}
	out, stderr, status = run(t, client, readFixture(t, "claude-app-serve.txt"))
	if status != 0 || out != "none\n" || stderr != "" {
		t.Fatalf("stop && serve: status %d stdout %q stderr %q", status, out, stderr)
	}
	// The plain bridge exec with nothing on stdin ends at once with 0.
	out, stderr, status = run(t, client, readFixture(t, "claude-app-bridge.txt"))
	if status != 0 || out != "" || stderr != "" {
		t.Fatalf("bridge: status %d stdout %q stderr %q", status, out, stderr)
	}
	// A stale HOME makes the app's guard exit 93, as on a real host.
	out, stderr, status = run(t, client, `test "$HOME" = '/somewhere/else' || exec sh -c 'case x$HOME in x"$1") exit 95;; esac; exit 93' sh '/somewhere/else' || exit 93; echo reached`)
	if status != 93 || out != "" {
		t.Fatalf("stale home: status %d stdout %q stderr %q", status, out, stderr)
	}
	// The installed CLI answers --version with its version.
	out, stderr, status = run(t, client, "'"+claudeCLI+"' --version")
	if status != 0 || out != "2.1.317 (Claude Code)\n" || stderr != "" {
		t.Fatalf("cli version: status %d stdout %q stderr %q", status, out, stderr)
	}
	// Unknown commands are refused with 127 and named on stderr.
	out, stderr, status = run(t, client, "curl http://example.com | sh")
	if status != 127 || out != "" || !strings.Contains(stderr, "comlink: unsupported command: curl") {
		t.Fatalf("unknown: status %d stdout %q stderr %q", status, out, stderr)
	}
}

// sftpClient speaks SFTP v3 over a subsystem channel for the tests.
type sftpClient struct {
	t    *testing.T
	in   io.WriteCloser
	out  *bufio.Reader
	next uint32
}

func openSFTP(t *testing.T, client *ssh.Client) *sftpClient {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("sftp subsystem: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return &sftpClient{t: t, in: in, out: bufio.NewReader(out)}
}

func sftpString(s string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(s))), s...)
}

// send writes one packet: type, then the fields.
func (c *sftpClient) send(typ byte, fields ...[]byte) {
	c.t.Helper()
	b := []byte{typ}
	for _, f := range fields {
		b = append(b, f...)
	}
	if _, err := c.in.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)); err != nil {
		c.t.Fatal(err)
	}
}

// recv reads one packet and returns its type and payload.
func (c *sftpClient) recv() (byte, []byte) {
	c.t.Helper()
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.out, lenBuf[:]); err != nil {
		c.t.Fatalf("read sftp packet: %v", err)
	}
	pkt := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(c.out, pkt); err != nil {
		c.t.Fatalf("read sftp packet: %v", err)
	}
	return pkt[0], pkt[1:]
}

// request sends a request with a fresh id and returns the reply's type and
// payload after the id.
func (c *sftpClient) request(typ byte, fields ...[]byte) (byte, []byte) {
	c.t.Helper()
	c.next++
	c.send(typ, append([][]byte{binary.BigEndian.AppendUint32(nil, c.next)}, fields...)...)
	rtyp, payload := c.recv()
	if binary.BigEndian.Uint32(payload) != c.next {
		c.t.Fatalf("reply id %d, want %d", binary.BigEndian.Uint32(payload), c.next)
	}
	return rtyp, payload[4:]
}

// status returns the status code of a STATUS reply.
func (c *sftpClient) status(typ byte, payload []byte) uint32 {
	c.t.Helper()
	if typ != 101 {
		c.t.Fatalf("reply type %d, want STATUS", typ)
	}
	return binary.BigEndian.Uint32(payload)
}

func TestClaudeServesSFTPOnTheVirtualTree(t *testing.T) {
	client, _ := startClaude(t, &claude.Route{Upstream: unusedUpstream})
	c := openSFTP(t, client)
	c.send(1, binary.BigEndian.AppendUint32(nil, 3))
	if typ, payload := c.recv(); typ != 2 || binary.BigEndian.Uint32(payload) != 3 || !bytes.Contains(payload, []byte("posix-rename@openssh.com")) {
		t.Fatalf("VERSION: type %d payload %q", typ, payload)
	}
	typ, payload := c.request(200, sftpString("limits@openssh.com"))
	if typ != 201 || len(payload) != 32 || binary.BigEndian.Uint64(payload) != 262144 {
		t.Fatalf("limits: type %d payload %x", typ, payload)
	}
	// The home directory and its parents exist, as the app's LSTAT chain
	// expects; what the app never made does not.
	for _, dir := range []string{"/", "/home", "/home/comlink", "/home/comlink/.claude/remote/run"} {
		if typ, payload := c.request(7, sftpString(dir)); typ != 105 || binary.BigEndian.Uint32(payload[4+8+8:])&0o40000 == 0 {
			t.Fatalf("LSTAT %s: type %d payload %x", dir, typ, payload)
		}
	}
	if typ, payload := c.request(17, sftpString(claudeRun+"/tree.missing")); c.status(typ, payload) != 2 {
		t.Fatalf("STAT of a missing file: status %d", c.status(typ, payload))
	}
	// The token the app writes: OPEN with create|trunc|write, WRITE, CLOSE.
	token := claudeRun + "/token.48d050350390fb1c"
	typ, payload = c.request(3, sftpString(token), binary.BigEndian.AppendUint32(nil, 2|8|16), binary.BigEndian.AppendUint32(nil, 0))
	if typ != 102 {
		t.Fatalf("OPEN for write: type %d", typ)
	}
	handle := payload
	if typ, payload := c.request(6, handle, binary.BigEndian.AppendUint64(nil, 0), sftpString("060a6a9c-7e57-4e82-9e3e-6495fbdf6024")); c.status(typ, payload) != 0 {
		t.Fatalf("WRITE: status %d", c.status(typ, payload))
	}
	if typ, payload := c.request(8, handle); typ != 105 || binary.BigEndian.Uint64(payload[4:]) != 36 {
		t.Fatalf("FSTAT: type %d payload %x", typ, payload)
	}
	if typ, payload := c.request(4, handle); c.status(typ, payload) != 0 {
		t.Fatal("CLOSE failed")
	}
	// Read it back: STAT, OPEN for read, READ, READ at the end, CLOSE.
	if typ, payload := c.request(17, sftpString(token)); typ != 105 || binary.BigEndian.Uint64(payload[4:]) != 36 {
		t.Fatalf("STAT after write: type %d payload %x", typ, payload)
	}
	typ, payload = c.request(3, sftpString(token), binary.BigEndian.AppendUint32(nil, 1), binary.BigEndian.AppendUint32(nil, 0))
	if typ != 102 {
		t.Fatalf("OPEN for read: type %d", typ)
	}
	handle = payload
	typ, payload = c.request(5, handle, binary.BigEndian.AppendUint64(nil, 0), binary.BigEndian.AppendUint32(nil, 8192))
	if typ != 103 || string(payload[4:]) != "060a6a9c-7e57-4e82-9e3e-6495fbdf6024" {
		t.Fatalf("READ: type %d payload %q", typ, payload)
	}
	if typ, payload := c.request(5, handle, binary.BigEndian.AppendUint64(nil, 36), binary.BigEndian.AppendUint32(nil, 8192)); c.status(typ, payload) != 1 {
		t.Fatalf("READ at the end: status %d, want EOF", c.status(typ, payload))
	}
	c.request(4, handle)
	// REALPATH cleans; MKDIR, posix-rename, READDIR and REMOVE work on the tree.
	if typ, payload := c.request(16, sftpString("/home/comlink/.claude/../.claude/remote")); typ != 104 || string(payload[8:8+len("/home/comlink/.claude/remote")]) != "/home/comlink/.claude/remote" {
		t.Fatalf("REALPATH: type %d payload %q", typ, payload)
	}
	if typ, payload := c.request(14, sftpString("/home/comlink/.claude/remote/plugins/02b2e3e9/abc"), binary.BigEndian.AppendUint32(nil, 0)); c.status(typ, payload) != 0 {
		t.Fatal("MKDIR failed")
	}
	if typ, payload := c.request(200, sftpString("posix-rename@openssh.com"), sftpString(token), sftpString(claudeRun+"/token.moved")); c.status(typ, payload) != 0 {
		t.Fatal("posix-rename failed")
	}
	typ, payload = c.request(11, sftpString(claudeRun))
	if typ != 102 {
		t.Fatalf("OPENDIR: type %d", typ)
	}
	handle = payload
	if typ, payload := c.request(12, handle); typ != 104 || !bytes.Contains(payload, []byte("token.moved")) || bytes.Contains(payload, []byte("token.48d050350390fb1c")) {
		t.Fatalf("READDIR: type %d payload %q", typ, payload)
	}
	if typ, payload := c.request(12, handle); c.status(typ, payload) != 1 {
		t.Fatal("second READDIR is not EOF")
	}
	if typ, payload := c.request(13, sftpString(claudeRun+"/token.moved")); c.status(typ, payload) != 0 {
		t.Fatal("REMOVE failed")
	}
	if typ, payload := c.request(13, sftpString(claudeRun+"/token.moved")); c.status(typ, payload) != 2 {
		t.Fatal("REMOVE of a removed file is not NO_SUCH_FILE")
	}
	if typ, payload := c.request(19, sftpString("/home/comlink")); c.status(typ, payload) != 2 {
		t.Fatal("READLINK is not NO_SUCH_FILE")
	}
	if typ, payload := c.request(200, sftpString("hardlink@openssh.com")); c.status(typ, payload) != 8 {
		t.Fatal("unknown extension is not OP_UNSUPPORTED")
	}
	// What SFTP wrote, the bridge reads; a large upload keeps its size only.
	typ, payload = c.request(3, sftpString("/home/comlink/.claude/remote/srv/"+claudeHelper+"/server.part"), binary.BigEndian.AppendUint32(nil, 2|8|16), binary.BigEndian.AppendUint32(nil, 0))
	if typ != 102 {
		t.Fatalf("OPEN upload: type %d", typ)
	}
	handle = payload
	chunk := strings.Repeat("x", 200000)
	for i := range 6 {
		if typ, payload := c.request(6, handle, binary.BigEndian.AppendUint64(nil, uint64(i*len(chunk))), sftpString(chunk)); c.status(typ, payload) != 0 {
			t.Fatal("upload WRITE failed")
		}
	}
	c.request(4, handle)
	// SETSTAT with permissions makes the upload executable, as the app does.
	if typ, payload := c.request(9, sftpString("/home/comlink/.claude/remote/srv/"+claudeHelper+"/server.part"), binary.BigEndian.AppendUint32(nil, 4), binary.BigEndian.AppendUint32(nil, 0o755)); c.status(typ, payload) != 0 {
		t.Fatal("SETSTAT failed")
	}
	if typ, payload := c.request(18, sftpString("/home/comlink/.claude/remote/srv/"+claudeHelper+"/server.part"), sftpString("/home/comlink/.claude/remote/srv/"+claudeHelper+"/server")); c.status(typ, payload) != 0 {
		t.Fatal("RENAME failed")
	}
	b := openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	var stat struct {
		Exists bool   `json:"exists"`
		IsDir  bool   `json:"isDir"`
		Size   int    `json:"size"`
		Mode   string `json:"mode"`
	}
	_ = json.Unmarshal(b.call("files.stat", map[string]any{"path": "/home/comlink/.claude/remote/srv/" + claudeHelper + "/server"}), &stat)
	if !stat.Exists || stat.IsDir || stat.Size != 1200000 || stat.Mode != "-rwxr-xr-x" {
		t.Fatalf("files.stat of the upload: %+v", stat)
	}
	var read struct {
		Content string `json:"content"`
		Exists  bool   `json:"exists"`
	}
	_ = json.Unmarshal(b.call("files.read", map[string]any{"path": "/home/comlink/.claude/remote/plugins/02b2e3e9/abc/.synced", "maxBytes": 1048576}), &read)
	if read.Exists || read.Content != "" {
		t.Fatalf("files.read of a missing file: %+v", read)
	}
	var list struct {
		Entries []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			IsDir bool   `json:"isDir"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(b.call("files.list", map[string]any{"path": "/home/comlink"}), &list)
	if len(list.Entries) != 0 {
		t.Fatalf("files.list of the home hides dot names: %+v", list.Entries)
	}
	_ = json.Unmarshal(b.call("files.list", map[string]any{"path": "/home/comlink", "includeDotNames": true}), &list)
	if len(list.Entries) != 1 || list.Entries[0].Name != ".claude" || !list.Entries[0].IsDir || list.Entries[0].Path != "/home/comlink/.claude" {
		t.Fatalf("files.list with dot names: %+v", list.Entries)
	}
	if msg := b.callErr("files.list", map[string]any{"path": "/nowhere"}); msg != "not a directory" {
		t.Fatalf("files.list of a missing directory: %q", msg)
	}
	var git struct {
		IsRepo bool `json:"isRepo"`
	}
	_ = json.Unmarshal(b.call("git.info", map[string]any{"path": "/home/comlink"}), &git)
	if git.IsRepo {
		t.Fatal("git.info claims a repository")
	}
	var prune struct {
		Root string `json:"root"`
		Kept int    `json:"kept"`
	}
	_ = json.Unmarshal(b.call("plugins.prune", map[string]any{"keep": []string{"abc"}, "minAgeDays": 30}), &prune)
	if prune.Kept != 1 || prune.Root == "" {
		t.Fatalf("plugins.prune: %+v", prune)
	}
	// A plugin arrives as a gzip-compressed tar over SFTP and is unpacked by
	// files.extract_tar.
	var tarBuf bytes.Buffer
	gz := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		mode int64
		body string
	}{{"plugin.json", 0o644, `{"name":"example"}`}, {"bin/run", 0o755, "#!/bin/sh\n"}, {".synced", 0o644, "x"}} {
		_ = tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(e.body))
	}
	_ = tw.Close()
	_ = gz.Close()
	archive := "/home/comlink/.claude/remote/plugins/02b2e3e9/abc.tar.gz"
	typ, payload = c.request(3, sftpString(archive), binary.BigEndian.AppendUint32(nil, 2|8|16), binary.BigEndian.AppendUint32(nil, 0))
	if typ != 102 {
		t.Fatalf("OPEN archive: type %d", typ)
	}
	handle = payload
	if typ, payload := c.request(6, handle, binary.BigEndian.AppendUint64(nil, 0), sftpString(tarBuf.String())); c.status(typ, payload) != 0 {
		t.Fatal("WRITE archive failed")
	}
	c.request(4, handle)
	var extracted struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(b.call("files.extract_tar", map[string]any{"archivePath": archive, "destDir": "/home/comlink/.claude/remote/plugins/02b2e3e9/abc"}), &extracted)
	if !extracted.Success {
		t.Fatalf("extract_tar: %+v", extracted)
	}
	_ = json.Unmarshal(b.call("files.read", map[string]any{"path": "/home/comlink/.claude/remote/plugins/02b2e3e9/abc/plugin.json"}), &read)
	if !read.Exists || read.Content != `{"name":"example"}` {
		t.Fatalf("extracted file: %+v", read)
	}
	_ = json.Unmarshal(b.call("files.stat", map[string]any{"path": "/home/comlink/.claude/remote/plugins/02b2e3e9/abc/bin/run"}), &stat)
	if !stat.Exists || stat.Mode != "-rwx------" {
		t.Fatalf("extracted executable: %+v", stat)
	}
	_ = json.Unmarshal(b.call("files.stat", map[string]any{"path": "/home/comlink/.claude/remote/plugins/02b2e3e9/abc/.synced"}), &stat)
	if !stat.Exists || stat.Size != 0 {
		t.Fatalf(".synced marker (the archive's own copy is skipped): %+v", stat)
	}
	_ = json.Unmarshal(b.call("files.extract_tar", map[string]any{"archivePath": archive + ".missing", "destDir": "/home/comlink/x"}), &extracted)
	if extracted.Success || !strings.Contains(extracted.Error, "archive not found") {
		t.Fatalf("extract_tar of a missing archive: %+v", extracted)
	}
	// A plain tar is unpacked as well.
	var plain bytes.Buffer
	ptw := tar.NewWriter(&plain)
	_ = ptw.WriteHeader(&tar.Header{Name: "note.txt", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = ptw.Write([]byte("hi"))
	_ = ptw.Close()
	typ, payload = c.request(3, sftpString(archive+".plain"), binary.BigEndian.AppendUint32(nil, 2|8|16), binary.BigEndian.AppendUint32(nil, 0))
	if typ != 102 {
		t.Fatalf("OPEN plain archive: type %d", typ)
	}
	handle = payload
	c.request(6, handle, binary.BigEndian.AppendUint64(nil, 0), sftpString(plain.String()))
	c.request(4, handle)
	_ = json.Unmarshal(b.call("files.extract_tar", map[string]any{"archivePath": archive + ".plain", "destDir": "/home/comlink/.claude/remote/plugins/02b2e3e9/def"}), &extracted)
	_ = json.Unmarshal(b.call("files.read", map[string]any{"path": "/home/comlink/.claude/remote/plugins/02b2e3e9/def/note.txt"}), &read)
	if !extracted.Success || read.Content != "hi" {
		t.Fatalf("plain tar: %+v %+v", extracted, read)
	}
	if msg := b.callErr("files.extract_tar", map[string]any{"archivePath": archive}); msg != "archivePath and destDir are required" {
		t.Fatalf("extract_tar without destDir: %q", msg)
	}
	if msg := b.callErr("git.worktree_create", map[string]any{}); !strings.HasPrefix(msg, "method not found") {
		t.Fatalf("unknown method: %q", msg)
	}
}

// claudeSpawn is the app's process.spawn of Claude Code with the flags it
// passes, for session id.
func claudeSpawn(id, sessionID string) map[string]any {
	return map[string]any{"id": id, "command": claudeCLI, "args": []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--effort=medium", "--model=claude-example-1", "--permission-prompt-tool=stdio", "--session-id=" + sessionID, "--setting-sources=user,project,local", "--permission-mode=default", "--include-partial-messages", "--replay-user-messages"}, "cwd": "/home/comlink/project", "env": map[string]string{"ANTHROPIC_AUTH_TOKEN": "app-token-never-used", "HOME": "/home/comlink"}}
}

func TestClaudeChatIsAnACPSession(t *testing.T) {
	up := newAgentUpstream(t)
	up.scenario = func(s *acpConvo, prompt string) {
		if prompt == "again" {
			// The second turn only ends when the app cancels it.
			<-s.cancelled
			return
		}
		s.update(map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "thinking about " + prompt}})
		s.text("Hello, ")
		s.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call-1", "title": "Run a command", "kind": "execute", "status": "pending", "rawInput": map[string]any{"command": "echo hi"}})
		if got := s.askPermission("Run a command", "allow_once", "allow_always", "reject_once"); got != "allow_once" {
			s.t.Errorf("permission chosen: %q, want allow_once", got)
		}
		s.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "status": "completed", "content": []map[string]any{{"type": "content", "content": map[string]any{"type": "text", "text": "hi\n"}}}})
		s.text("world")
	}
	client, logBuf := startClaude(t, &claude.Route{Upstream: up.wsURL(), Credential: upstream.Credential{Token: testToken}, Version: "test"})
	b := openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	var spawned struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal(b.call("process.spawn", claudeSpawn("proc-1", "11111111-2222-4333-8444-555555555555")), &spawned)
	if !spawned.Success {
		t.Fatal("process.spawn did not succeed")
	}
	cli := &cliReader{b: b, id: "proc-1"}
	// The app's startup control requests are answered in order.
	applied := b.stdin_("proc-1", `{"request_id":"r1","type":"control_request","request":{"subtype":"initialize","hooks":{}}}`+"\n"+`{"request_id":"r2","type":"control_request","request":{"subtype":"set_permission_mode","mode":"acceptEdits"}}`+"\n")
	if applied == 0 {
		t.Fatal("process.stdin applied nothing")
	}
	init := cli.expect("control_response", "")
	resp := init["response"].(map[string]any)
	if resp["request_id"] != "r1" || resp["subtype"] != "success" {
		t.Fatalf("initialize response %v", init)
	}
	if inner := resp["response"].(map[string]any); inner["claude_code_version"] == nil || inner["current_permission_mode"] != "default" {
		t.Fatalf("initialize response body %v", inner)
	}
	mode := cli.expect("control_response", "")
	if mode["response"].(map[string]any)["response"].(map[string]any)["mode"] != "acceptEdits" {
		t.Fatalf("set_permission_mode response %v", mode)
	}
	if status := cli.expect("system", "status"); status["permissionMode"] != "acceptEdits" {
		t.Fatalf("status after set_permission_mode %v", status)
	}
	// Every other control request the app sends is answered with success
	// and its request id.
	var lines []string
	for i, subtype := range []string{"apply_flag_settings", "list_permission_rules", "get_usage", "mcp_status", "get_context_usage", "ui_panes", "ui_attach", "ui_render", "ui_detach", "rename_session", "set_model"} {
		lines = append(lines, fmt.Sprintf(`{"request_id":"c%d","type":"control_request","request":{"subtype":%q,"props":{"word":"Working"}}}`, i, subtype))
	}
	b.stdin_("proc-1", strings.Join(lines, "\n")+"\n")
	for i := range lines {
		m := cli.expect("control_response", "")
		r := m["response"].(map[string]any)
		if r["request_id"] != fmt.Sprintf("c%d", i) || r["subtype"] != "success" {
			t.Fatalf("control response %d: %v", i, m)
		}
		// The app reads parentSettingsIncluded as "the policy I sent is in force".
		if i == 1 {
			if state := r["response"].(map[string]any)["state"].(map[string]any); state["parentSettingsIncluded"] != true {
				t.Fatalf("list_permission_rules state %v", state)
			}
		}
	}
	// A user message runs a prompt.
	b.stdin_("proc-1", `{"type":"user","uuid":"u-1","session_id":"11111111-2222-4333-8444-555555555555","message":{"role":"user","content":"say hello"}}`+"\n")
	if m := cli.expect("system", "running"); m["sdk_host_only"] != true {
		t.Fatalf("running %v", m)
	}
	if m := cli.expect("command_lifecycle", "queued"); m["command_uuid"] != "u-1" {
		t.Fatalf("queued %v", m)
	}
	cli.expect("command_lifecycle", "started")
	if m := cli.expect("system", "init"); m["cwd"] != "/home/comlink/project" || m["session_id"] != "11111111-2222-4333-8444-555555555555" || m["model"] != "claude-example-1" {
		t.Fatalf("init %v", m)
	}
	if m := cli.expect("user", ""); m["isReplay"] != true || m["uuid"] != "u-1" {
		t.Fatalf("user echo %v", m)
	}
	start := cli.expect("stream_event", "message_start")
	if start["user_message_uuid"] != "u-1" {
		t.Fatalf("message_start %v", start)
	}
	if block := cli.expect("stream_event", "content_block_start"); block["event"].(map[string]any)["content_block"].(map[string]any)["type"] != "thinking" {
		t.Fatalf("first block %v", block)
	}
	if delta := cli.expect("stream_event", "content_block_delta"); delta["event"].(map[string]any)["delta"].(map[string]any)["thinking"] != "thinking about say hello" {
		t.Fatalf("thinking delta %v", delta)
	}
	if delta := cli.expect("stream_event", "content_block_delta"); delta["event"].(map[string]any)["delta"].(map[string]any)["text"] != "Hello, " {
		t.Fatalf("text delta %v", delta)
	}
	// The tool call is a tool_use block that ends the message.
	if block := cli.expect("stream_event", "content_block_start"); block["event"].(map[string]any)["content_block"].(map[string]any)["name"] != "Run a command" {
		t.Fatalf("tool_use block %v", block)
	}
	first := cli.expect("assistant", "")
	content := first["message"].(map[string]any)["content"].([]any)
	if len(content) != 3 || content[0].(map[string]any)["type"] != "thinking" || content[1].(map[string]any)["text"] != "Hello, " || content[2].(map[string]any)["id"] != "call-1" {
		t.Fatalf("first assistant message content %v", content)
	}
	if delta := cli.expect("stream_event", "message_delta"); delta["event"].(map[string]any)["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("message_delta %v", delta)
	}
	// The permission request becomes the app's can_use_tool prompt; the
	// app's allow picks the upstream's allow_once option.
	perm := cli.expect("control_request", "")
	req := perm["request"].(map[string]any)
	if req["subtype"] != "can_use_tool" || req["tool_name"] != "Run a command" || req["tool_use_id"] != "call-1" || req["input"].(map[string]any)["command"] != "echo hi" {
		t.Fatalf("can_use_tool %v", req)
	}
	b.stdin_("proc-1", `{"type":"control_response","response":{"subtype":"success","request_id":"`+perm["request_id"].(string)+`","response":{"behavior":"allow","updatedInput":{"command":"echo hi"}}}}`+"\n")
	if m := cli.expect("user", ""); m["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] != "hi\n" {
		t.Fatalf("tool_result %v", m)
	}
	cli.expect("stream_event", "message_start")
	if delta := cli.expect("stream_event", "content_block_delta"); delta["event"].(map[string]any)["delta"].(map[string]any)["text"] != "world" {
		t.Fatalf("second text delta %v", delta)
	}
	last := cli.expect("assistant", "")
	if content := last["message"].(map[string]any)["content"].([]any); len(content) != 1 || content[0].(map[string]any)["text"] != "world" {
		t.Fatalf("last assistant message content %v", content)
	}
	if delta := cli.expect("stream_event", "message_delta"); delta["event"].(map[string]any)["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("final message_delta %v", delta)
	}
	cli.expect("stream_event", "message_stop")
	result := cli.expect("result", "")
	if result["subtype"] != "success" || result["is_error"] != false || result["result"] != "Hello, world" || result["num_turns"] != float64(1) {
		t.Fatalf("result %v", result)
	}
	if m := cli.expect("command_lifecycle", "completed"); m["command_uuid"] != "u-1" {
		t.Fatalf("completed %v", m)
	}
	cli.expect("system", "idle")
	// A second message reuses the session; an interrupt cancels upstream.
	b.stdin_("proc-1", `{"type":"user","uuid":"u-2","message":{"role":"user","content":[{"type":"text","text":"again"}]}}`+"\n")
	b.stdin_("proc-1", `{"request_id":"r3","type":"control_request","request":{"subtype":"interrupt"}}`+"\n")
	if m := cli.expect("result", ""); m["result"] != "" || m["subtype"] != "success" {
		t.Fatalf("second result %v", m)
	}
	cli.expect("system", "idle")
	up.mu.Lock()
	prompts, inits, auths, cancels := up.prompts, up.inits, up.auths, up.cancels
	up.mu.Unlock()
	if len(prompts) != 2 || prompts[0] != "say hello" || prompts[1] != "again" {
		t.Fatalf("upstream prompts %q", prompts)
	}
	if len(inits) != 1 || !strings.Contains(string(inits[0]), `"clientInfo":{"name":"comlink","version":"test"}`) {
		t.Fatalf("upstream initialize %s", inits)
	}
	if len(auths) != 1 || auths[0] != "Bearer "+testToken {
		t.Fatalf("upstream auths %q", auths)
	}
	if cancels != 1 {
		t.Fatalf("upstream cancels %d, want 1", cancels)
	}
	// Records are numbered without gaps, as the app's reattach expects.
	for i, seq := range cli.seqs {
		if i > 0 && seq != cli.seqs[i-1]+1 {
			t.Fatalf("stdout seqs %v", cli.seqs)
		}
	}
	// A second bridge (the app reconnected) reattaches and gets the records
	// again from the sequence it asks for.
	b2 := openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	var re struct {
		Found        bool `json:"found"`
		Running      bool `json:"running"`
		FirstSeq     int  `json:"firstSeq"`
		LastSeq      int  `json:"lastSeq"`
		StdinApplied int  `json:"stdinApplied"`
	}
	_ = json.Unmarshal(b2.call("process.reattach", map[string]any{"id": "proc-1", "fromSeq": cli.seqs[len(cli.seqs)-1]}), &re)
	if !re.Found || !re.Running || re.FirstSeq != 1 || re.LastSeq != cli.seqs[len(cli.seqs)-1] || re.StdinApplied == 0 {
		t.Fatalf("reattach %+v", re)
	}
	if rec := b2.record("proc-1", "stdout"); rec.Seq != cli.seqs[len(cli.seqs)-1] {
		t.Fatalf("replayed seq %d, want %d", rec.Seq, cli.seqs[len(cli.seqs)-1])
	}
	_ = json.Unmarshal(b2.call("process.reattach", map[string]any{"id": "proc-9", "fromSeq": 1}), &re)
	if re.Found {
		t.Fatal("reattach of an unknown process found it")
	}
	// Kill ends the process with an exit record on the attached bridge.
	b2.call("process.killAndWait", map[string]any{"id": "proc-1", "signal": "SIGTERM"})
	if rec := b2.record("proc-1", "exit"); rec.ExitCode != 143 {
		t.Fatalf("exit record %+v", rec)
	}
	if strings.Contains(logBuf.String(), testToken) {
		t.Fatal("log has the token")
	}
}

func TestClaudeChatReportsUpstreamFailures(t *testing.T) {
	client, logBuf := startClaude(t, &claude.Route{Upstream: "ws://127.0.0.1:1/acp", Credential: upstream.Credential{Token: testToken}})
	b := openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	b.call("process.spawn", claudeSpawn("proc-1", "11111111-2222-4333-8444-555555555555"))
	b.stdin_("proc-1", `{"type":"user","uuid":"u-1","message":{"role":"user","content":"hello"}}`+"\n")
	cli := &cliReader{b: b, id: "proc-1"}
	result := cli.expect("result", "")
	if result["subtype"] != "error_during_execution" || result["is_error"] != true || !strings.Contains(result["result"].(string), "dial ws://127.0.0.1:1/acp") {
		t.Fatalf("result %v", result)
	}
	cli.expect("system", "idle")
	if !strings.Contains(logBuf.String(), "dial ws://127.0.0.1:1/acp") {
		t.Fatalf("log %q", logBuf.String())
	}
	// A token command that fails is reported the same way, without its output.
	client, _ = startClaude(t, &claude.Route{Upstream: "ws://127.0.0.1:1/acp", Credential: upstream.Credential{Command: "echo " + testToken + "; exit 3"}})
	b = openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	b.call("process.spawn", claudeSpawn("proc-2", "11111111-2222-4333-8444-555555555555"))
	b.stdin_("proc-2", `{"type":"user","uuid":"u-1","message":{"role":"user","content":"hello"}}`+"\n")
	cli = &cliReader{b: b, id: "proc-2"}
	result = cli.expect("result", "")
	if result["is_error"] != true || !strings.Contains(result["result"].(string), "token command failed") || strings.Contains(result["result"].(string), testToken) {
		t.Fatalf("result %v", result)
	}
}

func TestClaudeAnswersShellSpawns(t *testing.T) {
	client, _ := startClaude(t, &claude.Route{Upstream: unusedUpstream})
	b := openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	transcript := "/home/comlink/.claude/projects/-home-comlink/11111111-2222-4333-8444-555555555555.jsonl"
	cases := []struct {
		name   string
		args   []string
		stdout string
		exit   int
	}{
		{"transcript size", []string{"-c", `test -s '` + transcript + `'; echo "__claude_sh${__claude_unset:-_ran}:$?"`}, "__claude_sh_ran:1\n", 0},
		{"subagent listing", []string{"-c", `ls -1 '/home/comlink/.claude/projects/-home-comlink'/'1111'/subagents/agent-*.jsonl 2>/dev/null; s=$?; (exit $s); echo "__claude_sh${__claude_unset:-_ran}:$?"`}, "__claude_sh_ran:1\n", 0},
		{"transcript read", []string{"-c", `LC_ALL=C sh -c 'wc -c < "$1" && head -c 65536 "$1"' sh '` + transcript + `'`}, "", 1},
		{"head of an empty file", []string{"-c", `head -c 3 '/home/comlink/.claude/remote/srv/` + claudeHelper + `/server' && echo ok`}, "ok\n", 0},
		{"transcript glob loop", []string{"-c", `for f in "${CLAUDE_CONFIG_DIR:-$HOME/.claude}"/projects/*/'1111'.jsonl; do [ -f "$f" ] && printf "%s\n" "$f"; done`}, "", 1},
		{"subagent origin loop", []string{"-c", `i=0; for f in '/home/comlink/.claude/projects/x/a.jsonl' '/home/comlink/.claude/projects/x/b.jsonl'; do printf '__ORIGIN__%s:' "$i"; [ -f "$f" ] && LC_ALL=C head -c 4096 "$f" 2>/dev/null | LC_ALL=C tr -d '\r\n'; echo; i=$((i+1)); done`}, "__ORIGIN__0:\n__ORIGIN__1:\n", 0},
		{"pid probe loop", []string{"-c", `for p in 4242 4243; do if [ -r /proc/$p/stat ]; then s=$(cat /proc/$p/stat 2>/dev/null); echo "$p $s EOR"; else set -- $(ps -o pgid= -p $p 2>/dev/null); [ -n "$1" ] && echo "$p $1 EOR"; fi; done`}, "", 1},
		{"if then else", []string{"-c", `if test -d /home/comlink; then echo yes; else echo no; fi; if test -d /nowhere; then echo yes; else echo no; fi`}, "yes\nno\n", 0},
		{"workflow find", []string{"-c", `LC_ALL=C QUOTING_STYLE=literal find '/home/comlink/.claude/projects/x/subagents/workflows' -type f -name 'agent-*.jsonl' -exec ls -ln {} + 2>/dev/null`}, "", 1},
	}
	for i, tc := range cases {
		id := fmt.Sprintf("sh-%d", i)
		b.call("process.spawn", map[string]any{"id": id, "command": "sh", "args": tc.args})
		var out []byte
		for {
			rec := b.record(id, "")
			if rec.Stream == "exit" {
				if rec.ExitCode != tc.exit {
					t.Errorf("%s: exit %d, want %d", tc.name, rec.ExitCode, tc.exit)
				}
				break
			}
			if rec.Stream == "stdout" {
				data, _ := base64.StdEncoding.DecodeString(rec.Data)
				out = append(out, data...)
			}
		}
		if string(out) != tc.stdout {
			t.Errorf("%s: stdout %q, want %q", tc.name, out, tc.stdout)
		}
	}
	// The loops produced no stderr: the records read so far hold stdout and
	// exits only.
	for _, rec := range b.records {
		if rec.Stream == "stderr" {
			t.Fatalf("unexpected stderr record %+v", rec)
		}
	}
	b.call("process.spawn", map[string]any{"id": "other", "command": "/usr/bin/python3", "args": []string{"-c", "1"}})
	var stderr []byte
	for {
		rec := b.record("other", "")
		if rec.Stream == "exit" {
			if rec.ExitCode != 127 {
				t.Fatalf("other command exit %d", rec.ExitCode)
			}
			break
		}
		data, _ := base64.StdEncoding.DecodeString(rec.Data)
		stderr = append(stderr, data...)
	}
	if string(stderr) != "comlink: unsupported command: /usr/bin/python3\n" {
		t.Fatalf("other command stderr %q", stderr)
	}
	if msg := b.callErr("process.stdin", map[string]any{"id": "nope", "data": ""}); msg != "no such process" {
		t.Fatalf("stdin to an unknown process: %q", msg)
	}
	if msg := b.callErr("process.spawn", map[string]any{"command": "sh"}); msg != "missing process id" {
		t.Fatalf("spawn without id: %q", msg)
	}
	if msg := b.callErr("files.read", json.RawMessage(`"x"`)); msg != "invalid params" {
		t.Fatalf("bad params: %q", msg)
	}
}

// chatTurn spawns a Claude Code stand-in with args on a fresh connection of
// route, sends one user message and returns the turn's result text.
func chatTurn(t *testing.T, route *claude.Route, sessionArg, text string) string {
	t.Helper()
	client, _ := startClaude(t, route)
	b := openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	spawn := claudeSpawn("proc-1", "unused")
	args := spawn["args"].([]string)
	for i, a := range args {
		if strings.HasPrefix(a, "--session-id=") {
			args[i] = sessionArg
		}
	}
	b.call("process.spawn", spawn)
	cli := &cliReader{b: b, id: "proc-1"}
	b.stdin_("proc-1", `{"type":"user","uuid":"u-1","message":{"role":"user","content":`+strconv.Quote(text)+`}}`+"\n")
	result := cli.expect("result", "")
	if result["is_error"] != false {
		t.Fatalf("turn failed: %v", result)
	}
	return result["result"].(string)
}

func TestClaudeChatResumesItsAgentSession(t *testing.T) {
	const chat = "11111111-2222-4333-8444-555555555555"
	newUpstream := func() *agentUpstream {
		up := newAgentUpstream(t)
		up.scenario = func(s *acpConvo, prompt string) { s.text("reply to " + prompt) }
		return up
	}
	route := func(up *agentUpstream, dir string) *claude.Route {
		return &claude.Route{Upstream: up.wsURL(), Credential: upstream.Credential{Token: testToken}, Version: "test", Chats: dir}
	}

	t.Run("a resumed chat loads the agent session it used", func(t *testing.T) {
		up, dir := newUpstream(), t.TempDir()
		up.loadSession = true
		if got := chatTurn(t, route(up, dir), "--session-id="+chat, "first"); got != "reply to first" {
			t.Fatalf("first turn %q", got)
		}
		// A new connection and a new comlink process: only the directory remains.
		if got := chatTurn(t, route(up, dir), "--resume="+chat, "second"); got != "reply to second" {
			t.Fatalf("resumed turn %q (replayed history must not be shown again)", got)
		}
		// Another chat gets its own agent session.
		chatTurn(t, route(up, dir), "--session-id=22222222-2222-4333-8444-555555555555", "other")
		up.mu.Lock()
		defer up.mu.Unlock()
		if up.news != 2 || strings.Join(up.loads, ",") != "sess-1" {
			t.Fatalf("session/new %d times, session/load %v; want 2 and [sess-1]", up.news, up.loads)
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if info, _ := f.Info(); info.Mode().Perm() != 0o600 {
				t.Errorf("chat file %s mode %v, want 0600", f.Name(), info.Mode().Perm())
			}
		}
	})

	t.Run("an agent that cannot load sessions gets a new one", func(t *testing.T) {
		up, dir := newUpstream(), t.TempDir()
		chatTurn(t, route(up, dir), "--session-id="+chat, "first")
		chatTurn(t, route(up, dir), "--resume="+chat, "second")
		up.mu.Lock()
		defer up.mu.Unlock()
		if up.news != 2 || len(up.loads) != 0 {
			t.Fatalf("session/new %d times, session/load %v; want 2 and none", up.news, up.loads)
		}
	})

	t.Run("a session the agent no longer has is replaced", func(t *testing.T) {
		up, dir := newUpstream(), t.TempDir()
		up.loadSession = true
		chatTurn(t, route(up, dir), "--session-id="+chat, "first")
		up.mu.Lock()
		up.loadFails = true
		up.mu.Unlock()
		if got := chatTurn(t, route(up, dir), "--resume="+chat, "second"); got != "reply to second" {
			t.Fatalf("turn after a failed load %q", got)
		}
		up.mu.Lock()
		up.loadFails = false
		up.mu.Unlock()
		// The replacement is what the chat resumes next.
		chatTurn(t, route(up, dir), "--resume="+chat, "third")
		up.mu.Lock()
		defer up.mu.Unlock()
		if up.news != 2 || strings.Join(up.loads, ",") != "sess-1,sess-2" {
			t.Fatalf("session/new %d times, session/load %v; want 2 and [sess-1 sess-2]", up.news, up.loads)
		}
	})

	t.Run("without a chats directory every connection starts anew", func(t *testing.T) {
		up := newUpstream()
		up.loadSession = true
		chatTurn(t, route(up, ""), "--session-id="+chat, "first")
		chatTurn(t, route(up, ""), "--resume="+chat, "second")
		up.mu.Lock()
		defer up.mu.Unlock()
		if up.news != 2 || len(up.loads) != 0 {
			t.Fatalf("session/new %d times, session/load %v; want 2 and none", up.news, up.loads)
		}
	})
}

// shRun runs `sh -c script` through the bridge and returns its stdout and
// exit code.
func shRun(t *testing.T, b *bridge, id, script string) (string, int) {
	t.Helper()
	b.call("process.spawn", map[string]any{"id": id, "command": "sh", "args": []string{"-c", script}})
	var out []byte
	for {
		rec := b.record(id, "")
		if rec.Stream == "exit" {
			return string(out), rec.ExitCode
		}
		if rec.Stream == "stdout" {
			data, _ := base64.StdEncoding.DecodeString(rec.Data)
			out = append(out, data...)
		}
	}
}

func TestClaudeChatTranscriptSurvivesTheProcess(t *testing.T) {
	const chat = "11111111-2222-4333-8444-555555555555"
	const path = "/home/comlink/.claude/projects/-home-comlink-project/" + chat + ".jsonl"
	up := newAgentUpstream(t)
	up.loadSession = true
	up.scenario = func(s *acpConvo, prompt string) { s.text("reply to " + prompt) }
	dir := t.TempDir()
	route := func() *claude.Route {
		return &claude.Route{Upstream: up.wsURL(), Credential: upstream.Credential{Token: testToken}, Version: "test", Chats: dir}
	}
	chatTurn(t, route(), "--session-id="+chat, "first")

	// A new comlink process: the app finds the chat's transcript where Claude
	// Code keeps it, with the search the app runs, and reads it.
	client, _ := startClaude(t, route())
	b := openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	found, code := shRun(t, b, "sh-1", `for f in "${CLAUDE_CONFIG_DIR:-$HOME/.claude}"/projects/*/'`+chat+`'.jsonl; do [ -f "$f" ] && printf "%s\n" "$f"; done`)
	if found != path+"\n" || code != 0 {
		t.Fatalf("transcript search printed %q (exit %d), want %q", found, code, path+"\n")
	}
	read, code := shRun(t, b, "sh-2", `LC_ALL=C sh -c 'wc -c < "$1" && head -c 65536 "$1"' sh '`+path+`'`)
	if code != 0 {
		t.Fatalf("transcript read exit %d: %q", code, read)
	}
	type line struct {
		Type       string          `json:"type"`
		UUID       string          `json:"uuid"`
		ParentUUID *string         `json:"parentUuid"`
		SessionID  string          `json:"sessionId"`
		Cwd        string          `json:"cwd"`
		Message    json.RawMessage `json:"message"`
	}
	parse := func(s string) []line {
		var out []line
		for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
			var x line
			if json.Unmarshal([]byte(l), &x) == nil && x.Type != "" {
				out = append(out, x)
			}
		}
		return out
	}
	lines := parse(read)
	if len(lines) != 2 || lines[0].Type != "user" || lines[1].Type != "assistant" || lines[0].ParentUUID != nil || lines[1].ParentUUID == nil || *lines[1].ParentUUID != lines[0].UUID {
		t.Fatalf("transcript lines %+v", lines)
	}
	for _, l := range lines {
		if l.SessionID != chat || l.Cwd != "/home/comlink/project" {
			t.Fatalf("transcript line %+v", l)
		}
	}
	if !strings.Contains(string(lines[0].Message), "first") || !strings.Contains(string(lines[1].Message), "reply to first") {
		t.Fatalf("transcript messages %s / %s", lines[0].Message, lines[1].Message)
	}

	// The resumed chat continues the same transcript after its last line.
	chatTurn(t, route(), "--resume="+chat, "second")
	files, _ := os.ReadDir(dir)
	var record []byte
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".jsonl") {
			record, _ = os.ReadFile(dir + "/" + f.Name())
		}
	}
	all := parse(string(record))
	if len(all) != 4 || *all[2].ParentUUID != all[1].UUID || !strings.Contains(string(all[3].Message), "reply to second") {
		t.Fatalf("transcript after the resumed turn %+v", all)
	}

	// Another upstream's transcripts are not shown.
	other := &claude.Route{Upstream: "ws://127.0.0.1:1/acp", Chats: dir}
	client, _ = startClaude(t, other)
	b = openBridge(t, client, readFixture(t, "claude-app-bridge.txt"), "")
	if out, code := shRun(t, b, "sh-3", `for f in "$HOME"/.claude/projects/*/'`+chat+`'.jsonl; do [ -f "$f" ] && printf "%s\n" "$f"; done`); out != "" || code != 1 {
		t.Fatalf("other upstream found %q (exit %d)", out, code)
	}
}
