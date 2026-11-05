package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/coder/websocket"

	"github.com/circlesac/comlink/internal/upstream"
)

// acpConn is an ACP client connection: JSON-RPC 2.0, one message per text
// frame, with requests both ways.
type acpConn struct {
	conn      *websocket.Conn
	mu        sync.Mutex
	next      int
	pending   map[string]chan acpReply
	closed    chan struct{}
	err       error
	onNotify  func(method string, params json.RawMessage)
	onRequest func(method string, params json.RawMessage) (any, *rpcError)
}

type acpReply struct {
	result json.RawMessage
	err    *rpcError
}

type acpMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// dialACP connects to url with token and starts reading.
func dialACP(url, token string, onNotify func(string, json.RawMessage), onRequest func(string, json.RawMessage) (any, *rpcError)) (*acpConn, error) {
	conn, err := upstream.Dial(url, token)
	if err != nil {
		return nil, err
	}
	c := &acpConn{conn: conn, pending: map[string]chan acpReply{}, closed: make(chan struct{}), onNotify: onNotify, onRequest: onRequest}
	go c.read()
	return c, nil
}

func (c *acpConn) read() {
	defer func() {
		c.mu.Lock()
		for _, ch := range c.pending {
			ch <- acpReply{err: &rpcError{-32000, "upstream closed"}}
		}
		c.pending = map[string]chan acpReply{}
		c.mu.Unlock()
		close(c.closed)
	}()
	for {
		typ, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.err = err
			return
		}
		if typ != websocket.MessageText {
			c.err = errors.New("upstream sent a binary message")
			_ = c.conn.Close(websocket.StatusUnsupportedData, "binary message")
			return
		}
		var m acpMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			go func() {
				result, rpcErr := c.onRequest(m.Method, m.Params)
				resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
				if rpcErr != nil {
					resp["error"] = rpcErr
				} else {
					resp["result"] = result
				}
				_ = c.write(resp)
			}()
		case m.Method != "":
			c.onNotify(m.Method, m.Params)
		default:
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- acpReply{result: m.Result, err: m.Error}
			}
		}
	}
}

func (c *acpConn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.conn.Write(context.Background(), websocket.MessageText, b)
}

// call sends a request and waits for its reply, ctx or the connection's end.
func (c *acpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := strconv.Itoa(c.next)
	ch := make(chan acpReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, fmt.Errorf("%s: %s", method, reply.err.Message)
		}
		return reply.result, nil
	case <-c.closed:
		return nil, fmt.Errorf("%s: upstream closed", method)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *acpConn) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *acpConn) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}
