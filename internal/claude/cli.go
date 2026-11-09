package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// cli stands in for one Claude Code process the app spawned: it speaks
// stream-json on the process's stdin and stdout and runs each user message
// as an ACP prompt on the upstream.
type cli struct {
	route   *Route
	p       *proc
	stdin   io.Reader
	out     io.Writer
	cwd     string
	session string
	model   string
	mode    string
	version string

	ctx    context.Context
	cancel context.CancelFunc
	turns  chan json.RawMessage
	wmu    sync.Mutex

	mu         sync.Mutex
	pending    map[string]chan json.RawMessage // the app's control_responses by request_id
	acp        *acpConn
	acpSession string
	resume     string // the ACP session this chat last used, loaded again when the upstream is dialled anew
	replaying  bool   // session/load is replaying history the app already shows
	turn       *turn  // the turn in progress, for ACP updates
	parent     string // the last transcript line's uuid
}

func newCLI(r *Route, p *proc, stdin io.Reader, args []string, cwd string) *cli {
	ctx, cancel := context.WithCancel(context.Background())
	c := &cli{route: r, p: p, stdin: stdin, out: p.writer("stdout"), cwd: cwd, mode: "default", model: "default", version: "0.0.0", ctx: ctx, cancel: cancel, turns: make(chan json.RawMessage, 64), pending: map[string]chan json.RawMessage{}}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--session-id="):
			c.session = strings.TrimPrefix(a, "--session-id=")
		case strings.HasPrefix(a, "--resume="):
			c.session = strings.TrimPrefix(a, "--resume=")
		case strings.HasPrefix(a, "--model="):
			c.model = strings.TrimPrefix(a, "--model=")
		case strings.HasPrefix(a, "--permission-mode="):
			c.mode = strings.TrimPrefix(a, "--permission-mode=")
		}
	}
	if c.session == "" {
		c.session = uuid()
	}
	// A reopened chat continues its transcript after the last line.
	if n, ok := r.fs.stat(r.transcriptPath(cwd, c.session)); ok {
		lines := strings.Split(strings.TrimSpace(string(n.data)), "\n")
		var last struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal([]byte(lines[len(lines)-1]), &last) == nil {
			c.parent = last.UUID
		}
	}
	return c
}

// run reads the app's stream-json lines until the process ends.
func (c *cli) run() {
	go c.loop()
	br := bufio.NewReaderSize(c.stdin, 1<<16)
	for {
		line, err := readLine(br, 64<<20)
		if err != nil {
			break
		}
		var m struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Response  json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		switch m.Type {
		case "control_request":
			c.control(m.RequestID, m.Request)
		case "control_response":
			var resp struct {
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			}
			_ = json.Unmarshal(m.Response, &resp)
			c.mu.Lock()
			ch := c.pending[resp.RequestID]
			delete(c.pending, resp.RequestID)
			c.mu.Unlock()
			if ch != nil {
				ch <- resp.Response
			}
		case "user":
			select {
			case c.turns <- append(json.RawMessage(nil), line...):
			case <-c.ctx.Done():
			}
		}
	}
	c.stop()
}

// stop ends the process: the turn in progress is cancelled and the
// upstream session closed.
func (c *cli) stop() {
	c.cancel()
	c.mu.Lock()
	acp := c.acp
	c.acp = nil
	c.mu.Unlock()
	if acp != nil {
		acp.close()
	}
	c.p.exited(143)
}

// emit writes one stream-json line with a uuid and the session id.
func (c *cli) emit(m map[string]any) {
	if _, ok := m["uuid"]; !ok {
		m["uuid"] = uuid()
	}
	m["session_id"] = c.session
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.out.Write(append(b, '\n'))
	if t, _ := m["type"].(string); t == "user" || t == "assistant" {
		c.record(t, m)
	}
}

