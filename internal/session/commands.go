package session

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/circlesac/comlink/internal/sshd"
)

// Up registers name with server and token, writes the ssh config block
// whose ProxyCommand runs `<exe> codex proxy --name NAME`, prints what is
// available on stdout (and, on stderr, a hint when sshConfig does not
// include the session config files), and holds the session until ctx is
// done. It then removes the session's files and returns nil.
func (s *Store) Up(ctx context.Context, name, server, token, exe, sshConfig string, stdout, stderr io.Writer) error {
	block, err := sshd.ConfigBlock(name, []string{exe, "codex", "proxy", "--name", name})
	if err != nil {
		return err
	}
	sess, err := s.Start(name, server, token, block)
	if err != nil {
		return err
	}
	if sess.Replaced != nil {
		_, _ = fmt.Fprintf(stderr, "comlink: removed stale session %q (pid %d is gone)\n", sess.Replaced.Name, sess.Replaced.PID)
	}
	if included, err := s.Included(sshConfig); err != nil || !included {
		_, _ = fmt.Fprintf(stderr, "comlink: %s does not include %s; run 'comlink ssh-include' once so the Codex app can use the alias\n", sshConfig, s.IncludePattern())
	}
	_, _ = fmt.Fprintf(stdout, "%s is up (pid %d)\n", sess.Name, sess.PID)
	_, _ = fmt.Fprintf(stdout, "  server:       %s\n", sess.Server)
	_, _ = fmt.Fprintf(stdout, "  Codex app:    SSH connection %q (ssh %s)\n", sess.Name, sess.Name)
	_, _ = fmt.Fprintf(stdout, "  ACP clients:  agent command %s acp --name %s\n", exe, sess.Name)
	_, _ = fmt.Fprintln(stdout, "Press Ctrl-C to stop.")
	<-ctx.Done()
	return sess.Close()
}

// Resolve returns the endpoint and token of the session called name for
// protocol, "codex" or "acp". A name that is not up is a *NotUpError.
func (s *Store) Resolve(name, protocol string) (endpoint, token string, err error) {
	record, token, err := s.Load(name)
	if err != nil {
		return "", "", err
	}
	switch protocol {
	case "codex":
		return record.Codex, token, nil
	case "acp":
		return record.ACP, token, nil
	}
	return "", "", errors.New("unknown protocol " + protocol)
}

// PrintList prints one line per session that is up (name, pid and server;
// never the token) on stdout, reports removed stale records on stderr and
// says on stderr when nothing is up.
func (s *Store) PrintList(stdout, stderr io.Writer) error {
	live, stale, err := s.List()
	if err != nil {
		return err
	}
	for _, r := range stale {
		_, _ = fmt.Fprintf(stderr, "comlink: removed stale session %q (pid %d is gone)\n", r.Name, r.PID)
	}
	if len(live) == 0 {
		_, _ = fmt.Fprintln(stderr, "comlink: no sessions are up")
		return nil
	}
	for _, r := range live {
		_, _ = fmt.Fprintf(stdout, "%s\tup\tpid %d\t%s\n", r.Name, r.PID, r.Server)
	}
	return nil
}
