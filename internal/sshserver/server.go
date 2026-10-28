// Package sshserver speaks the server side of SSH over one connection. It
// accepts session channels and hands each exec request to a Route; it knows
// nothing about the client on the other end. It also renders the
// ~/.ssh/config block that routes an alias through comlink.
package sshserver

import (
	"fmt"
	"net"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Route answers exec requests for one kind of client. Exec serves command on
// ch (stdout, stderr and stdin are the channel's) and returns the exit
// status. Nothing is run by the server itself.
type Route interface {
	Exec(ch ssh.Channel, command string) (status int)
}

// Serve speaks the server side of SSH on conn for one connection and returns
// when the client disconnects. No client authentication is performed: the
// transport is the user's own ProxyCommand process, not a network listener.
func Serve(conn net.Conn, hostKey ssh.Signer, route Route) error {
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(hostKey)

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
		wg.Go(func() { handleSession(ch, chReqs, route) })
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
func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request, route Route) {
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
				status := route.Exec(ch, payload.Command)
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
