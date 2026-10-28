# comlink

SSH front door that relays coding-agent clients to WebSocket agent endpoints.

A client that only knows how to reach a remote machine over SSH (today the
Codex desktop app's SSH connections) runs `ssh <alias>`. The alias's
`ProxyCommand` starts `comlink`, which speaks the server side of SSH on its
own stdin/stdout, answers the few shell commands the client sends and relays
the agent session to a WebSocket endpoint. There is no sshd, no shell and no
listening port. The contract comlink implements is in [AGENTS.md](AGENTS.md).

## Boundaries

- comlink does not implement any agent protocol. It recognises the command
  that opens an agent session and moves WebSocket messages between the SSH
  channel and the upstream endpoint. The protocol lives in the endpoint.
- Each supported client is a route. The Codex app is the first route.
- Commands are matched against what the client actually sends; nothing is
  ever executed. An unrecognised command is logged to stderr and exits 127.
- The upstream token is a secret: it is read from a file or the environment
  and never printed, logged or placed in an error.

## Layout

```
main.go                CLI declaration and wiring
internal/sshd/         SSH server: host key, global and session requests; hands exec to a route; the ssh-config block
internal/codex/        Codex app route: command matching and the WebSocket relay
test/                  end-to-end and real-OpenSSH tests
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

Then add `example-agent` as an SSH connection in the Codex app. Without
`--token-file`, `comlink stdio` reads the token from `$COMLINK_TOKEN`; with
neither, the upstream is dialled without an `Authorization` header.

Check the alias by hand:

```sh
ssh example-agent codex --version
```

`comlink stdio` generates an ed25519 host key on first run and keeps it at
`$XDG_STATE_HOME/comlink/host_ed25519` (default
`~/.local/state/comlink/host_ed25519`) so `StrictHostKeyChecking accept-new`
pins a stable key. The block's `HostKeyAlias comlink-<alias>` stores that key
in `known_hosts` under its own name, so it never collides with a real host
called `<alias>`. `--report-version` sets the Codex version reported to
`codex --version` (default `0.165.0`); the app refuses versions below its
minimum.

## Refreshing the Codex fixtures

`internal/codex/testdata/codex-app-proxy.txt` is the exec string the Codex app
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
3. Copy the proxy line into `internal/codex/testdata/codex-app-proxy.txt` and
   the other lines into the four other files, or, if only the launcher
   changed, regenerate them by wrapping each payload in the new launcher
   (`codex-app-unknown.txt` is always regenerated this way). Then update the
   contract in `AGENTS.md`.

## Tests

```sh
gofmt -l .
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
go vet ./...
go test -race ./...
```

Unit tests sit next to their package: shell quoting, command parsing against
the fixtures, and SSH request handling with a fake route. The integration
tests in `test/` cover an SSH client driving the server end to end
(port-forward request, version probe, refusals, and the proxy route relayed
to an upstream that checks the bearer token) and, when an `ssh` binary is on
`PATH`, the same flows through real OpenSSH with comlink as its
`ProxyCommand`. CI runs the same commands and then builds the binary and runs
`--help` and `--version`.

## Tooling

| Package | Version | Release date | Deciding criterion |
| --- | --- | --- | --- |
| `github.com/alecthomas/kong` | v1.16.1 | 2026-08-09 | Chosen over urfave/cli v3 and cobra: the command is a typed struct declaration and help, usage and `--version` are generated from it; its only module requirements are test-only. |
| `golang.org/x/crypto/ssh` | v0.58.0 | 2026-10-09 | The SSH server implementation; the maintained Go SSH library. |
| `github.com/coder/websocket` | v1.8.15 | 2026-06-15 | Chosen over gorilla/websocket, whose last release (v1.5.3) is 2024-06-14, more than twelve months ago. |
| `golangci-lint` | v2.15.0 | 2026-10-16 | Linter in the test gate with the default linter set and `gofmt`. |
