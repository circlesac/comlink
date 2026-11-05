package claude

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/upstream"
)

// The helper's bridge is newline-delimited JSON-RPC 2.0 on the exec
// channel. The app sends requests; the bridge answers them and, between
// answers, streams the output of the processes it spawned as records
// `{"type":"stream","processId":...,"stream":"stdout"|"stderr"|"exit",...}`.
// Every line carries `"end":{"at":<offset of the line>,"len":<length of the
// line without end and its closing brace>}` so the app can resume a stream
// it lost. The app authenticates each request with the token it wrote over
// SFTP; the transport is the user's own ssh, so comlink does not check it.

// methods is what the bridge serves, announced in server.capabilities so the
// app asks for nothing else.
var methods = []string{"server.ping", "server.capabilities", "server.shutdown", "files.list", "files.stat", "files.read", "files.realpath", "files.extract_tar", "git.info", "process.spawn", "process.stdin", "process.kill", "process.killAndWait", "process.reattach", "process.resize", "plugins.prune"}

var features = []string{"process.stdin.offset", "server.line_end", "server.instance_id", "files.list.includeDotNames", "files.extract_tar.execBit"}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// record is one stream line of a process: output, or its exit.
type record struct {
	seq      int
	stream   string
	data     []byte
	exitCode int
}

// recordCap is how much output a process keeps for a reattach.
const recordCap = 4 << 20

// proc is a process the app spawned: a virtual Claude Code, or a shell
// command answered at once. Its output is kept up to recordCap bytes for
// the app to reattach after a lost bridge.
type proc struct {
	id      string
	mu      sync.Mutex
	records []record
	kept    int
	seq     int
	running bool
	exit    int
	sink    func(record) // the attached bridge, nil while detached
	stdin   *io.PipeWriter
	applied int64
	done    chan struct{}
	stop    func() // ends the process
}

// emit appends one record and hands it to the attached bridge.
func (p *proc) emit(stream string, data []byte, exitCode int) {
	p.mu.Lock()
	p.seq++
	rec := record{seq: p.seq, stream: stream, data: data, exitCode: exitCode}
	p.records = append(p.records, rec)
	p.kept += len(data)
	for p.kept > recordCap && len(p.records) > 1 {
		p.kept -= len(p.records[0].data)
		p.records = p.records[1:]
	}
	sink := p.sink
	p.mu.Unlock()
	if sink != nil {
		sink(rec)
	}
}

// writer returns an io.Writer that emits to stream.
func (p *proc) writer(stream string) io.Writer {
	return writerFunc(func(b []byte) (int, error) {
		p.emit(stream, append([]byte(nil), b...), 0)
		return len(b), nil
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// exited records the process's end.
func (p *proc) exited(code int) {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	p.running = false
	p.exit = code
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	p.mu.Unlock()
	p.emit("exit", nil, code)
	close(p.done)
}

// procs is the table of a route's processes.
type procs struct {
	route *Route
	mu    sync.Mutex
	m     map[string]*proc
}

func newProcs(r *Route) *procs { return &procs{route: r, m: map[string]*proc{}} }

func (ps *procs) get(id string) *proc {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.m[id]
}

// bridge serves one --bridge channel.
type bridge struct {
	route *Route
	ch    ssh.Channel
	wmu   sync.Mutex
	at    int64 // bytes written on the channel so far
}

// serveBridge serves the helper's RPC on ch until the app closes it.
func (r *Route) serveBridge(ch ssh.Channel) int {
	b := &bridge{route: r, ch: ch}
	defer b.detachAll()
	br := bufio.NewReaderSize(ch, 1<<16)
	for {
		line, err := readLine(br, upstream.ReadLimit)
		if err != nil {
			return 0
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil || req.Method == "" {
			if err := b.reply(req.ID, nil, &rpcError{-32600, "invalid request"}); err != nil {
				return 1
			}
			continue
		}
		result, rpcErr := b.handle(&req)
		if err := b.reply(req.ID, result, rpcErr); err != nil {
			return 1
		}
		if req.Method == "process.reattach" {
			b.replay(&req)
		}
	}
}

// readLine reads one line of at most limit bytes.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > limit {
			return nil, errors.New("line too long")
		}
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > 0 && errors.Is(err, io.EOF) {
				return line, nil
			}
			return nil, err
		}
	}
}

// writeLine writes v as one line with its end marker.
func (b *bridge) writeLine(v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	body = body[:len(body)-1] // the closing brace, reopened for end
	b.wmu.Lock()
	defer b.wmu.Unlock()
	line := fmt.Sprintf(`%s,"end":{"at":%d,"len":%d}}`+"\n", body, b.at, len(body))
	_, err := io.WriteString(b.ch, line)
	b.at += int64(len(line))
	return err
}

