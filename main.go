// Command comlink is a front door for coding-agent clients. For the Codex
// and Claude desktop apps, `comlink codex --server URL [--name NAME]` and
// `comlink claude --server URL [--name NAME]` register NAME (by default the
// first label of the server's host) as an ssh alias for as long as they
// run, and ssh runs `comlink codex --stdio` or `comlink claude --stdio` as
// that alias's ProxyCommand: it speaks the server side of SSH on its own
// stdin/stdout, answers the shell commands the app sends and relays the
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
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"

	"github.com/circlesac/comlink/internal/acp"
	"github.com/circlesac/comlink/internal/claude"
	"github.com/circlesac/comlink/internal/codex"
	"github.com/circlesac/comlink/internal/session"
	"github.com/circlesac/comlink/internal/sshd"
	"github.com/circlesac/comlink/internal/upstream"
)

type cli struct {
	Version kong.VersionFlag `help:"Print the comlink version and exit."`
	Codex   codexCmd         `cmd:"" name:"codex" help:"Codex desktop app: 'comlink codex --server URL [--name NAME]' makes <server>/codex the SSH host NAME (default: the first label of the server's host) until stopped, adding the Include line for it to ~/.ssh/config if missing; list prints those sessions."`
	Claude  claudeCmd        `cmd:"" name:"claude" help:"Claude desktop app (Code tab): 'comlink claude --server URL [--name NAME]' makes <server>/acp the SSH host NAME (default: the first label of the server's host) until stopped, adding the Include line for it to ~/.ssh/config if missing; list prints those sessions."`
	ACP     acpCmd           `cmd:"" name:"acp" help:"Bridge an ACP client's stdin/stdout (newline-delimited JSON-RPC) to <server>/acp; the client runs this as its agent command."`
}

// codexCmd runs on its own (kong runs a command that has a Run method even
// when it has subcommands) and owns its flags, so `comlink codex --help`
// lists them. Which flags go together is checked by session.Flags.Check:
// kong cannot say that --server is required without --stdio (--name
// optional) but exactly one of --name and --server is required with it, and
// tagging either required would make usage claim otherwise. kong also accepts these flags after list,
// which therefore refuses them.
type codexCmd struct {
	Stdio         bool   `group:"codex" help:"Serve one SSH connection on stdin/stdout, reaching the session --name or the server --server. ssh runs this via a Host block's ProxyCommand; you normally never run it yourself."`
	Name          string `group:"codex" placeholder:"NAME" help:"Session name: the SSH alias the Codex app connects to. Defaults to the first label of the server's host (wss://padawan.example.com gives padawan; comlink for an IP address or localhost), followed by -2, -3, ... when that name is up or already has ssh configuration. With --stdio, the running session to reach."`
	Server        string `group:"codex" placeholder:"URL" help:"comlink server base URL (ws:// or wss://); the Codex app reaches <server>/codex."`
	TokenFile     string `group:"codex" type:"path" placeholder:"PATH" help:"File whose trimmed contents are the server's bearer token. Falls back to $COMLINK_TOKEN. Not with --stdio --name: a session's token comes from the session."`
	TokenCommand  string `group:"codex" placeholder:"COMMAND" help:"Shell command whose trimmed stdout is the server's bearer token, run for every connection (a session stores the command, not a token). Exclusive with --token-file. Not with --stdio --name."`
	ReportVersion string `group:"codex" placeholder:"VERSION" help:"With --stdio: Codex version reported to 'codex --version' (default 0.165.0)."`

	List codexListCmd `cmd:"" name:"list" help:"Print the Codex sessions that are up."`
}

func (c *codexCmd) flags() session.Flags {
	return session.Flags{Client: session.Codex, Stdio: c.Stdio, Name: c.Name, Server: c.Server, TokenFile: c.TokenFile, TokenCommand: c.TokenCommand, ReportVersion: c.ReportVersion}
}

// claudeCmd is codexCmd for the Claude desktop app: the same session and
// --stdio forms, relayed to <server>/acp as ACP sessions.
type claudeCmd struct {
	Stdio        bool   `group:"claude" help:"Serve one SSH connection on stdin/stdout, reaching the session --name or the server --server. ssh runs this via a Host block's ProxyCommand; you normally never run it yourself."`
	Name         string `group:"claude" placeholder:"NAME" help:"Session name: the SSH alias the Claude app connects to. Defaults to the first label of the server's host (wss://padawan.example.com gives padawan; comlink for an IP address or localhost), followed by -2, -3, ... when that name is up or already has ssh configuration. With --stdio, the running session to reach."`
	Server       string `group:"claude" placeholder:"URL" help:"comlink server base URL (ws:// or wss://); each chat the Claude app opens is an ACP session on <server>/acp."`
	TokenFile    string `group:"claude" type:"path" placeholder:"PATH" help:"File whose trimmed contents are the server's bearer token. Falls back to $COMLINK_TOKEN. Not with --stdio --name: a session's token comes from the session."`
	TokenCommand string `group:"claude" placeholder:"COMMAND" help:"Shell command whose trimmed stdout is the server's bearer token, run for every chat (a session stores the command, not a token). Exclusive with --token-file. Not with --stdio --name."`

	List claudeListCmd `cmd:"" name:"list" help:"Print the Claude sessions that are up."`
}

func (c *claudeCmd) flags() session.Flags {
	return session.Flags{Client: session.Claude, Stdio: c.Stdio, Name: c.Name, Server: c.Server, TokenFile: c.TokenFile, TokenCommand: c.TokenCommand}
}

