package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/circlesac/comlink/internal/upstream"
	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// DirectTCPIP serves the loopback HTTP forward the desktop creates for browser
// sign-in. It never dials the requested address on the local machine.
func (r *Route) DirectTCPIP(parent context.Context, host string, port uint32) (func(ssh.Channel), bool) {
	if port != 1455 || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return nil, false
	}
	return func(ch ssh.Channel) { r.forwardHTTP(parent, ch, host, port) }, true
}

func (r *Route) forwardHTTP(parent context.Context, ch ssh.Channel, host string, port uint32) {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = ch.Close() })
	defer stop()
	fail := func(status int) {
		response := &http.Response{StatusCode: status, ProtoMajor: 1, ProtoMinor: 1, Close: true, ContentLength: 0, Header: http.Header{"Cache-Control": {"no-store"}}}
		_ = response.Write(ch)
	}
	request, err := http.ReadRequest(bufio.NewReader(io.LimitReader(ch, 32<<10)))
	if err != nil {
		fail(http.StatusBadRequest)
		return
	}
	if request.Method != http.MethodGet || request.ContentLength > 0 || len(request.TransferEncoding) != 0 || request.URL.IsAbs() || request.URL.Host != "" {
		fail(http.StatusBadRequest)
		return
	}
	_ = request.Body.Close()
	endpoint, err := url.Parse(r.Upstream)
	if err != nil {
		fail(http.StatusBadGateway)
		return
	}
	endpoint = endpoint.JoinPath("forward")
	token, err := r.Credential.BearerContext(ctx)
	if err != nil {
		r.Logger.Print("callback credential is unavailable")
		fail(http.StatusBadGateway)
		return
	}
	remote, err := upstream.DialContext(ctx, endpoint.String(), token)
	if err != nil {
		r.Logger.Print("callback endpoint is unavailable")
		fail(http.StatusBadGateway)
		return
	}
	defer func() { _ = remote.CloseNow() }()
	remote.SetReadLimit(2 << 20)
	packet, err := json.Marshal(struct {
		Host string `json:"host"`
		Port uint32 `json:"port"`
		Path string `json:"path"`
	}{host, port, request.URL.RequestURI()})
	if err != nil || remote.Write(ctx, websocket.MessageText, packet) != nil {
		fail(http.StatusBadGateway)
		return
	}
	typ, payload, err := remote.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		fail(http.StatusBadGateway)
		return
	}
	var result struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    *string           `json:"bodyBase64"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.Status < 200 || result.Status > 599 || result.Headers == nil || result.Body == nil {
		fail(http.StatusBadGateway)
		return
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		fail(http.StatusBadGateway)
		return
	}
	body, err := base64.StdEncoding.DecodeString(*result.Body)
	if err != nil || len(body) > 1<<20 {
		fail(http.StatusBadGateway)
		return
	}
	headers := http.Header{}
	for name, value := range result.Headers {
		if (name != "content-type" && name != "location" && name != "cache-control" && name != "content-security-policy" && name != "x-content-type-options") || len(value) > 8192 || strings.ContainsAny(value, "\r\n") {
			fail(http.StatusBadGateway)
			return
		}
		headers.Set(name, value)
	}
	headers.Set("Cache-Control", "no-store")
	response := &http.Response{StatusCode: result.Status, ProtoMajor: 1, ProtoMinor: 1, Close: true, ContentLength: int64(len(body)), Header: headers, Body: io.NopCloser(bytes.NewReader(body))}
	_ = response.Write(ch)
}