func (b *bridge) reply(id json.RawMessage, result any, rpcErr *rpcError) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if rpcErr != nil {
		return b.writeLine(map[string]any{"jsonrpc": "2.0", "id": id, "error": rpcErr})
	}
	if result == nil {
		result = map[string]any{}
	}
	return b.writeLine(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// stream writes one process record.
func (b *bridge) stream(id string, rec record) {
	line := map[string]any{"type": "stream", "processId": id, "stream": rec.stream, "seq": rec.seq}
	if rec.stream == "exit" {
		line["exitCode"] = rec.exitCode
	} else {
		line["data"] = base64.StdEncoding.EncodeToString(rec.data)
	}
	_ = b.writeLine(line)
}

// attach makes b the sink of p.
func (b *bridge) attach(p *proc) {
	p.mu.Lock()
	p.sink = func(rec record) { b.stream(p.id, rec) }
	p.mu.Unlock()
}

// detachAll detaches every process attached to b: the channel is gone.
func (b *bridge) detachAll() {
	ps := b.route.procs
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.m {
		p.mu.Lock()
		p.sink = nil
		p.mu.Unlock()
	}
}

// replay resends a reattached process's kept records from fromSeq.
func (b *bridge) replay(req *request) {
	var params struct {
		ID      string `json:"id"`
		FromSeq int    `json:"fromSeq"`
	}
	_ = json.Unmarshal(req.Params, &params)
	p := b.route.procs.get(params.ID)
	if p == nil {
		return
	}
	p.mu.Lock()
	recs := append([]record(nil), p.records...)
	p.mu.Unlock()
	for _, rec := range recs {
		if rec.seq >= params.FromSeq {
			b.stream(p.id, rec)
		}
	}
	b.attach(p)
}

func (b *bridge) handle(req *request) (any, *rpcError) {
	r := b.route
	var p struct {
		Path            string            `json:"path"`
		ArchivePath     string            `json:"archivePath"`
		DestDir         string            `json:"destDir"`
		MaxBytes        int               `json:"maxBytes"`
		IncludeDotNames bool              `json:"includeDotNames"`
		ID              string            `json:"id"`
		Command         string            `json:"command"`
		Args            []string          `json:"args"`
		Cwd             string            `json:"cwd"`
		Env             map[string]string `json:"env"`
		Data            string            `json:"data"`
		Offset          *int64            `json:"offset"`
		FromSeq         int               `json:"fromSeq"`
		Keep            []string          `json:"keep"`
		MinAgeDays      int               `json:"minAgeDays"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
	}
	switch req.Method {
	case "server.ping":
		return map[string]any{"pong": true}, nil
	case "server.capabilities":
		return map[string]any{"version": r.helperVersion(), "methods": methods, "instanceId": r.instanceID(), "startedAt": r.startedAt().UnixMilli(), "features": features, "peerCheck": "off"}, nil
	case "server.shutdown":
		return map[string]any{}, nil
	case "files.read":
		n, ok := r.fs.stat(p.Path)
		if !ok || n.dir {
			return map[string]any{"content": "", "exists": false}, nil
		}
		data := n.data
		if p.MaxBytes > 0 && len(data) > p.MaxBytes {
			data = data[:p.MaxBytes]
		}
		return map[string]any{"content": string(data), "exists": true}, nil
	case "files.stat":
		n, ok := r.fs.stat(p.Path)
		if !ok {
			return map[string]any{"exists": false}, nil
		}
		return map[string]any{"exists": true, "isDir": n.dir, "size": n.size, "mode": modeString(n)}, nil
	case "files.list":
		names, ok := r.fs.list(p.Path)
		if !ok {
			return nil, &rpcError{-32000, "not a directory"}
		}
		entries := []map[string]any{}
		for _, name := range names {
			if strings.HasPrefix(name, ".") && !p.IncludeDotNames {
				continue
			}
			n, _ := r.fs.stat(clean(p.Path) + "/" + name)
			entries = append(entries, map[string]any{"name": name, "path": clean(p.Path) + "/" + name, "isDir": n.dir})
		}
		return map[string]any{"entries": entries}, nil
	case "files.realpath":
		return map[string]any{"path": clean(p.Path)}, nil
	case "files.extract_tar":
		// The app uploads a plugin as a tar over SFTP, then has it unpacked
		// into the plugin directory, which it marks with .synced.
		if p.ArchivePath == "" || p.DestDir == "" {
			return nil, &rpcError{-32602, "archivePath and destDir are required"}
		}
		if err := r.fs.extractTar(p.ArchivePath, p.DestDir); err != nil {
			return map[string]any{"success": false, "error": err.Error()}, nil
		}
		r.fs.put(clean(p.DestDir)+"/.synced", nil, 0o600)
		return map[string]any{"success": true}, nil
	case "git.info":
		return map[string]any{"isRepo": false, "repoSlug": "", "defaultBranch": ""}, nil
	case "plugins.prune":
		return map[string]any{"root": r.Home + "/.claude/remote/plugins", "pruned": []string{}, "prunedLegacy": []string{}, "staleArchives": 0, "kept": len(p.Keep), "live": 0, "young": 0, "legacy": "swept", "minAgeDays": p.MinAgeDays}, nil
	case "process.spawn":
		if p.ID == "" {
			return nil, &rpcError{-32602, "missing process id"}
		}
		b.spawn(p.ID, p.Command, p.Args, p.Cwd)
		return map[string]any{"success": true}, nil
	case "process.stdin":
		pr := r.procs.get(p.ID)
		if pr == nil {
			return nil, &rpcError{-32000, "no such process"}
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return nil, &rpcError{-32602, "stdin data is not base64"}
		}
		applied := pr.feed(data, p.Offset)
		return map[string]any{"success": true, "applied": applied}, nil
	case "process.kill", "process.killAndWait":
		pr := r.procs.get(p.ID)
		if pr == nil {
			return nil, &rpcError{-32000, "no such process"}
		}
		pr.stop()
		if req.Method == "process.killAndWait" {
			select {
			case <-pr.done:
			case <-time.After(5 * time.Second):
			}
		}
		return map[string]any{"success": true}, nil
	case "process.resize":
		return map[string]any{"success": true}, nil
	case "process.reattach":
		pr := r.procs.get(p.ID)
		if pr == nil {
			return map[string]any{"found": false, "running": false, "firstSeq": 0, "lastSeq": 0, "stdinApplied": 0}, nil
		}
		pr.mu.Lock()
		defer pr.mu.Unlock()
		first := 0
		if len(pr.records) > 0 {
			first = pr.records[0].seq
		}
		return map[string]any{"found": true, "running": pr.running, "firstSeq": first, "lastSeq": pr.seq, "stdinApplied": pr.applied}, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

// feed appends data to the process's stdin, skipping what offset says was
// applied already, and returns the bytes applied so far.
func (p *proc) feed(data []byte, offset *int64) int64 {
	p.mu.Lock()
	applied, stdin, running := p.applied, p.stdin, p.running
	p.mu.Unlock()
	if offset != nil && *offset < applied {
		skip := min(applied-*offset, int64(len(data)))
		data = data[skip:]
	}
	if len(data) == 0 || !running || stdin == nil {
		return applied
	}
	// The virtual process reads its stdin as it goes; a write only waits for
	// it to catch up.
	n, _ := stdin.Write(data)
	p.mu.Lock()
	p.applied += int64(n)
	applied = p.applied
	p.mu.Unlock()
	return applied
}

// spawn starts the process id: Claude Code when command is the installed
// CLI, else a shell command answered from the virtual tree, whose output
// comes as stream records on this bridge.
func (b *bridge) spawn(id, command string, args []string, cwd string) {
	r := b.route
	pr, pw := io.Pipe()
	p := &proc{id: id, running: true, stdin: pw, done: make(chan struct{})}
	p.stop = func() { p.exited(143) }
	r.procs.mu.Lock()
	if old := r.procs.m[id]; old != nil {
		old.stop()
	}
	r.procs.m[id] = p
	r.procs.mu.Unlock()
	b.attach(p)
	if cwd == "" {
		cwd = r.Home
	}
	if strings.Contains(command, "/.claude/remote/ccd-cli/") {
		c := newCLI(r, p, pr, args, cwd)
		p.stop = c.stop
		go c.run()
		return
	}
	go func() {
		_ = pr.Close()
		sh := &shell{route: r, fs: r.fs, cwd: cwd, out: p.writer("stdout"), errOut: p.writer("stderr")}
		status := 127
		switch {
		case command == "sh" && len(args) >= 2 && args[0] == "-c":
			status = sh.run(args[1])
		default:
			_, _ = io.WriteString(sh.errOut, "comlink: unsupported command: "+command+"\n")
		}
		p.exited(status)
	}()
}

func (r *Route) instanceID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.instance == "" {
		r.instance = randomID(16)
		r.started = time.Now()
	}
	return r.instance
}

func (r *Route) startedAt() time.Time {
	r.instanceID()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}
