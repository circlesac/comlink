// Package upstream dials the WebSocket endpoint that receives an agent
// session. It is shared by every route: the bearer token goes only into the
// Authorization header, and errors name the endpoint without its secrets.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// ReadLimit is the largest WebSocket message accepted from either side of a
// relay. The Codex app announces 16 MiB messages; the library default is
// 32 KiB.
const ReadLimit = 64 << 20

// Dial opens the WebSocket to rawURL with token as its bearer token (no
// Authorization header when token is empty) and raises the read limit to
// ReadLimit. An error names the upstream without its query or userinfo and
// never carries the header.
func Dial(rawURL, token string) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var header http.Header
	if token != "" {
		header = http.Header{"Authorization": {"Bearer " + token}}
	}
	c, resp, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{HTTPHeader: header})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, redactDialError(err, rawURL)
	}
	c.SetReadLimit(ReadLimit)
	return c, nil
}

// redactDialError rewrites a websocket.Dial failure so that it names the
// upstream without its query or userinfo. Go's URL and HTTP errors quote the
// full URL, so an upstream such as wss://host/rpc?token=... would otherwise
// put the token in the log.
func redactDialError(err error, rawURL string) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err // the cause, without the URL
	}
	return fmt.Errorf("dial %s: %w", redactURL(rawURL), err)
}

// redactURL returns raw without its userinfo, query and fragment. A URL that
// does not parse is not echoed at all: its secret part cannot be told apart.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid upstream URL>"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// LoadToken returns the trimmed contents of path, or $COMLINK_TOKEN when
// path is empty. An empty result means no token: the upstream is dialled
// without an Authorization header. A token file that group or others can
// read is still used, with a warning naming the path (never the token).
func LoadToken(path string, logger *log.Logger) (string, error) {
	if path == "" {
		return strings.TrimSpace(os.Getenv("COMLINK_TOKEN")), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		logger.Printf("warning: token file %s is readable by group or others; restrict it with chmod 600", path)
	}
	return strings.TrimSpace(string(b)), nil
}
