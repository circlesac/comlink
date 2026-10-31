// Command comlink is a front door for coding-agent clients. For the Codex
// desktop app, `comlink codex --name NAME --server URL` registers NAME as an
// ssh alias for as long as it runs, and ssh runs `comlink codex --stdio` as
// that alias's ProxyCommand: it speaks the server side of SSH on its own
// stdin/stdout, answers the few shell commands the app sends and relays the
// agent session to the server's WebSocket. ACP clients, which launch their
// agent as a local subprocess, run `comlink acp --server URL`, which bridges
// stdio to the server's ACP WebSocket.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/circlesac/comlink/internal/acp"
	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/session"
	"github.com/circlesac/comlink/internal/sshd"
	"github.com/circlesac/comlink/internal/upstream"
)

type cli struct {
	Version kong.VersionFlag `help:"Print the comlink version and exit."`
	Codex   codexCmd         `cmd:"" name:"codex" help:"Codex desktop app: 'comlink codex --name NAME --server URL' makes <server>/codex the SSH host NAME until stopped, adding the Include line for it to ~/.ssh/config if missing; list prints those sessions."`
	ACP     acpCmd           `cmd:"" name:"acp" help:"Bridge an ACP client's stdin/stdout (newline-delimited JSON-RPC) to <server>/acp; the client runs this as its agent command."`
}

// codexCmd runs on its own (kong runs a command that has a Run method even
// when it has subcommands) and owns its flags, so `comlink codex --help`
// lists them. Which flags go together is checked by session.Flags.Check:
// kong cannot say that --name and --server are both required without
// --stdio and exactly one of them with it, and tagging them required would
// make usage claim otherwise. kong also accepts these flags after list,
// which therefore refuses them.
type codexCmd struct {
	Stdio         bool   `group:"codex" help:"Serve one SSH connection on stdin/stdout, reaching the session --name or the server --server. ssh runs this via a Host block's ProxyCommand; you normally never run it yourself."`
	Name          string `group:"codex" placeholder:"NAME" help:"Session name: the SSH alias the Codex app connects to. With --stdio, the running session to reach."`
	Server        string `group:"codex" placeholder:"URL" help:"comlink server base URL (ws:// or wss://); the Codex app reaches <server>/codex."`
	TokenFile     string `group:"codex" type:"path" placeholder:"PATH" help:"File whose trimmed contents are the server's bearer token. Falls back to $COMLINK_TOKEN. Not with --stdio --name: a session's token comes from the session."`
	ReportVersion string `group:"codex" placeholder:"VERSION" help:"With --stdio: Codex version reported to 'codex --version' (default 0.165.0)."`

	List codexListCmd `cmd:"" name:"list" help:"Print the Codex sessions that are up."`
}

func (c *codexCmd) flags() session.Flags {
	return session.Flags{Stdio: c.Stdio, Name: c.Name, Server: c.Server, TokenFile: c.TokenFile, ReportVersion: c.ReportVersion}
}

type codexListCmd struct{}

type acpCmd struct {
	Server    string `required:"" placeholder:"URL" help:"comlink server base URL (ws:// or wss://); dials <server>/acp."`
	TokenFile string `type:"path" placeholder:"PATH" help:"File whose trimmed contents are the server's bearer token. Falls back to $COMLINK_TOKEN."`
}

func main() {
	var c cli
	ctx := kong.Parse(&c,
		kong.Name("comlink"),
		kong.Description("Front door that relays coding-agent clients to a comlink server: SSH for the Codex desktop app, stdio for ACP clients."),
		kong.UsageOnError(),
		kong.Help(help),
		// Without this the command list shows only leaves, which leaves out
		// `comlink codex` itself.
		kong.ConfigureHelp(kong.HelpOptions{NoExpandSubcommands: true}),
		kong.Groups{"codex": "Flags of 'comlink codex' itself (not of list):"},
		kong.Vars{"version": "comlink " + Version},
	)
	ctx.FatalIfErrorf(ctx.Run())
}

// help is kong's help with one correction: kong writes `codex <command>` for
// a command that has subcommands, but `comlink codex` also runs without one.
// Being buffered, it wraps at $COLUMNS or 80 rather than the terminal width.
func help(options kong.HelpOptions, ctx *kong.Context) error {
	var b strings.Builder
	stdout := ctx.Stdout
	ctx.Stdout = &b
	err := kong.DefaultHelpPrinter(options, ctx)
	ctx.Stdout = stdout
	_, werr := io.WriteString(stdout, strings.ReplaceAll(b.String(), "codex <command>", "codex [<command>]"))
	return errors.Join(err, werr)
}

func (c *codexCmd) Run(kctx *kong.Context) error {
	// kong also runs a parent's Run after its subcommand's; list has done
	// its work by then.
	if kctx.Command() != "codex" {
		return nil
	}
	if err := c.flags().Check(); err != nil {
		return err
	}
	if c.Stdio {
		return c.serve()
	}
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

// serve is --stdio: one SSH connection on stdin/stdout, relayed to the
// session's or the server's Codex endpoint.
func (c *codexCmd) serve() error {
	logger := log.New(os.Stderr, "comlink: ", 0)
	url, token, err := c.flags().Endpoint(logger)
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
	reportVersion := c.ReportVersion
	if reportVersion == "" {
		reportVersion = "0.165.0"
	}
	return sshd.Serve(&sshd.StdioConn{In: os.Stdin, Out: os.Stdout}, signer, &codex.Route{
		Upstream:      url,
		Token:         token,
		ReportVersion: reportVersion,
		Logger:        logger,
	})
}

func (c *codexListCmd) Run(parent *codexCmd) error {
	if err := parent.flags().CheckUnset("list"); err != nil {
		return err
	}
	store, err := session.Open()
	if err != nil {
		return err
	}
	return store.PrintList(os.Stdout, os.Stderr)
}

func (c *acpCmd) Run() error {
	logger := log.New(os.Stderr, "comlink: ", 0)
	url, err := upstream.Endpoint(c.Server, "acp")
	if err != nil {
		return err
	}
	token, err := upstream.LoadToken(c.TokenFile, logger)
	if err != nil {
		return err
	}
	bridge := &acp.Bridge{Upstream: url, Token: token, Logger: logger}
	if status := bridge.Run(os.Stdin, os.Stdout); status != 0 {
		os.Exit(status)
	}
	return nil
}
