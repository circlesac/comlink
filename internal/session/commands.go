package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"net/url"
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
// Server is required, Name is optional (Up derives it from Server) and
// ReportVersion is refused; with it, exactly one of Name and Server is
// required, and TokenFile goes only with Server, because a session's token
// comes from the session.
func (f Flags) Check() error {
	if !f.Stdio {
		if f.ReportVersion != "" {
			return errors.New("--report-version goes with --stdio")
		}
		if f.Server == "" {
			return errors.New("missing flags: --server=URL")
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

// Up is `comlink codex --server URL [--name NAME]`. An empty name is
// derived from server as defaultName does, and the ready lines say so.
// Once name and server are valid it makes sure the ssh config at sshConfig includes the session
// config files (adding the line as include does and saying so on stderr;
// the line stays after the session), then registers name with server and
// token, writes the ssh config block whose ProxyCommand runs
// `<exe> codex --stdio --name NAME`, prints the alias on stdout, and holds
// the session until ctx is done. It then removes the session's files and
// returns nil. When the Include line cannot be added no session file is
// written.
func (s *Store) Up(ctx context.Context, name, server, token, exe, sshConfig string, stdout, stderr io.Writer) error {
	if _, err := upstream.Endpoint(server, "codex"); err != nil {
		return err
	}
	derived := name == ""
	if derived {
		var err error
		if name, err = defaultName(server); err != nil {
			return err
		}
	}
	block, err := sshd.ConfigBlock(name, []string{exe, "codex", "--stdio", "--name", name})
	if err != nil {
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
	if derived {
		_, _ = fmt.Fprintf(stdout, "  name:       %s, the first label of the server's host (--name sets another)\n", sess.Name)
	}
	_, _ = fmt.Fprintf(stdout, "  server:     %s\n", sess.Server)
	_, _ = fmt.Fprintf(stdout, "  Codex app:  SSH connection %q (ssh %s)\n", sess.Name, sess.Name)
	_, _ = fmt.Fprintln(stdout, "Press Ctrl-C to stop.")
	<-ctx.Done()
	return sess.Close()
}

// defaultName is the session name for a valid server URL when --name is
// not given: the first DNS label of its host, lowercased. The Include line
// sits at the top of ~/.ssh/config, so the alias applies to every ssh the
// user runs; an IP address or localhost as the alias would capture the
// user's own ssh to it. For those, and for a label that is not a valid
// alias, there is no default and the error asks for --name.
func defaultName(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("server URL: %w", err)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	label, _, _ := strings.Cut(host, ".")
	var why string
	switch _, addrErr := netip.ParseAddr(host); {
	case addrErr == nil:
		why = "it is an IP address"
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		why = "it is localhost"
	case label == "localhost":
		why = `its first label "localhost" would capture your own ssh to localhost`
	case !ValidName(label):
		why = fmt.Sprintf("its first label %q is not a valid alias", label)
	default:
		return label, nil
	}
	return "", fmt.Errorf("cannot name the session after server host %q: %s; pass --name NAME", host, why)
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
