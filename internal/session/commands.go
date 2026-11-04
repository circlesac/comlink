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
	TokenCommand  string
	ReportVersion string
}

// Check rejects the combinations `comlink codex` refuses. Without Stdio,
// Server is required, Name is optional (Up derives it from Server) and
// ReportVersion is refused; with it, exactly one of Name and Server is
// required, and TokenFile and TokenCommand go only with Server, because a
// session's token comes from the session. TokenFile and TokenCommand are
// exclusive.
func (f Flags) Check() error {
	if f.TokenFile != "" && f.TokenCommand != "" {
		return errors.New("--token-file and --token-command are exclusive")
	}
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
	case f.Name != "" && f.TokenCommand != "":
		return errors.New("--token-command goes with --server; a session's token comes from 'comlink codex --name NAME'")
	}
	return nil
}

// CheckUnset rejects any of the flags given to the subcommand sub of
// `comlink codex` (list), which kong lets inherit them.
func (f Flags) CheckUnset(sub string) error {
	if f != (Flags{}) {
		return fmt.Errorf("'comlink codex %s' takes no flags; --stdio, --name, --server, --token-file, --token-command and --report-version go with 'comlink codex' itself", sub)
	}
	return nil
}

// Endpoint returns the Codex endpoint and token that `comlink codex --stdio`
// relays to: the running session Name's, or <Server>/codex with the token
// from TokenCommand, TokenFile or $COMLINK_TOKEN (warnings go to logger). A
// token command runs here, once per connection. The state directory is
// opened only for Name.
func (f Flags) Endpoint(logger *log.Logger) (endpoint, token string, err error) {
	if f.Name == "" {
		if endpoint, err = upstream.Endpoint(f.Server, "codex"); err != nil {
			return "", "", err
		}
		cred, err := upstream.LoadCredential(f.TokenFile, f.TokenCommand, logger)
		if err != nil {
			return "", "", err
		}
		token, err = cred.Bearer()
		return endpoint, token, err
	}
	store, err := Open()
	if err != nil {
		return "", "", err
	}
	return store.Resolve(f.Name)
}

