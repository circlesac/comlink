// Command comlink is a front door for coding-agent clients. It runs as an
// ssh ProxyCommand, speaks the server side of SSH on its own stdin/stdout,
// answers the few shell commands the client sends and relays the agent
// session to a WebSocket endpoint. For ACP clients that launch their agent as
// a local subprocess, it bridges stdio to the remote agent's WebSocket.
// `comlink up` registers a named session that both kinds of client reach by
// name for as long as it runs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/circlesac/comlink/internal/acp"
	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/session"
	"github.com/circlesac/comlink/internal/sshd"
	"github.com/circlesac/comlink/internal/upstream"
)

type cli struct {
	Version    kong.VersionFlag `help:"Print the comlink version and exit."`
	Up         upCmd            `cmd:"" name:"up" help:"Make an agent available under a name to the Codex app and ACP clients until stopped."`
	List       listCmd          `cmd:"" name:"list" help:"Print the names that are up."`
	SSHInclude sshIncludeCmd    `cmd:"" name:"ssh-include" help:"Add the Include line for comlink's session config files to the top of ~/.ssh/config."`
	Codex      codexCmd         `cmd:"" name:"codex" help:"Codex desktop app SSH connections."`
	ACP        acpCmd           `cmd:"" name:"acp" help:"Bridge an ACP client's stdin/stdout (newline-delimited JSON-RPC) to a WebSocket agent endpoint."`
}

type codexCmd struct {
	Proxy codexProxyCmd `cmd:"" name:"proxy" help:"Serve one SSH connection on stdin/stdout (the ssh ProxyCommand mode)."`
}

// source is the pair of flags that picks the upstream: a session name or a
// URL, exactly one of them. kong's xor rejects both; resolve rejects neither.
// They are not tagged required, which would print both as mandatory in usage.
type source struct {
	Name      string `xor:"source" placeholder:"NAME" help:"Session registered by 'comlink up' (or give --upstream)."`
	Upstream  string `xor:"source" placeholder:"URL" help:"WebSocket endpoint, ws:// or wss:// (or give --name)."`
	TokenFile string `type:"path" placeholder:"PATH" help:"With --upstream: file whose trimmed contents are the upstream bearer token. Falls back to $COMLINK_TOKEN."`
}

type codexProxyCmd struct {
	source
	ReportVersion string `default:"0.165.0" placeholder:"VERSION" help:"Codex version reported to 'codex --version'."`
}

type acpCmd struct {
	source
}

type upCmd struct {
	Name      string `required:"" placeholder:"NAME" help:"Session name: the Codex app's SSH alias and the --name of 'comlink codex proxy' and 'comlink acp'."`
	Server    string `required:"" placeholder:"URL" help:"comlink server base URL (ws:// or wss://); it serves the Codex app at <server>/codex and ACP at <server>/acp."`
	TokenFile string `type:"path" placeholder:"PATH" help:"File whose trimmed contents are the server's bearer token. Falls back to $COMLINK_TOKEN."`
}

type listCmd struct{}

type sshIncludeCmd struct{}

func main() {
	var c cli
	ctx := kong.Parse(&c,
		kong.Name("comlink"),
		kong.Description("Front door that relays coding-agent clients to WebSocket agent endpoints: SSH for the Codex desktop app, stdio for ACP clients."),
		kong.UsageOnError(),
		kong.Vars{"version": "comlink " + Version},
	)
	ctx.FatalIfErrorf(ctx.Run())
}

// resolve returns the upstream URL and token for the source: the session's
// endpoint for protocol ("codex" or "acp") with the session's token, or the
// URL as given with the token from --token-file or $COMLINK_TOKEN.
func (s *source) resolve(protocol string, logger *log.Logger) (url, token string, err error) {
	if s.Name == "" && s.Upstream == "" {
		return "", "", errors.New("give --name NAME (a session from 'comlink up') or --upstream URL")
	}
	if s.Name == "" {
		token, err := upstream.LoadToken(s.TokenFile, logger)
		return s.Upstream, token, err
	}
	if s.TokenFile != "" {
		return "", "", errors.New("--token-file goes with --upstream; a session's token comes from 'comlink up'")
	}
	store, err := session.Open()
	if err != nil {
		return "", "", err
	}
	return store.Resolve(s.Name, protocol)
}

func (c *codexProxyCmd) Run() error {
	logger := log.New(os.Stderr, "comlink: ", 0)
	url, token, err := c.resolve("codex", logger)
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
		Upstream:      url,
		Token:         token,
		ReportVersion: c.ReportVersion,
		Logger:        logger,
	})
}

func (c *acpCmd) Run() error {
	logger := log.New(os.Stderr, "comlink: ", 0)
	url, token, err := c.resolve("acp", logger)
	if err != nil {
		return err
	}
	bridge := &acp.Bridge{Upstream: url, Token: token, Logger: logger}
	if status := bridge.Run(os.Stdin, os.Stdout); status != 0 {
		os.Exit(status)
	}
	return nil
}

func (c *upCmd) Run() error {
	token, err := upstream.LoadToken(c.TokenFile, log.New(os.Stderr, "comlink: ", 0))
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate comlink executable: %w", err)
	}
	store, err := session.Open()
	if err != nil {
		return err
	}
	sshConfig, err := session.SSHConfigPath()
	if err != nil {
		return err
	}
	// The session ends on SIGINT or SIGTERM; armed before anything is
	// written so a signal during setup still removes the files.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return store.Up(ctx, c.Name, c.Server, token, exe, sshConfig, os.Stdout, os.Stderr)
}

func (c *listCmd) Run() error {
	store, err := session.Open()
	if err != nil {
		return err
	}
	return store.PrintList(os.Stdout, os.Stderr)
}

func (c *sshIncludeCmd) Run() error {
	store, err := session.Open()
	if err != nil {
		return err
	}
	sshConfig, err := session.SSHConfigPath()
	if err != nil {
		return err
	}
	return store.Include(sshConfig, os.Stdout)
}
