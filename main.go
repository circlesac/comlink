// Command comlink is an SSH front door for coding-agent clients. It runs as an
// ssh ProxyCommand, speaks the server side of SSH on its own stdin/stdout,
// answers the few shell commands the client sends and relays the agent
// session to a WebSocket endpoint.
package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"
)

type cli struct {
	Version   kong.VersionFlag `help:"Print the comlink version and exit."`
	Stdio     stdioCmd         `cmd:"" help:"Serve one SSH connection on stdin/stdout (the ssh ProxyCommand mode)."`
	SSHConfig sshConfigCmd     `cmd:"" name:"ssh-config" help:"Print a ~/.ssh/config block that routes an alias through comlink."`
}

type stdioCmd struct {
	Upstream      string `required:"" placeholder:"URL" help:"WebSocket endpoint that receives the agent session (ws:// or wss://)."`
	TokenFile     string `type:"path" placeholder:"PATH" help:"File whose trimmed contents are the upstream bearer token. Falls back to $COMLINK_TOKEN."`
	ReportVersion string `default:"0.165.0" placeholder:"VERSION" help:"Codex version reported to 'codex --version'."`
}

type sshConfigCmd struct {
	Alias     string `required:"" placeholder:"NAME" help:"Host alias to print."`
	Upstream  string `required:"" placeholder:"URL" help:"WebSocket endpoint passed to 'comlink stdio --upstream'."`
	TokenFile string `type:"path" placeholder:"PATH" help:"Token file passed to 'comlink stdio --token-file'."`
}

func main() {
	var c cli
	ctx := kong.Parse(&c,
		kong.Name("comlink"),
		kong.Description("SSH front door that relays coding-agent clients to WebSocket agent endpoints."),
		kong.UsageOnError(),
		kong.Vars{"version": "comlink " + Version},
	)
	ctx.FatalIfErrorf(ctx.Run())
}

func (c *stdioCmd) Run() error {
	logger := log.New(os.Stderr, "comlink: ", 0)
	token, err := loadToken(c.TokenFile)
	if err != nil {
		return err
	}
	signer, err := loadOrCreateHostKey(hostKeyPath())
	if err != nil {
		return err
	}
	return serve(&stdioConn{in: os.Stdin, out: os.Stdout}, options{
		upstream:      c.Upstream,
		token:         token,
		reportVersion: c.ReportVersion,
		hostKey:       signer,
		logger:        logger,
	})
}

// loadToken returns the trimmed contents of path, or $COMLINK_TOKEN when path
// is empty. An empty result means no token: the upstream is dialled without
// an Authorization header.
func loadToken(path string) (string, error) {
	if path == "" {
		return strings.TrimSpace(os.Getenv("COMLINK_TOKEN")), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *sshConfigCmd) Run() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate comlink executable: %w", err)
	}
	_, err = io.WriteString(os.Stdout, sshConfigBlock(c.Alias, exe, c.Upstream, c.TokenFile))
	return err
}

// stdioConn presents a reader/writer pair (the ProxyCommand's stdin and
// stdout) as a net.Conn so the SSH server can run over it.
type stdioConn struct {
	in  io.Reader
	out io.Writer
}

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

func (c *stdioConn) Read(b []byte) (int, error)  { return c.in.Read(b) }
func (c *stdioConn) Write(b []byte) (int, error) { return c.out.Write(b) }
func (c *stdioConn) LocalAddr() net.Addr         { return stdioAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr        { return stdioAddr{} }
func (c *stdioConn) SetDeadline(time.Time) error { return nil }

func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

func (c *stdioConn) Close() error {
	var errs []error
	if cl, ok := c.in.(io.Closer); ok {
		errs = append(errs, cl.Close())
	}
	if cl, ok := c.out.(io.Closer); ok {
		errs = append(errs, cl.Close())
	}
	return errors.Join(errs...)
}