// Up is `comlink codex --server URL [--name NAME]`. An explicit name is
// used as is: Start refuses it when it is up, and when the ssh config at
// sshConfig already applies to it (a Host line of the user's matches it,
// see sshConfigured) Up warns on stderr once the session is up. Without a
// name Up tries the base defaultName gives, then base-2, base-3 and so on,
// skipping a candidate the user's ssh configuration applies to and one
// Start reports as up (also when another session took it after the check),
// and the ready lines say which name it took and why. Only finitely many
// sessions can be up, so the only endless run of skips is ssh configuration
// that matches every suffix (a pattern such as `Host padawan*`): two
// candidates in a row with ssh configuration end it with an error asking
// for --name, before any session file is written. The first candidate
// without ssh configuration makes sure the ssh config at sshConfig includes
// the session config files (adding the line as include does and saying so
// on stderr; the line stays after the session), so when that candidate is
// up and the next two are configured the line is there by the time Up
// refuses. Up then registers name with server and token, writes the ssh
// config block whose ProxyCommand runs `<exe> codex --stdio --name NAME`,
// prints the alias and the server (redacted; the record keeps it whole) on
// stdout, and holds the session until ctx is done. It
// then removes the session's files and returns nil. When the Include line
// cannot be added no session file is written.
func (s *Store) Up(ctx context.Context, name, server string, cred upstream.Credential, exe, sshConfig string, stdout, stderr io.Writer) error {
	if _, err := upstream.Endpoint(server, "codex"); err != nil {
		return err
	}
	derived := name == ""
	base := name
	var skipped []string // why the derived name is not the first label of the host
	if derived {
		var why string
		if base, why = defaultName(server); why != "" {
			skipped = append(skipped, why)
		}
	}
	included := false
	configured := "" // the previous candidate, when ssh configuration applies to it
	warning := ""    // why an explicit name's session shadows the user's Host line
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		block, err := sshd.ConfigBlock(name, []string{exe, "codex", "--stdio", "--name", name})
		if err != nil {
			return err
		}
		if where := s.sshConfigured(sshConfig, name); where != "" {
			why := fmt.Sprintf("%q already has ssh configuration (the Host line at %s matches it)", name, where)
			if !derived {
				warning = why
			} else if configured != "" {
				return fmt.Errorf("%q and %q both have ssh configuration, so a pattern in it likely matches every %s-N; pass --name NAME", configured, name, base)
			} else {
				configured = name
				skipped = append(skipped, name+" has ssh configuration")
				continue
			}
		}
		configured = ""
		if !included {
			if err := s.addInclude(sshConfig, stderr); err != nil {
				return err
			}
			included = true
		}
		sess, err := s.Start(name, server, cred, block)
		var up *upError
		if derived && errors.As(err, &up) {
			skipped = append(skipped, name+" is already up")
			continue
		}
		if err != nil {
			return err
		}
		if sess.Replaced != nil {
			_, _ = fmt.Fprintf(stderr, "comlink: removed stale session %q (pid %d is gone)\n", sess.Replaced.Name, sess.Replaced.PID)
		}
		if warning != "" {
			_, _ = fmt.Fprintf(stderr, "comlink: warning: %s; this session's Host block takes precedence while it runs\n", warning)
		}
		_, _ = fmt.Fprintf(stdout, "%s is up (pid %d)\n", sess.Name, sess.PID)
		switch {
		case derived && len(skipped) == 0:
			_, _ = fmt.Fprintf(stdout, "  name:       %s, the first label of the server's host (--name sets another)\n", sess.Name)
		case derived:
			_, _ = fmt.Fprintf(stdout, "  name:       %s (%s; --name sets another)\n", sess.Name, strings.Join(skipped, ", "))
		}
		_, _ = fmt.Fprintf(stdout, "  server:     %s\n", upstream.Redact(sess.Server))
		_, _ = fmt.Fprintf(stdout, "  Codex app:  SSH connection %q (ssh %s)\n", sess.Name, sess.Name)
		_, _ = fmt.Fprintln(stdout, "Press Ctrl-C to stop.")
		<-ctx.Done()
		return sess.Close()
	}
}

// defaultName is the base of the session name for a valid server URL when
// --name is not given: the first DNS label of its host, lowercased. The
// Include line sits at the top of ~/.ssh/config, so the alias applies to
// every ssh the user runs; an IP address or localhost as the alias would
// capture the user's own ssh to it. For those, and for a label that is not
// a valid alias, the base is comlink and why says the reason.
func defaultName(server string) (name, why string) {
	u, _ := url.Parse(server) // upstream.Endpoint has parsed it
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	label, _, _ := strings.Cut(host, ".")
	switch _, addrErr := netip.ParseAddr(host); {
	case addrErr == nil:
		why = fmt.Sprintf("the server host %s is an IP address", host)
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		why = fmt.Sprintf("the server host %s is localhost", host)
	case label == "localhost":
		why = fmt.Sprintf("the first label %q of the server host %s would capture your own ssh to localhost", label, host)
	case !ValidName(label):
		why = fmt.Sprintf("the first label %q of the server host %s is not a valid alias", label, host)
	default:
		return label, ""
	}
	return "comlink", why
}

// Resolve returns the Codex endpoint and the token of the session called
// name, running the session's token command when it has one. A name that is
// not up is a *NotUpError.
func (s *Store) Resolve(name string) (endpoint, token string, err error) {
	record, cred, err := s.Load(name)
	if err != nil {
		return "", "", err
	}
	// A session's token command runs for every connection, so each one gets
	// a fresh token.
	if token, err = cred.Bearer(); err != nil {
		return "", "", err
	}
	return record.Codex, token, nil
}

// PrintList prints one line per session that is up (name, pid and server
// without its userinfo or query; never the token) on stdout, reports removed stale records on stderr and
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
		_, _ = fmt.Fprintf(stdout, "%s\tup\tpid %d\t%s\n", r.Name, r.PID, upstream.Redact(r.Server))
	}
	return nil
}