// record adds a user or assistant message to the chat's transcript, kept in
// the chats directory and on the virtual tree, so the app can show the chat
// again after its process is gone. It is called with wmu held.
func (c *cli) record(kind string, m map[string]any) {
	var parent any
	if c.parent != "" {
		parent = c.parent
	}
	line, err := json.Marshal(map[string]any{"parentUuid": parent, "isSidechain": false, "userType": "external", "cwd": c.cwd, "sessionId": c.session, "version": c.version, "type": kind, "message": m["message"], "uuid": m["uuid"], "timestamp": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return
	}
	c.parent, _ = m["uuid"].(string)
	if err := (chats{c.route.Chats}).appendLine(c.route.Upstream, c.session, line); err != nil {
		c.route.Logger.Printf("session %s: transcript not kept: %v", c.session, err)
	}
	p := c.route.transcriptPath(c.cwd, c.session)
	old, _ := c.route.fs.stat(p)
	c.route.fs.put(p, append(append(old.data, line...), '\n'), 0o600)
}

func (c *cli) respond(requestID string, response any) {
	r := map[string]any{"subtype": "success", "request_id": requestID}
	if response != nil {
		r["response"] = response
	}
	c.emit(map[string]any{"type": "control_response", "response": r})
}

// control answers one of the app's control requests.
func (c *cli) control(requestID string, raw json.RawMessage) {
	var req struct {
		Subtype string         `json:"subtype"`
		Mode    string         `json:"mode"`
		Props   map[string]any `json:"props"`
	}
	_ = json.Unmarshal(raw, &req)
	switch req.Subtype {
	case "initialize":
		c.respond(requestID, map[string]any{
			"commands": []any{}, "agents": []any{}, "output_style": "default", "available_output_styles": []string{"default"},
			"models":                  []map[string]any{{"value": c.model, "resolvedModel": c.model, "displayName": c.model, "description": "served by comlink", "supportsEffort": false}},
			"account":                 map[string]any{"tokenSource": "none", "apiProvider": "comlink"},
			"pid":                     1,
			"claude_code_version":     c.version,
			"current_permission_mode": c.mode,
			"hooks_applied":           false,
		})
	case "list_permission_rules":
		c.respond(requestID, map[string]any{"state": map[string]any{"rules": []any{}, "workspaceDirectories": []string{c.cwd}, "originalCwd": c.cwd, "managedOnly": false, "parentSettingsIncluded": true}})
	case "get_usage":
		c.respond(requestID, map[string]any{"session": map[string]any{"total_cost_usd": 0, "total_api_duration_ms": 0, "total_duration_ms": 0, "total_lines_added": 0, "total_lines_removed": 0, "model_usage": map[string]any{}}, "subscription_type": nil, "rate_limits_available": false, "rate_limits": nil, "behaviors": nil})
	case "mcp_status":
		c.respond(requestID, map[string]any{"mcpServers": []any{}})
	case "get_context_usage":
		c.respond(requestID, map[string]any{"categories": []any{}, "totalTokens": 0, "maxTokens": 200000, "rawMaxTokens": 200000, "autocompactSource": "model-default", "percentage": 0, "gridRows": []any{}, "model": c.model, "memoryFiles": []any{}, "mcpTools": []any{}, "mcpInstructions": []any{}, "agents": []any{}, "slashCommands": map[string]any{"totalCommands": 0, "includedCommands": 0, "tokens": 0}, "skills": map[string]any{"totalSkills": 0, "includedSkills": 0, "tokens": 0, "skillFrontmatter": []any{}}})
	case "ui_panes":
		c.respond(requestID, map[string]any{"panes": []any{}, "shown_id": nil, "focused_id": nil, "focus_requested_id": nil})
	case "ui_attach":
		c.respond(requestID, map[string]any{"surfaces": []string{"desktop"}})
	case "ui_render":
		c.respond(requestID, map[string]any{"tree": map[string]any{"type": "engine", "ref": 0}, "props": req.Props, "rewritten": false, "hooked": false, "drawn_by": []any{}})
	case "ui_detach":
		c.respond(requestID, map[string]any{"detached": true, "surfaces": []any{}})
	case "set_permission_mode":
		if req.Mode != "" {
			c.mode = req.Mode
		}
		c.respond(requestID, map[string]any{"mode": c.mode})
		c.emit(map[string]any{"type": "system", "subtype": "status", "status": nil, "permissionMode": c.mode})
	case "interrupt":
		c.interrupt()
		c.respond(requestID, map[string]any{})
	default:
		c.respond(requestID, map[string]any{})
	}
}

// interrupt cancels the turn in progress upstream.
func (c *cli) interrupt() {
	c.mu.Lock()
	acp, session := c.acp, c.acpSession
	c.mu.Unlock()
	if acp != nil && session != "" {
		_ = acp.notify("session/cancel", map[string]any{"sessionId": session})
	}
}

// loop runs the queued user messages one after another.
func (c *cli) loop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case raw := <-c.turns:
			c.runTurn(raw)
		}
	}
}

