package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

// options configures one SSH connection served by comlink.
type options struct {
	upstream      string
	token         string
	reportVersion string
	hostKey       ssh.Signer
	logger        *log.Logger
}

// serve speaks the server side of SSH on conn for one connection and returns
// when the client disconnects. No client authentication is performed: the
// transport is the user's own ProxyCommand process, not a network listener.
func serve(conn net.Conn, opts options) error {
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(opts.hostKey)

	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return fmt.Errorf("ssh handshake: %w", err)
	}
	defer func() { _ = sconn.Close() }()

	go handleGlobalRequests(reqs)

	var wg sync.WaitGroup
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "comlink only opens session channels")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		wg.Go(func() { handleSession(ch, chReqs, opts) })
	}
	wg.Wait()
	return nil
}

// handleGlobalRequests accepts port-forward requests without forwarding
// anything (the Codex app insists on -R and exits if it is refused) and
// refuses everything else.
func handleGlobalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			var fwd struct {
				Addr string
				Port uint32
			}
			if err := ssh.Unmarshal(req.Payload, &fwd); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			var reply []byte
			if fwd.Port == 0 {
				// The client asked for a dynamic port; it expects one in the reply.
				reply = ssh.Marshal(struct{ Port uint32 }{49152})
			}
			_ = req.Reply(true, reply)
		case "cancel-tcpip-forward":
			_ = req.Reply(true, nil)
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// handleSession answers the requests on one session channel. Only exec is
// served; shell, pty, subsystem and agent forwarding are refused.
func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request, opts options) {
	defer func() { _ = ch.Close() }()
	done := make(chan struct{})
	ran := false
	for req := range reqs {
		switch req.Type {
		case "env":
			_ = req.Reply(true, nil)
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil || ran {
				_ = req.Reply(false, nil)
				continue
			}
			ran = true
			_ = req.Reply(true, nil)
			go func() {
				defer close(done)
				status := runCommand(ch, payload.Command, opts)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
				_ = ch.CloseWrite()
				_ = ch.Close()
			}()
		default:
			// shell, pty-req, subsystem (including sftp), auth-agent-req@openssh.com, ...
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	if ran {
		<-done
	}
}

// runCommand answers one exec request on ch and returns the exit status.
// Nothing is executed: the command is matched against the Codex app route.
func runCommand(ch ssh.Channel, exec string, opts options) int {
	cmd := parseCommand(exec)
	if len(cmd.Marker) > 0 && cmd.Kind != kindUnknown {
		if _, err := ch.Write(cmd.Marker); err != nil {
			return 1
		}
	}
	switch cmd.Kind {
	case kindProxy:
		return relay(ch, opts)
	case kindBootstrap, kindKill, kindProbe:
		return 0
	case kindVersion:
		_, _ = io.WriteString(ch, "codex-cli "+opts.reportVersion+"\n")
		return 0
	default:
		// The client sees this as the command's stderr. It is not logged as well:
		// a ProxyCommand's own stderr reaches the same terminal and would repeat it.
		_, _ = io.WriteString(ch.Stderr(), "comlink: unsupported command: "+cmd.Payload+"\n")
		return 127
	}
}
