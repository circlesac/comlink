package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/upstream"
	"github.com/coder/websocket"
)

func TestCallbackCredentialCommandObservesSSHOwnerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	token, err := (upstream.Credential{Command: "echo " + testToken}).BearerContext(ctx)
	if token != "" || err == nil || !strings.Contains(err.Error(), "cancelled") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("cancelled credential command returned a token or unsafe error")
	}
}

func TestBinaryCallbackForwardThroughOpenSSH(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/forward" || r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(401)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, _, err = ws.Read(ctx)
		if err != nil {
			return
		}
		_ = ws.Write(ctx, websocket.MessageText, []byte(`{"status":200,"headers":{"content-type":"text/html"},"bodyBase64":"cGFnZQ=="}`))
	}))
	defer server.Close()
	bin := buildComlink(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testToken), 0600); err != nil {
		t.Fatal(err)
	}
	proxy := quote(bin) + " codex --stdio --server " + quote("ws"+strings.TrimPrefix(server.URL, "http")) + " --token-file " + quote(tokenFile)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-o", "ProxyCommand="+proxy, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-W", "127.0.0.1:1455", "comlink@example-agent")
	command.Env = append(os.Environ(), "XDG_STATE_HOME="+t.TempDir())
	command.Stdin = strings.NewReader("GET /success HTTP/1.1\r\nHost: localhost\r\n\r\n")
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	if err != nil {
		t.Fatalf("built CLI forward: %v (%s)", err, diagnostic.String())
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(output)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "page" {
		t.Fatalf("forwarded response: %d %q %v", response.StatusCode, body, err)
	}
	if strings.Contains(diagnostic.String(), testToken) {
		t.Fatal("CLI diagnostic exposed token")
	}
}

func TestCodexCallbackForwardsHTTPOverRealSSHWithSecretOnlyInFrame(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/prefix/codex/forward" || req.URL.RawQuery != "" || req.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("wrong callback endpoint or authentication")
			w.WriteHeader(401)
			return
		}
		ws, err := websocket.Accept(w, req, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		typ, packet, err := ws.Read(ctx)
		if err != nil || typ != websocket.MessageText {
			t.Errorf("read callback: %v", err)
			return
		}
		var value struct {
			Host string
			Port uint32
			Path string
		}
		if json.Unmarshal(packet, &value) != nil || value.Host != "127.0.0.1" || value.Port != 1455 || value.Path != "/auth/callback?code=private-code&state=private-state" {
			t.Error("callback request changed")
		}
		received <- struct{}{}
		_ = ws.Write(ctx, websocket.MessageText, []byte(`{"status":302,"headers":{"location":"/success"},"bodyBase64":"`+base64.StdEncoding.EncodeToString([]byte("native-page"))+`"}`))
	}))
	defer server.Close()
	client, logs := startCodex(t, codex.Route{Upstream: "ws" + strings.TrimPrefix(server.URL, "http") + "/prefix/codex", Credential: upstream.Credential{Token: testToken}})
	conn, err := client.Dial("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	request, _ := http.NewRequest(http.MethodGet, "http://localhost:1455/auth/callback?code=private-code&state=private-state", nil)
	if err := request.Write(conn); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 302 || response.Header.Get("Location") != "/success" || response.Header.Get("Cache-Control") != "no-store" || string(body) != "native-page" {
		t.Fatalf("changed HTTP callback response: %d %q", response.StatusCode, body)
	}
	<-received
	for _, secret := range []string{testToken, "private-code", "private-state"} {
		if strings.Contains(logs.String(), secret) {
			t.Error("callback diagnostic exposed secret")
		}
	}
}

func TestCodexCallbackRefusesOtherDestinationsAndMalformedUpstream(t *testing.T) {
	client, _ := startCodex(t, codex.Route{Upstream: unusedUpstream})
	for _, destination := range []string{"outside.example:1455", "127.0.0.1:22", "localhost:65535"} {
		conn, err := client.Dial("tcp", destination)
		if err == nil {
			_ = conn.Close()
			t.Errorf("accepted %s", destination)
		}
	}
	for _, reply := range []string{`not-json`, `{"status":101,"headers":{},"bodyBase64":""}`, `{"status":200,"headers":{"location":"/success\r\nX-Forged: unsafe"},"bodyBase64":""}`, `{"status":200,"headers":{},"bodyBase64":"%"}`, `{"status":200,"headers":{}}`, `{"status":200,"headers":{},"bodyBase64":""} {}`} {
		t.Run(reply, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.CloseNow() }()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				_, _, _ = ws.Read(ctx)
				_ = ws.Write(ctx, websocket.MessageText, []byte(reply))
			}))
			defer server.Close()
			client, _ := startCodex(t, codex.Route{Upstream: "ws" + strings.TrimPrefix(server.URL, "http") + "/codex"})
			conn, err := client.Dial("tcp", "localhost:1455")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.WriteString(conn, "GET /success HTTP/1.1\r\nHost: localhost\r\n\r\n")
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != 502 {
				t.Fatalf("status %d, want 502", response.StatusCode)
			}
		})
	}
}

func TestCodexCallbackDisconnectCancelsPendingEndpoint(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		_, _, err = ws.Read(r.Context())
		if err != nil {
			return
		}
		close(started)
		_, _, _ = ws.Read(r.Context())
		close(stopped)
	}))
	defer server.Close()
	client, _ := startCodex(t, codex.Route{Upstream: "ws" + strings.TrimPrefix(server.URL, "http") + "/codex"})
	conn, err := client.Dial("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /success HTTP/1.1\r\nHost: localhost\r\n\r\n")
	<-started
	_ = client.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("callback survived SSH disconnect")
	}
}
