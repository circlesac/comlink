# comlink

SSH front door that relays coding-agent clients to WebSocket agent endpoints.

A client that only knows how to reach a remote machine over SSH (today the
Codex desktop app's SSH connections) runs `ssh <alias>`. The alias's
`ProxyCommand` starts `comlink`, which speaks the server side of SSH on its
own stdin/stdout, answers the few shell commands the client sends and relays
the agent session to a WebSocket endpoint. There is no sshd, no shell and no
listening port. ACP clients, which launch their agent as a local subprocess,
run `comlink acp` instead; it bridges stdio to the remote agent's WebSocket
(see [ACP clients](#acp-clients)). The contracts comlink implements are in
[AGENTS.md](AGENTS.md).

## Boundaries

- comlink does not implement any agent protocol. It recognises the command
  that opens an agent session and moves WebSocket messages between the SSH
  channel (or, for ACP, stdio) and the upstream endpoint. The protocol lives
  in the endpoint.
- Each supported client is a route. The Codex app is the first route; ACP
  clients are the second, through `comlink acp`.
- Commands are matched against what the client actually sends; nothing is
  ever executed. An unrecognised command is logged to stderr and exits 127.
- The upstream token is a secret: it is read from a file or the environment
  and never printed, logged or placed in an error.

## Layout

```
main.go                CLI declaration and wiring
internal/sshd/         SSH server: host key, global and session requests; hands exec to a route; the ssh-config block
internal/codex/        Codex app route: command matching and the WebSocket relay
internal/acp/          ACP stdio bridge: stdin lines to text frames and text frames to stdout lines
internal/upstream/     upstream WebSocket dial with the bearer token, the read limit and URL redaction in errors
test/                  all tests, black-box through the exported API; testdata/ holds the captured Codex commands
```

## Install

Build from a checkout:

```sh
go build -o comlink .
```

Alternatively, since the repository is private, tell Go not to use the
module proxy for it and install directly:

```sh
GOPRIVATE=github.com/circlesac go install github.com/circlesac/comlink@latest
```

This needs `git` to be able to authenticate to GitHub (for example through
`gh auth setup-git` or an SSH key with a `url.<ssh>.insteadOf` rewrite),
because `go install` fetches the module with git.

## Setup

Put the upstream bearer token in a file only you can read, then print the
ssh config block and append it to `~/.ssh/config`:

```sh
mkdir -p ~/.config/comlink
(umask 077; printf '%s' "$TOKEN" > ~/.config/comlink/token)
comlink ssh-config --alias example-agent \
  --upstream wss://agent.example.com/codex \
  --token-file ~/.config/comlink/token >> ~/.ssh/config
```

The block looks like this (the token file is referenced by path only):

```
Host example-agent
  User comlink
  ProxyCommand /usr/local/bin/comlink stdio --upstream wss://agent.example.com/codex --token-file /home/alice/.config/comlink/token
  HostKeyAlias comlink-example-agent
  StrictHostKeyChecking accept-new
```

The alias may contain only letters, digits, `-`, `_` and `.`; `ssh-config`
refuses anything else (spaces, newlines, the ssh patterns `*`, `?` and `!`)
with a non-zero exit and prints nothing.

Then add `example-agent` as an SSH connection in the Codex app. Without
`--token-file`, `comlink stdio` reads the token from `$COMLINK_TOKEN`; with
neither, the upstream is dialled without an `Authorization` header. A token
file that group or others can read is still used, but `comlink stdio` prints
a one-line warning naming the file (never the token) to stderr.

Check the alias by hand:

```sh
ssh example-agent codex --version
```

`comlink stdio` generates an ed25519 host key on first run and keeps it at
`$XDG_STATE_HOME/comlink/host_ed25519` (default
`~/.local/state/comlink/host_ed25519`) so `StrictHostKeyChecking accept-new`
pins a stable key. When `XDG_STATE_HOME` is unset and the home directory
cannot be determined, `comlink stdio` exits non-zero instead of keeping the
key in the current directory. The block's `HostKeyAlias comlink-<alias>`
stores that key in `known_hosts` under its own name, so it never collides
with a real host called `<alias>`. `--report-version` sets the Codex version
reported to `codex --version` (default `0.165.0`); the app refuses versions
below its minimum.

## ACP clients

An ACP client such as Zed starts its agent as a local subprocess and speaks
newline-delimited JSON-RPC on the subprocess's stdin/stdout. When the agent
serves ACP over a WebSocket instead (`/acp`, one message per text frame,
`Authorization: Bearer <token>`), configure comlink as the agent command:

```sh
comlink acp --upstream wss://agent.example.com/acp \
  --token-file ~/.config/comlink/token
```

The client's agent command is `comlink` with those arguments; comlink
needs no other setup (no host key, no ssh config).

The token rules are the same as for `comlink stdio`: `--token-file`, else
`$COMLINK_TOKEN`, else no `Authorization` header, with a warning for a token
file others can read. comlink does not parse the messages: each stdin line
goes to the upstream as one text frame and each text frame comes back as one
stdout line. stdout carries nothing else; diagnostics go to stderr. When the
client closes stdin, comlink closes the upstream with status 1000 and exits
0; when the upstream closes, comlink exits 0 for a normal close and 1
otherwise. An upstream that cannot be reached or refuses the token is one
stderr line naming the URL without its query or userinfo, and exit 1.

## Refreshing the Codex fixtures

`test/testdata/codex-app-proxy.txt` is the exec string the Codex app
sends to open the app-server proxy, captured from a real client.
`codex-app-probe.txt`, `codex-app-version.txt`, `codex-app-bootstrap.txt` and
`codex-app-kill.txt` next to it are the exec strings for the other payloads,
each wrapped in the same launcher. `codex-app-unknown.txt` is the same
launcher around a payload the app never sends (`ls -la /`); the refusal tests
use it. The tests parse them and run the relay against the proxy one. When
the app changes, record them again from a machine
the app can reach over SSH with a real sshd:

1. Add a `ForceCommand` wrapper to the sshd `Match` block for the recording
   user that appends `$SSH_ORIGINAL_COMMAND` to a file, one command per line,
   for example `ForceCommand sh -c 'printf "%s\n" "$SSH_ORIGINAL_COMMAND" >> ~/commands.log; exec /bin/sh -c "$SSH_ORIGINAL_COMMAND"'`.
2. Connect the app to that host and let it run through its probes until it
   starts the proxy session.
3. Copy the proxy line into `test/testdata/codex-app-proxy.txt` and
   the other lines into the four other files, or, if only the launcher
   changed, regenerate them by wrapping each payload in the new launcher
   (`codex-app-unknown.txt` is always regenerated this way). Then update the
   contract in `AGENTS.md`.

## Tests

```sh
gofmt -l .
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
go vet ./...
go test -race -coverpkg=./... -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
```

All tests live in `test/`, one package with a file per subject, and use only
the exported API: `internal/` has no tests and nothing is exported for
testing. Most tests run an SSH client against `sshd.Serve` with the Codex
route and look at what the client sees: stdout bytes, stderr, the exit status
and the route's log.

- `codex_test.go`: the Codex commands, from the fixtures and crafted. The
  version probe, the acknowledged setup commands, refusals (exit 127 naming
  the unwrapped payload), the `printf '%b'` marker written to stdout first,
  how the launcher wrapper is split off, and which answer wins when a
  payload matches several commands.
- `relay_test.go`: the proxy route. Messages relayed to an upstream that
  checks the bearer token, the `Authorization` header absent without a
  token, exit 1 with a redacted log line when the upstream cannot be reached
  or refuses the token, the clean and unclean close cases on each side
  (1001 to the upstream when the client goes away without a close frame),
  and a write blocked on a client that stopped reading being released when
  the client's connection closes.
- `sshd_test.go`: the server with a stand-in route. The exec handoff,
  one exec per session, malformed requests, the port-forward replies, the
  refused shell, pty, subsystem and channel types, and `StdioConn`.
- `config_test.go`: the `ssh-config` block, its shell quoting, that `sh`
  splits the `ProxyCommand` line back into the original arguments, and the
  accepted and refused aliases.
- `hostkey_test.go`: host key creation under concurrent first runs, the
  state path (an error without `XDG_STATE_HOME` or a home directory), and
  the error cases.
- `acp_test.go`: the ACP bridge against a WebSocket upstream that checks
  the bearer token. Line-to-frame and frame-to-line mapping (`\r`, empty
  lines, a line that is not JSON, a last line without a newline), many
  messages in order with one of several MiB, stdin EOF closing with 1000 and
  exit 0, upstream closes (1000 and 1001 exit 0, an error status or a drop
  exit 1), a wrong token, an unreachable or unparsable upstream (one
  redacted log line, exit 1), a binary frame (1003), an oversized line
  (1009), a failed stdout write (1001); and the built binary run as an
  agent subprocess with the token from a file or `$COMLINK_TOKEN`.
- `openssh_test.go`: when an `ssh` binary is on `PATH`, the version probe,
  a refusal and the proxy relay through real OpenSSH with the built binary
  as its `ProxyCommand`; and the built binary run directly: the token file
  permission warning, a missing token file, the missing home directory and a
  refused alias.

Coverage is measured across the module from `test/` with `-coverpkg=./...`;
CI fails when the total statement coverage is below 80%. `main.go` is only
exercised through the tests in `openssh_test.go` and `acp_test.go` that run
the built binary, so it does not count towards the profile. CI runs the same
commands and then builds the binary and runs `--help` and `--version`.

## Tooling

| Package | Version | Release date | Deciding criterion |
| --- | --- | --- | --- |
| `github.com/alecthomas/kong` | v1.16.1 | 2026-08-09 | Chosen over urfave/cli v3 and cobra: the command is a typed struct declaration and help, usage and `--version` are generated from it; its only module requirements are test-only. |
| `golang.org/x/crypto/ssh` | v0.58.0 | 2026-10-09 | The SSH server implementation; the maintained Go SSH library. |
| `github.com/coder/websocket` | v1.8.15 | 2026-06-15 | Chosen over gorilla/websocket, whose last release (v1.5.3) is 2024-06-14, more than twelve months ago. |
| `golangci-lint` | v2.15.0 | 2026-10-16 | Linter in the test gate with the default linter set and `gofmt`. |
