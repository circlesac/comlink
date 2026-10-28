// Command comlink is an SSH front door for coding-agent clients. It runs as an
// ssh ProxyCommand, speaks the server side of SSH on its own stdin/stdout,
// answers the few shell commands the client sends and relays the agent
// session to a WebSocket endpoint.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/sshd"
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
	token, err := loadToken(c.TokenFile, logger)
	if err != nil {
		return err
	}
	keyPath, err := sshd.HostKeyPath()
	if err != nil {
		return err
	}
	signer, err := sshd.LoadOrCreateHostKey(keyPath)
	if err != nil {
		return err
	}
	return sshd.Serve(&sshd.StdioConn{In: os.Stdin, Out: os.Stdout}, signer, &codex.Route{
		Upstream:      c.Upstream,
		Token:         token,
		ReportVersion: c.ReportVersion,
		Logger:        logger,
	})
}

// loadToken returns the trimmed contents of path, or $COMLINK_TOKEN when path
// is empty. An empty result means no token: the upstream is dialled without
// an Authorization header. A token file that group or others can read is
// still used, with a warning naming the path (never the token).
func loadToken(path string, logger *log.Logger) (string, error) {
	if path == "" {
		return strings.TrimSpace(os.Getenv("COMLINK_TOKEN")), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		logger.Printf("warning: token file %s is readable by group or others; restrict it with chmod 600", path)
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *sshConfigCmd) Run() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate comlink executable: %w", err)
	}
	block, err := sshd.ConfigBlock(c.Alias, exe, c.Upstream, c.TokenFile)
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, block)
	return err
}