// connect dials the upstream and opens the ACP session once.
func (c *cli) connect() error {
	c.mu.Lock()
	acp := c.acp
	if acp != nil {
		select {
		case <-acp.closed:
			// An idle connection can close while the app and stand-in live.
			// Reopen it before submitting this new prompt, retaining the saved
			// session. Requests already sent are never replayed here.
			c.acp, c.acpSession = nil, ""
		default:
			c.mu.Unlock()
			return nil
		}
	}
	c.mu.Unlock()
	token, err := c.route.Credential.Bearer()
	if err != nil {
		return err
	}
	acp, err = dialACP(c.route.Upstream, token, c.notification, c.request)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, 60*time.Second)
	defer cancel()
	init, err := acp.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}, "clientInfo": map[string]any{"name": "comlink", "version": c.route.Version}})
	if err != nil {
		acp.close()
		return err
	}
	var caps struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(init, &caps)
	store := chats{c.route.Chats}
	c.mu.Lock()
	previous := c.resume
	c.mu.Unlock()
	if previous == "" {
		previous = store.get(c.route.Upstream, c.session)
	}
	session := ""
	if previous != "" && caps.AgentCapabilities.LoadSession {
		// The chat had an agent session: continue it. Its history replays as
		// updates the app already shows, so they are not rendered again.
		c.mu.Lock()
		c.replaying = true
		c.mu.Unlock()
		_, err := acp.call(ctx, "session/load", map[string]any{"sessionId": previous, "cwd": c.cwd, "mcpServers": []any{}})
		c.mu.Lock()
		c.replaying = false
		c.mu.Unlock()
		if err == nil {
			session = previous
		} else {
			c.route.Logger.Printf("session %s: agent session %s could not be loaded, starting a new one: %v", c.session, previous, err)
		}
	}
	if session == "" {
		result, err := acp.call(ctx, "session/new", map[string]any{"cwd": c.cwd, "mcpServers": []any{}})
		if err != nil {
			acp.close()
			return err
		}
		var sess struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(result, &sess); err != nil || sess.SessionID == "" {
			acp.close()
			return errors.New("session/new: no sessionId in the result")
		}
		session = sess.SessionID
		if err := store.put(c.route.Upstream, c.session, session); err != nil {
			c.route.Logger.Printf("session %s: the chat will not resume its agent session: %v", c.session, err)
		}
	}
	c.mu.Lock()
	c.acp, c.acpSession, c.resume = acp, session, session
	c.mu.Unlock()
	return nil
}

// turn is one user message being answered.
type turn struct {
	c         *cli
	userUUID  string
	msgID     string
	open      bool
	index     int
	blockType string
	block     strings.Builder
	content   []map[string]any
	text      strings.Builder
	tools     map[string]string // tool call id -> name
}

// runTurn answers one user message: the app's lifecycle lines, the prompt
// upstream, its updates as stream events, then the result.
func (c *cli) runTurn(raw json.RawMessage) {
	var user map[string]any
	_ = json.Unmarshal(raw, &user)
	userUUID, _ := user["uuid"].(string)
	if userUUID == "" {
		userUUID = uuid()
	}
	t := &turn{c: c, userUUID: userUUID, tools: map[string]string{}}
	c.mu.Lock()
	c.turn = t
	c.mu.Unlock()
	started := time.Now()
	c.emit(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "running", "sdk_host_only": true})
	c.emit(map[string]any{"type": "command_lifecycle", "command_uuid": userUUID, "state": "queued"})
	c.emit(map[string]any{"type": "command_lifecycle", "command_uuid": userUUID, "state": "started"})
	c.emit(map[string]any{"type": "system", "subtype": "init", "cwd": c.cwd, "tools": []any{}, "mcp_servers": []any{}, "model": c.model, "permissionMode": c.mode, "slash_commands": []any{}, "apiKeySource": "none", "claude_code_version": c.version, "output_style": "default", "agents": []any{}, "skills": []any{}, "plugins": []any{}})
	c.emit(map[string]any{"type": "system", "subtype": "status", "status": "requesting", "user_message_uuids": []string{userUUID}})
	echo := map[string]any{}
	for k, v := range user {
		echo[k] = v
	}
	echo["isReplay"] = true
	echo["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	c.emit(echo)

	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "stop_reason": "end_turn", "terminal_reason": "completed", "total_cost_usd": 0, "usage": map[string]any{"input_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 0}, "modelUsage": map[string]any{}, "permission_denials": []any{}}
	if err := c.prompt(t, promptText(user)); err != nil {
		t.finish("end_turn")
		c.route.Logger.Printf("session %s: %v", c.session, err)
		result["subtype"], result["is_error"], result["result"] = "error_during_execution", true, err.Error()
		result["stop_reason"], result["terminal_reason"] = "error", "error"
	} else {
		t.finish("end_turn")
		result["result"] = t.text.String()
	}
	result["duration_ms"] = time.Since(started).Milliseconds()
	result["duration_api_ms"] = time.Since(started).Milliseconds()
	c.mu.Lock()
	c.turn = nil
	c.mu.Unlock()
	c.emit(result)
	c.emit(map[string]any{"type": "command_lifecycle", "command_uuid": userUUID, "state": "completed"})
	c.emit(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "idle", "sdk_host_only": true})
}

