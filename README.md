# comlink

comlink lets the Codex desktop app, the Claude desktop app and ACP clients
such as Zed talk to a coding agent that lives behind a WebSocket endpoint
(for example a Cloudflare Worker), without a machine of its own. The apps
believe they are connecting to an SSH host; the ACP client believes it
started a local agent. comlink plays that host or that agent on your
computer and relays the conversation to the endpoint.

- **Codex app**: `comlink codex --server URL` registers an SSH host the app
  can open; comlink answers the app's setup commands and relays its
  app-server session to `<server>/codex`.
- **Claude app** (Code tab): `comlink claude --server URL` does the same
  for Claude; comlink also stands in for the helper the app would install
  and for Claude Code itself, and runs each chat as an ACP session on
  `<server>/acp`.
- **ACP clients**: `comlink acp --server URL` is the agent command; it
  bridges the client's stdio to `<server>/acp`.

Nothing is installed on a server, no sshd runs, no port is opened: each
connection the app makes is one `comlink … --stdio` process that ssh starts
as the host's `ProxyCommand`.

## Install

macOS or Linux:

```sh
curl -fsSL https://github.com/circlesac/comlink/releases/latest/download/install.sh | sh
```

This puts the latest release in `~/.local/bin` (set `INSTALL_DIR` for
another directory) after checking it against `checksums.txt`. Windows:
download `comlink-windows-<arch>.zip` from the
[releases](https://github.com/circlesac/comlink/releases). Releases cover
macOS, Linux and Windows on `amd64` and `arm64`, with an SBOM and, when
built from a public build, a GitHub build-provenance attestation.

With Go installed, `go install github.com/circlesac/comlink@latest` builds
the latest `main` and `go build -o comlink .` builds a checkout; both report
version `0.0.0`. (The CalVer tags, `v26.11.0`, read as a major version to
Go modules, so `go install` cannot pick a release.)

## Codex app

Start a session and leave it running:

```sh
mkdir -p ~/.config/comlink
(umask 077; printf '%s' "$TOKEN" > ~/.config/comlink/token)
comlink codex --server wss://example-agent.example.com --token-file ~/.config/comlink/token
```

```
example-agent is up (pid 4242)
  name:       example-agent, the first label of the server's host (--name sets another)
  server:     wss://example-agent.example.com
  Codex app:  SSH connection "example-agent" (ssh example-agent)
Press Ctrl-C to stop.
```

Then add `example-agent` as an SSH connection in the Codex app and open a
chat on it. The name is the first label of the server's host; `--name NAME`
picks another. When a name is taken (a session is up, or your own ssh
configuration already has a `Host` line for it) the session takes
`example-agent-2`, `-3`, … and says why on the `name:` line. For a server
reached by IP address or `localhost` the name is `comlink`, so the alias
never captures your own `ssh` to that host.

The first session adds one line to the top of `~/.ssh/config`,
`Include ~/.local/state/comlink/ssh/*.conf`, and says so (the previous file
is backed up next to it once). That line is how ssh and the apps find every
session; it stays, and matches nothing while no session is up. Ctrl-C
ends the session and removes everything else it registered.

`comlink codex list` shows the sessions that are up. To check an alias by
hand: `ssh example-agent codex --version`.

## Claude app

The same, with `claude`:

```sh
comlink claude --server wss://example-agent.example.com --token-file ~/.config/comlink/token
```

```
example-agent is up (pid 4242)
  name:       example-agent, the first label of the server's host (--name sets another)
  server:     wss://example-agent.example.com
  Claude app: SSH connection "example-agent" (ssh example-agent)
Press Ctrl-C to stop.
```

In the Claude app, add `example-agent` as an SSH connection and open a new
session on it. Every chat becomes one ACP session on `<server>/acp`. The
app shows an empty home directory (`/home/comlink`) in its folder picker:
there are no files and no git there, and a plugin the app syncs goes
nowhere; what the agent can do is decided by the endpoint. A Claude and a Codex session on the same server
coexist under two names (`example-agent`, `example-agent-2`), and
`comlink claude list` shows the Claude ones.

Two rules of the app itself, not of comlink: an app configured for an API
gateway only allows SSH hosts named in its `sshHostAllowlist`, and it
refuses remote sessions while that gateway is on localhost. Both settings
are read when the app starts.

## ACP clients

An ACP client (Zed, for example) starts its agent as a subprocess and
speaks to it on stdin/stdout. Configure comlink as that agent command:

```sh
comlink acp --server wss://example-agent.example.com --token-file ~/.config/comlink/token
```

It dials `<server>/acp`, lives as long as the client keeps it, and
registers nothing: no session, no ssh config, no host key. Each line the
client writes goes to the endpoint as one WebSocket text frame and each
frame comes back as one line; comlink does not read the messages.

## Tokens

The token is the bearer token the endpoint expects (`Authorization:
Bearer …`). Give it one of three ways (`--token-file` and `--token-command`
are exclusive):

| Flag | Token |
| --- | --- |
| `--token-file PATH` | the file's contents, trimmed (comlink warns when others can read the file) |
| `--token-command CMD` | the trimmed stdout of CMD, run through `sh -c` (`cmd /C` on Windows) each time comlink dials; for short-lived tokens, such as one a login CLI keeps fresh |
| `$COMLINK_TOKEN` | the variable, when neither flag is given |

With none of them the endpoint is dialled without an `Authorization`
header. A session stores a token in a private file and a token command as
the command (with the directory and `PATH` it was given in), never its
output; the token is never printed, logged or put in an error. A command
that fails, prints nothing or more than one line, or runs longer than 30
seconds is an error and comlink does not dial.

## Commands

```
comlink codex  --server URL [--name NAME] [--token-file PATH | --token-command CMD]
comlink codex  list
comlink claude --server URL [--name NAME] [--token-file PATH | --token-command CMD]
comlink claude list
comlink acp    --server URL [--token-file PATH | --token-command CMD]
```

`--server` is always the endpoint's base URL (`ws://` or `wss://`, with or
without a path prefix); the Codex commands use `<server>/codex` and the
others `<server>/acp`. `--help` on any command prints its usage;
`--version` prints the version.