type codexListCmd struct{}

type claudeListCmd struct{}

type acpCmd struct {
	Server       string `required:"" placeholder:"URL" help:"comlink server base URL (ws:// or wss://); dials <server>/acp."`
	TokenFile    string `type:"path" placeholder:"PATH" help:"File whose trimmed contents are the server's bearer token. Falls back to $COMLINK_TOKEN."`
	TokenCommand string `placeholder:"COMMAND" help:"Shell command whose trimmed stdout is the server's bearer token, run when the bridge connects. Exclusive with --token-file."`
}

func main() {
	var c cli
	ctx := kong.Parse(&c,
		kong.Name("comlink"),
		kong.Description("Front door that relays coding-agent clients to a comlink server: SSH for the Codex and Claude desktop apps, stdio for ACP clients."),
		kong.UsageOnError(),
		kong.Help(help),
		// Without this the command list shows only leaves, which leaves out
		// `comlink codex` and `comlink claude` themselves.
		kong.ConfigureHelp(kong.HelpOptions{NoExpandSubcommands: true}),
		kong.Groups{"codex": "Flags of 'comlink codex' itself (not of list):", "claude": "Flags of 'comlink claude' itself (not of list):"},
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
	text := strings.ReplaceAll(b.String(), "codex <command>", "codex [<command>]")
	text = strings.ReplaceAll(text, "claude <command>", "claude [<command>]")
	_, werr := io.WriteString(stdout, text)
	return errors.Join(err, werr)
}

func (c *codexCmd) Run(kctx *kong.Context) error {
	// kong also runs a parent's Run after its subcommand's; list has done
	// its work by then.
	if kctx.Command() != "codex" {
		return nil
	}
	return run(c.flags(), func(url string, cred upstream.Credential, logger *log.Logger) sshd.Route {
		reportVersion := c.ReportVersion
		if reportVersion == "" {
			reportVersion = "0.165.0"
		}
		return &codex.Route{Upstream: url, Credential: cred, ReportVersion: reportVersion, Logger: logger}
	})
}

func (c *claudeCmd) Run(kctx *kong.Context) error {
	if kctx.Command() != "claude" {
		return nil
	}
	return run(c.flags(), func(url string, cred upstream.Credential, logger *log.Logger) sshd.Route {
		r := &claude.Route{Upstream: url, Credential: cred, Logger: logger, Version: Version}
		if store, err := session.Open(); err == nil {
			r.Chats = filepath.Join(store.Dir, "claude", "chats")
		} else {
			logger.Printf("chats will not resume their agent sessions: %v", err)
		}
		return r
	})
}

// run is `comlink <client>`: with --stdio one SSH connection on
// stdin/stdout served by the client's route, otherwise the session.
func run(f session.Flags, route func(url string, cred upstream.Credential, logger *log.Logger) sshd.Route) error {
	if err := f.Check(); err != nil {
		return err
	}
	if f.Stdio {
		return serve(f, route)
	}
	cred, err := upstream.LoadCredential(f.TokenFile, f.TokenCommand, log.New(os.Stderr, "comlink: ", 0))
	if err != nil {
		return err
	}
	// Run a token command once now, so a broken one fails here rather than
	// at the app's first connection; its token is not kept. Not for
	// arguments Up refuses anyway, so a usage error never waits on it.
	if _, err := upstream.Endpoint(f.Server, f.Client.Protocol()); err == nil && (f.Name == "" || session.ValidName(f.Name)) {
		if _, err := cred.Bearer(); err != nil {
			return err
		}
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
	// The session ends on SIGINT, SIGTERM or SIGHUP (its terminal closed);
	// armed before anything is written so a signal during setup still
	// removes the files.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return store.Up(ctx, f.Client, f.Name, f.Server, cred, exe, sshConfig, os.Stdout, os.Stderr)
}

// serve is --stdio: one SSH connection on stdin/stdout, relayed to the
// session's or the server's endpoint.
func serve(f session.Flags, route func(url string, cred upstream.Credential, logger *log.Logger) sshd.Route) error {
	logger := log.New(os.Stderr, "comlink: ", 0)
	url, cred, err := f.Endpoint(logger)
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
	return sshd.Serve(&sshd.StdioConn{In: os.Stdin, Out: os.Stdout}, signer, route(url, cred, logger))
}

func (c *codexListCmd) Run(parent *codexCmd) error { return list(parent.flags()) }

func (c *claudeListCmd) Run(parent *claudeCmd) error { return list(parent.flags()) }

// list is `comlink <client> list`, which kong lets inherit the client's
// flags, so it refuses them.
func list(f session.Flags) error {
	if err := f.CheckUnset("list"); err != nil {
		return err
	}
	store, err := session.Open()
	if err != nil {
		return err
	}
	return store.PrintList(f.Client, os.Stdout, os.Stderr)
}

func (c *acpCmd) Run() error {
	logger := log.New(os.Stderr, "comlink: ", 0)
	url, err := upstream.Endpoint(c.Server, "acp")
	if err != nil {
		return err
	}
	cred, err := upstream.LoadCredential(c.TokenFile, c.TokenCommand, logger)
	if err != nil {
		return err
	}
	token, err := cred.Bearer()
	if err != nil {
		return err
	}
	bridge := &acp.Bridge{Upstream: url, Token: token, Logger: logger}
	if status := bridge.Run(os.Stdin, os.Stdout); status != 0 {
		os.Exit(status)
	}
	return nil
}
