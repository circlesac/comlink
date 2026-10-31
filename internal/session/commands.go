package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/circlesac/comlink/internal/sshd"
	"github.com/circlesac/comlink/internal/upstream"
)

// Flags are the flags of `comlink codex`: without Stdio the session,
// with it one SSH connection on stdin/stdout. Which of them go together is
// more than kong can express, so Check says.
type Flags struct {
	Stdio         bool
	Name          string
	Server        string
	TokenFile     string
	ReportVersion string
}

// Check rejects the combinations `comlink codex` refuses. Without Stdio,
// Name and Server are both required and ReportVersion is refused; with
// it, exactly one of Name and Server is required, and TokenFile goes only
// with Server, because a session's token comes from the session.
func (f Flags) Check() error {
	if !f.Stdio {
		if f.ReportVersion != "" {
			return errors.New("--report-version goes with --stdio")
		}
		var missing []string
		if f.Name == "" {
			missing = append(missing, "--name=NAME")
		}
		if f.Server == "" {
			missing = append(missing, "--server=URL")
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing flags: %s", strings.Join(missing, ", "))
		}
		return nil
	}
	switch {
	case f.Name == "" && f.Server == "":
		return errors.New("--stdio needs --name NAME (a session from 'comlink codex --name NAME') or --server URL")
	case f.Name != "" && f.Server != "":
		return errors.New("--stdio takes --name or --server, not both")
	case f.Name != "" && f.TokenFile != "":
		return errors.New("--token-file goes with --server; a session's token comes from 'comlink codex --name NAME'")
	}
	return nil
}

// CheckUnset rejects any of the flags given to the subcommand sub of
// `comlink codex` (list), which kong lets inherit them.
func (f Flags) CheckUnset(sub string) error {
	if f != (Flags{}) {
		return fmt.Errorf("'comlink codex %s' takes no flags; --stdio, --name, --server, --token-file and --report-version go with 'comlink codex' itself", sub)
	}
	return nil
}

// Endpoint returns the Codex endpoint and token that `comlink codex --stdio`
// relays to: the running session Name's, or <Server>/codex with the token
// from TokenFile or $COMLINK_TOKEN (warnings go to logger). The state
// directory is opened only for Name.
func (f Flags) Endpoint(logger *log.Logger) (endpoint, token string, err error) {
	if f.Name == "" {
		if endpoint, err = upstream.Endpoint(f.Server, "codex"); err != nil {
			return "", "", err
		}
		token, err = upstream.LoadToken(f.TokenFile, logger)
		return endpoint, token, err
	}
	store, err := Open()
	if err != nil {
		return "", "", err
	}
	return store.Resolve(f.Name)
}

// Up is `comlink codex --name NAME --server URL`. Once name and server are
// valid it makes sure the ssh config at sshConfig includes the session
// config files (adding the line as include does and saying so on stderr;
// the line stays after the session), then registers name with server and
// token, writes the ssh config block whose ProxyCommand runs
// `<exe> codex --stdio --name NAME`, prints the alias on stdout, and holds
// the session until ctx is done. It then removes the session's files and
// returns nil. When the Include line cannot be added no session file is
// written.
func (s *Store) Up(ctx context.Context, name, server, token, exe, sshConfig string, stdout, stderr io.Writer) error {
	block, err := sshd.ConfigBlock(name, []string{exe, "codex", "--stdio", "--name", name})
	if err != nil {
		return err
	}
	if _, err := upstream.Endpoint(server, "codex"); err != nil {
		return err
	}
	if err := s.addInclude(sshConfig, stderr); err != nil {
		return err
	}
	sess, err := s.Start(name, server, token, block)
	if err != nil {
		return err
	}
	if sess.Replaced != nil {
		_, _ = fmt.Fprintf(stderr, "comlink: removed stale session %q (pid %d is gone)\n", sess.Replaced.Name, sess.Replaced.PID)
	}
	_, _ = fmt.Fprintf(stdout, "%s is up (pid %d)\n", sess.Name, sess.PID)
	_, _ = fmt.Fprintf(stdout, "  server:     %s\n", sess.Server)
	_, _ = fmt.Fprintf(stdout, "  Codex app:  SSH connection %q (ssh %s)\n", sess.Name, sess.Name)
	_, _ = fmt.Fprintln(stdout, "Press Ctrl-C to stop.")
	<-ctx.Done()
	return sess.Close()
}

// Resolve returns the Codex endpoint and the token of the session called
// name. A name that is not up is a *NotUpError.
func (s *Store) Resolve(name string) (endpoint, token string, err error) {
	record, token, err := s.Load(name)
	if err != nil {
		return "", "", err
	}
	return record.Codex, token, nil
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