`comlink codex --stdio` and `comlink claude --stdio` are what ssh runs as
a session's `ProxyCommand`: one SSH connection on stdin/stdout. You never
run them yourself, except with `--server URL` in a hand-written `Host`
block for a server that has no session (`--report-version V` sets the
Codex version `codex --version` reports, default `0.165.0`).

## Where things are

| Path | What |
| --- | --- |
| `~/.ssh/config` | the `Include` line, added once at the top; backup in `~/.ssh/config.comlink-bak` |
| `~/.local/state/comlink/ssh/NAME.conf` | the session's `Host` block (mode 0600) while it runs |
| `~/.local/state/comlink/run/` | the session's record, token or token command and lock (0600) while it runs |
| `~/.local/state/comlink/claude/chats/` | which agent session each Claude app chat uses and its transcript, so a reopened chat shows its history and continues (0600 files) |
| `~/.local/state/comlink/host_ed25519` | the SSH host key comlink pins for every session, made on first use |

`$XDG_STATE_HOME` replaces `~/.local/state`. A session is up while its
process runs; a record a killed session left behind is removed by the
next session on that name or by `list`.

## When something fails

- `session "NAME" is not up: run 'comlink codex --name NAME --server URL'
  first`, shown by the app or `ssh`: the session for that alias is not
  running.
- The app connects but the chat fails at once: comlink could not reach
  the endpoint or the token was refused. The session's terminal shows one
  line naming the endpoint (without its secrets); in the Claude app the
  chat's result line carries the same text.
- The Claude app shows no SSH option or refuses the host: see the two app
  rules under [Claude app](#claude-app).
- `"NAME" and "NAME-2" both have ssh configuration …; pass --name NAME`: a
  pattern in your ssh config (`Host example*`) matches every name comlink
  would try; choose one with `--name`.

## How it works

Every session is one `Host` block whose `ProxyCommand` is
`comlink <client> --stdio --name NAME`. When an app runs `ssh NAME`, that
process speaks the server side of SSH on its own stdin/stdout for that one
connection, with a pinned host key. For the Codex app it answers the
app's probe, `codex --version` and setup commands without running
anything, then terminates the WebSocket the app opens over the channel and
relays its messages to `<server>/codex`. For the Claude app it answers the
app's probe from a virtual home directory, serves the `sftp` subsystem and
the helper's JSON-RPC bridge on that directory, and when the app spawns
Claude Code it runs a stand-in that turns the app's stream-json into ACP
on `<server>/acp`: text and thinking as they stream, tool calls and their
results, the endpoint's permission requests as the app's own permission
prompts. The commands each app sends and the exact answers are the
contracts in [AGENTS.md](AGENTS.md); the session files, the ssh config
handling and the Claude bridge in detail are in
[docs/internals.md](docs/internals.md).

Browser sign-in over a Codex SSH connection also requires the server’s callback-forwarding endpoint. comlink relays the app’s local callback on port 1455; other destinations are refused. No additional flag or local service is required.

## Development

```sh
gofmt -l .
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
go vet ./...
go test -race -coverpkg=./... -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
```

One module, one binary. `main.go` declares the CLI; `internal/sshd` is the
SSH server (client-agnostic), `internal/codex` and `internal/claude` the
two app routes, `internal/acp` the stdio bridge, `internal/session` the
session registry and `internal/upstream` the endpoint dial and token
loading. All tests are in `test/`, black-box through the exported API and
the built binary, with the apps' captured commands under `test/testdata/`;
CI requires 80% statement coverage and runs on GitHub-hosted runners with
the latest stable Go. [docs/internals.md](docs/internals.md) lists what
each test file covers and how to refresh the fixtures from a real app.

Releases: `scripts/release.sh` from a clean `main` computes the next CalVer
version, builds every platform into `dist/` with SBOM and checksums and
publishes the GitHub Release; the `release` workflow runs it on dispatch
after the tests and `govulncheck`, attesting the archives.

## License

[FSL-1.1-MIT](LICENSE.md): you may read, use and modify the source, but not
offer it as a competing product or service. Each version becomes MIT two
years after it is released.

## Tooling

| Package | Version | Release date | Deciding criterion |
| --- | --- | --- | --- |
| `github.com/alecthomas/kong` | v1.16.1 | 2026-08-09 | Chosen over urfave/cli v3 and cobra: the command is a typed struct declaration and help, usage and `--version` are generated from it; its only module requirements are test-only. |
| `golang.org/x/crypto/ssh` | v0.58.0 | 2026-10-09 | The SSH server implementation; the maintained Go SSH library. |
| `github.com/coder/websocket` | v1.8.15 | 2026-06-15 | Chosen over gorilla/websocket, whose last release (v1.5.3) is 2024-06-14, more than twelve months ago. |
| `golangci-lint` | v2.15.0 | 2026-10-16 | Linter in the test gate with the default linter set and `gofmt`. |
| `golang.org/x/vuln` (`govulncheck`) | v1.9.0 | 2026-10-11 | The Go team's vulnerability checker; reports only vulnerabilities in code comlink actually calls. |
| `github.com/CycloneDX/cyclonedx-gomod` | v1.12.0 | 2026-08-22 | Builds the release SBOM from the Go module graph in the CycloneDX format. |
| `@circlesac/oneup` | latest | — | The Circles CalVer calculator the release policy requires. |
