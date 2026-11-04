// Package codex is the Codex desktop app's route: it recognises the shell
// commands the app sends over SSH, answers them, and relays the app-server
// proxy session to an upstream WebSocket.
package codex

import (
	"io"
	"log"

	"golang.org/x/crypto/ssh"

	"github.com/circlesac/comlink/internal/upstream"
)

// Route answers the Codex app's exec requests and relays its app-server
// session to Upstream.
type Route struct {
	Upstream      string              // WebSocket endpoint that receives the agent session
	Credential    upstream.Credential // Upstream's bearer token, resolved when the relay dials; none sends no Authorization header
	ReportVersion string              // version answered to codex --version
	Logger        *log.Logger         // diagnostics; never given the token
}

// Exec answers one exec request on ch and returns the exit status.
// Nothing is executed: the command is matched against the Codex app route.
func (r *Route) Exec(ch ssh.Channel, exec string) int {
	cmd := parseCommand(exec)
	if len(cmd.Marker) > 0 && cmd.Kind != kindUnknown {
		if _, err := ch.Write(cmd.Marker); err != nil {
			return 1
		}
	}
	switch cmd.Kind {
	case kindProxy:
		return r.relay(ch)
	case kindBootstrap, kindKill, kindProbe:
		return 0
	case kindVersion:
		_, _ = io.WriteString(ch, "codex-cli "+r.ReportVersion+"\n")
		return 0
	default:
		// The client sees this as the command's stderr. It is not logged as well:
		// a ProxyCommand's own stderr reaches the same terminal and would repeat it.
		_, _ = io.WriteString(ch.Stderr(), "comlink: unsupported command: "+cmd.Payload+"\n")
		return 127
	}
}