// promptText is the text of the app's user message: a string, or the text
// blocks of a content array.
func promptText(user map[string]any) string {
	msg, _ := user["message"].(map[string]any)
	switch content := msg["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, block := range content {
			if b, ok := block.(map[string]any); ok {
				if text, ok := b["text"].(string); ok && b["type"] == "text" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// prompt sends text as the session's prompt and returns when the agent's
// turn ends.
func (c *cli) prompt(t *turn, text string) error {
	if err := c.connect(); err != nil {
		return err
	}
	c.mu.Lock()
	acp, session := c.acp, c.acpSession
	c.mu.Unlock()
	if acp == nil {
		return errors.New("process ended")
	}
	_, err := acp.call(c.ctx, "session/prompt", map[string]any{"sessionId": session, "prompt": []map[string]any{{"type": "text", "text": text}}})
	if err != nil {
		// The upstream is gone: the next turn dials again.
		if errors.Is(err, c.ctx.Err()) || strings.HasSuffix(err.Error(), "upstream closed") {
			c.mu.Lock()
			if c.acp == acp {
				c.acp, c.acpSession = nil, ""
			}
			c.mu.Unlock()
			acp.close()
		}
	}
	_ = t
	return err
}

// notification handles the upstream's session updates.
func (c *cli) notification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	var p struct {
		Update struct {
			Kind       string          `json:"sessionUpdate"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Status     string          `json:"status"`
			RawInput   json.RawMessage `json:"rawInput"`
			RawOutput  json.RawMessage `json:"rawOutput"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	c.mu.Lock()
	t, replaying := c.turn, c.replaying
	c.mu.Unlock()
	if t == nil || replaying {
		return
	}
	u := p.Update
	switch u.Kind {
	case "agent_message_chunk":
		t.delta("text", textOf(u.Content))
	case "agent_thought_chunk":
		t.delta("thinking", textOf(u.Content))
	case "tool_call":
		t.toolUse(u.ToolCallID, u.Title, u.RawInput)
	case "tool_call_update":
		if u.Status == "completed" || u.Status == "failed" {
			t.toolResult(u.ToolCallID, u.Status == "failed", u.Content, u.RawOutput)
		}
	}
}

// textOf is the text of an ACP content block, or of a list of them.
func textOf(raw json.RawMessage) string {
	var one struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &one); err == nil {
		if one.Type == "text" {
			return one.Text
		}
		if one.Type == "content" {
			return textOf(one.Content)
		}
	}
	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		var parts []string
		for _, m := range many {
			if s := textOf(m); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// request answers the upstream's requests: a permission request becomes
// the app's can_use_tool prompt.
func (c *cli) request(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	if method != "session/request_permission" {
		return nil, &rpcError{-32601, "method not found: " + method}
	}
	var p struct {
		ToolCall struct {
			ID       string          `json:"toolCallId"`
			Title    string          `json:"title"`
			Kind     string          `json:"kind"`
			RawInput json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{-32602, "invalid params"}
	}
	pick := func(kinds ...string) string {
		for _, kind := range kinds {
			for _, o := range p.Options {
				if o.Kind == kind {
					return o.ID
				}
			}
		}
		if len(p.Options) > 0 {
			return p.Options[len(p.Options)-1].ID
		}
		return ""
	}
	requestID := uuid()
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.pending[requestID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
		if ctx.Err() != nil || c.ctx.Err() != nil {
			c.emit(map[string]any{"type": "control_cancel_request", "request_id": requestID})
		}
	}()
	input := any(map[string]any{"title": p.ToolCall.Title})
	if len(p.ToolCall.RawInput) > 0 {
		input = p.ToolCall.RawInput
	}
	toolUseID := p.ToolCall.ID
	if toolUseID == "" {
		toolUseID = requestID
	}
	name := p.ToolCall.Title
	if name == "" {
		name = p.ToolCall.Kind
	}
	c.emit(map[string]any{"type": "control_request", "request_id": requestID, "request": map[string]any{"subtype": "can_use_tool", "tool_name": name, "display_name": name, "input": input, "tool_use_id": toolUseID, "requires_user_interaction": true}})
	select {
	case raw := <-ch:
		var resp struct {
			Behavior string `json:"behavior"`
		}
		_ = json.Unmarshal(raw, &resp)
		if resp.Behavior == "allow" {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": pick("allow_once", "allow_always")}}, nil
		}
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": pick("reject_once", "reject_always")}}, nil
	case <-c.ctx.Done():
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	case <-ctx.Done():
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
}

// event emits one stream_event of the turn's message.
func (t *turn) event(e map[string]any) {
	m := map[string]any{"type": "stream_event", "event": e, "parent_tool_use_id": nil, "api_message_id": t.msgID}
	if e["type"] == "message_start" {
		m["user_message_uuid"] = t.userUUID
		m["user_message_uuids"] = []string{t.userUUID}
	}
	t.c.emit(m)
}

func (t *turn) message() map[string]any {
	return map[string]any{"model": t.c.model, "id": t.msgID, "type": "message", "role": "assistant", "content": t.content, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}
}

// start opens a message when none is open.
func (t *turn) start() {
	if t.open {
		return
	}
	t.msgID = "msg_" + randomID(12)
	t.open = true
	t.index = -1
	t.content = nil
	msg := t.message()
	msg["content"] = []any{}
	t.event(map[string]any{"type": "message_start", "message": msg})
}

// delta appends text to the block of kind (text or thinking), opening the
// message and the block as needed.
func (t *turn) delta(kind, text string) {
	if text == "" {
		return
	}
	t.start()
	if t.blockType != kind {
		t.closeBlock()
		t.index++
		t.blockType = kind
		block := map[string]any{"type": "text", "text": ""}
		if kind == "thinking" {
			block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
		}
		t.event(map[string]any{"type": "content_block_start", "index": t.index, "content_block": block})
	}
	t.block.WriteString(text)
	if kind == "thinking" {
		t.event(map[string]any{"type": "content_block_delta", "index": t.index, "delta": map[string]any{"type": "thinking_delta", "thinking": text}})
		return
	}
	t.text.WriteString(text)
	t.event(map[string]any{"type": "content_block_delta", "index": t.index, "delta": map[string]any{"type": "text_delta", "text": text}})
}

func (t *turn) closeBlock() {
	if t.blockType == "" {
		return
	}
	t.event(map[string]any{"type": "content_block_stop", "index": t.index})
	if t.blockType == "thinking" {
		t.content = append(t.content, map[string]any{"type": "thinking", "thinking": t.block.String(), "signature": ""})
	} else {
		t.content = append(t.content, map[string]any{"type": "text", "text": t.block.String()})
	}
	t.block.Reset()
	t.blockType = ""
}

// toolUse shows a tool call as a tool_use block that ends the message.
func (t *turn) toolUse(id, title string, rawInput json.RawMessage) {
	t.start()
	t.closeBlock()
	t.index++
	if id == "" {
		id = "toolu_" + randomID(12)
	}
	t.tools[id] = title
	var input any = map[string]any{}
	if len(rawInput) > 0 {
		input = rawInput
	}
	block := map[string]any{"type": "tool_use", "id": id, "name": title, "input": input}
	t.event(map[string]any{"type": "content_block_start", "index": t.index, "content_block": block})
	t.event(map[string]any{"type": "content_block_stop", "index": t.index})
	t.content = append(t.content, block)
	t.finish("tool_use")
}

// toolResult reports a finished tool call as the tool_result the app shows
// under its tool_use.
func (t *turn) toolResult(id string, failed bool, content, rawOutput json.RawMessage) {
	text := textOf(content)
	if text == "" && len(rawOutput) > 0 {
		text = string(rawOutput)
	}
	t.c.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": failed}}}, "parent_tool_use_id": nil})
}

// finish closes the open message with stopReason.
func (t *turn) finish(stopReason string) {
	if !t.open {
		return
	}
	t.closeBlock()
	if t.content == nil {
		t.content = []map[string]any{}
	}
	t.c.emit(map[string]any{"type": "assistant", "message": t.message(), "parent_tool_use_id": nil})
	t.event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	t.event(map[string]any{"type": "message_stop"})
	t.open = false
}

// uuid is a random version 4 UUID.
func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
