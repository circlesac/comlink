# comlink

Front door that relays coding-agent clients to WebSocket agent endpoints:
over SSH for the Codex desktop app, over stdio for ACP clients.

A client that only knows how to reach a remote machine over SSH (today the
Codex desktop app's SSH connections) runs `ssh <alias>`. The alias's
`ProxyCommand` starts `comlink codex proxy`, which speaks the server side of SSH on its
own stdin/stdout, answers the few shell commands the client sends and relays
the agent session to a WebSocket endpoint. There is no sshd, no shell and no
listening port. ACP clients, which launch their agent as a local subprocess,
run `comlink acp` instead; it bridges stdio to the remote agent's WebSocket
(see [ACP clients](#acp-clients)). `comlink up --name NAME --server URL`
makes one agent available to both kinds of client under a stable name for
as long as it runs (see [Sessions](#sessions)). The contracts comlink
implements are in [AGENTS.md](AGENTS.md).

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

## Commands

```
comlink up --name NAME --server URL [--token-file PATH]
comlink list
comlink ssh-include
comlink codex proxy (--name NAME | --upstream URL [--token-file PATH]) [--report-version V]
comlink acp (--name NAME | --upstream URL [--token-file PATH])
```

`comlink up` registers a named session and holds it in the foreground;
`comlink list` prints the names that are up; `comlink ssh-include` adds the
one line to `~/.ssh/config` that lets the Codex app find every session (see
[Sessions](#sessions)). The client commands are grouped by client:
`comlink codex proxy` serves one SSH connection on stdin/stdout and is what
the ssh `ProxyCommand` runs; `comlink acp` is the agent command of an ACP
client (see [ACP clients](#acp-clients)). Each takes either `--name`, the
session to reach, or `--upstream`, an endpoint URL with its own token.
`comlink --help`, `comlink codex --help` and `--help` on each command print
their usage; `comlink --version` prints the version.

## Layout

```
main.go                CLI declaration and wiring
internal/sshd/         SSH server, client-agnostic: host key, global and session requests; hands exec to a route; renders an ssh config block around a given ProxyCommand
internal/codex/        Codex app route: command matching and the WebSocket relay
internal/acp/          ACP stdio bridge: stdin lines to text frames and text frames to stdout lines
internal/session/      named sessions: the record, token and ssh config files under the state directory, liveness by file lock, the ~/.ssh/config Include, and the up / list / resolve commands behind main.go
internal/upstream/     upstream WebSocket dial with the bearer token, the read limit and URL redaction in errors; token loading from a file or $COMLINK_TOKEN
test/                  all tests, black-box through the exported API and the built binary; testdata/ holds the captured Codex commands
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

## Sessions

A comlink server serves both protocols under one base URL: the Codex
app-server protocol at `<server>/codex` and ACP at `<server>/acp`. A server
that implements only one of them just rejects the other when a client uses
it. `comlink up` makes that server available under a name, to the Codex app
and to ACP clients, for as long as it runs:

```sh
mkdir -p ~/.config/comlink
(umask 077; printf '%s' "$TOKEN" > ~/.config/comlink/token)
comlink up --name example-agent --server wss://agent.example.com \
  --token-file ~/.config/comlink/token
```

It prints what is available and stays in the foreground:

```
example-agent is up (pid 4242)
  server:       wss://agent.example.com
  Codex app:    SSH connection "example-agent" (ssh example-agent)
  ACP clients:  agent command /usr/local/bin/comlink acp --name example-agent
Press Ctrl-C to stop.
```

Ctrl-C (SIGINT) or SIGTERM ends it with exit 0 and removes everything it
registered. Several names can be up at once, each with its own server. A
name is a run of letters, digits, `-`, `_` and `.`; it is the ssh alias and
part of the file names, so anything else is refused. The token comes from
`--token-file`, else `$COMLINK_TOKEN`, else the server is dialled without
an `Authorization` header; a token file that group or others can read is
used with a one-line warning naming the file.

Client-side configuration is written once and never edited again:

- **Codex app.** Run `comlink ssh-include` once. It puts
  `Include /home/alice/.local/state/comlink/ssh/*.conf` (the absolute path
  of the session directory) at the top of `~/.ssh/config` (creating the file
  with mode 0600 if there is none), so it applies before any `Host` block; the previous content is kept byte for byte below it and
  copied to `~/.ssh/config.comlink-bak` the first time the file changes.
  Running it again changes nothing. Then add `example-agent` as an SSH
  connection in the app. While `up` runs, that directory holds the alias's
  `Host` block:

  ```
  Host example-agent
    User comlink
    ProxyCommand /usr/local/bin/comlink codex proxy --name example-agent
    HostKeyAlias comlink-example-agent
    StrictHostKeyChecking accept-new
  ```

  `up` prints a reminder on stderr when `~/.ssh/config` lacks the Include
  line; it never edits that file itself. Check an alias by hand with
  `ssh example-agent codex --version`.
- **ACP clients.** Configure the agent command as
  `comlink acp --name example-agent`.

`comlink codex proxy --name NAME` and `comlink acp --name NAME` read the
session's server and token and use `<server>/codex` and `<server>/acp`.
When NAME is not up they exit non-zero at once with one stderr line,
`session "NAME" is not up: run 'comlink up --name NAME --server URL' first`,
which the ssh client or the ACP client shows. Both commands also take
`--upstream URL [--token-file PATH]` instead of `--name`, for an endpoint
that is not registered as a session.

`comlink list` prints each name that is up with its pid and server URL
(never the token), one per line, and removes records whose `up` process is
gone.

Everything lives under `$XDG_STATE_HOME/comlink` (default
`~/.local/state/comlink`); `up` exits non-zero when `XDG_STATE_HOME` is
unset and the home directory cannot be determined:

```
run/NAME.json    the record: name, server, the /codex and /acp endpoints, pid (mode 0600)
run/NAME.token   the token, and nothing else (mode 0600; never in the record or the ssh config)
run/NAME.lock    held with flock by the up process for its lifetime
ssh/NAME.conf    the Host block above (mode 0600)
host_ed25519     the host key comlink codex proxy pins (see below)
```

A session is up while its lock is held. The kernel releases the lock when
the `up` process ends, however it ends, so a record left behind by a killed
`up` is stale even if its pid has since been reused; `up` on that name
removes it and continues, and `list` removes it. The pid in the record is
for display. On a build without flock (not unix) liveness falls back to
whether the recorded pid exists.

`comlink codex proxy` generates an ed25519 host key on first run and keeps it at
`$XDG_STATE_HOME/comlink/host_ed25519` so `StrictHostKeyChecking accept-new`
pins a stable key. The block's `HostKeyAlias comlink-<alias>`
stores that key in `known_hosts` under its own name, so it never collides
with a real host called `<alias>`. `--report-version` sets the Codex version
reported to `codex --version` (default `0.165.0`); the app refuses versions
below its minimum.

## ACP clients

An ACP client such as Zed starts its agent as a local subprocess and speaks
newline-delimited JSON-RPC on the subprocess's stdin/stdout. When the agent
serves ACP over a WebSocket instead (`/acp`, one message per text frame,
`Authorization: Bearer <token>`), configure comlink as the agent command,
by session name or by endpoint:

```sh
comlink acp --name example-agent
comlink acp --upstream wss://agent.example.com/acp \
  --token-file ~/.config/comlink/token
```

The client's agent command is `comlink` with those arguments; comlink
needs no other setup (no host key, no ssh config).

With `--upstream` the token rules are the same as for `comlink codex proxy`: `--token-file`, else
`$COMLINK_TOKEN`, else no `Authorization` header, with a warning for a token
file others can read. comlink does not parse the messages: each stdin line
goes to the upstream as one text frame and each text frame comes back as one
stdout line. A binary frame, or a text frame that contains a newline (`\n`
or `\r`, which the client would read as several lines), is not written; comlink
closes the upstream with status 1003, logs one line and exits 1. stdout
carries nothing else; diagnostics go to stderr. When the
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
the exported API or the built binary: `internal/` has no tests and nothing
is exported for testing. Most tests run an SSH client against `sshd.Serve`
with the Codex route and look at what the client sees: stdout bytes, stderr,
the exit status and the route's log. Tests that need the binary build it
once per run (`buildComlink`) and run it with a temporary `HOME` and
`XDG_STATE_HOME`.

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
- `config_test.go`: the ssh config block for a given `ProxyCommand`
  argument list, its shell quoting, that `sh` splits the `ProxyCommand` line
  back into the original arguments, and the accepted and refused aliases.
- `hostkey_test.go`: host key creation under concurrent first runs, the
  state path (an error without `XDG_STATE_HOME` or a home directory), and
  the error cases.
- `acp_test.go`: the ACP bridge against a WebSocket upstream that checks
  the bearer token. Line-to-frame and frame-to-line mapping (`\r`, empty
  lines, a line that is not JSON, a last line without a newline), many
  messages in order with one of several MiB, stdin EOF closing with 1000 and
  exit 0, upstream closes (1000 and 1001 exit 0, an error status or a drop
  exit 1), a wrong token, an unreachable or unparsable upstream (one
  redacted log line, exit 1), a binary frame or a text frame holding `\n` or
  `\r` (1003, nothing of it on stdout), an oversized line
  (1009), a failed stdout write (1001); and the built binary run as an
  agent subprocess with the token from a file or `$COMLINK_TOKEN`.
- `openssh_test.go`: when an `ssh` binary is on `PATH`, the version probe,
  a refusal and the proxy relay through real OpenSSH with the built binary
  as its `ProxyCommand`; and the built binary run directly: the token file
  permission warning, a missing token file and the missing home directory.
- `session_test.go`: the session registry in process. The `/codex` and
  `/acp` endpoints for bases with and without a path prefix or trailing
  slash, the name rule, the state directory from `XDG_STATE_HOME` or the
  home directory, `Start`/`Load`/`List`/`Close` with the file modes and the
  token kept out of the record, a refused second start, stale records
  (lock not held) removed by `List` and replaced by `Start` while `Load`
  reports them as not up, `Up` driven to its ready line and ended through
  its context, `Resolve`, `PrintList`, and the `~/.ssh/config` Include:
  created private, prepended with the rest byte-identical and the mode
  kept, backed up once, idempotent, the spellings ssh accepts, and the
  failure cases.
- `token_test.go`: the token from a file or `$COMLINK_TOKEN`, trimmed, the
  permission warning naming the path and a missing file.
- `up_test.go`: the built binary with a temporary `HOME` and
  `XDG_STATE_HOME`. `up` writes the record, token and ssh config with
  private modes and no token outside the token file, prints what is
  available and the ssh-include hint, and removes exactly its files on
  SIGTERM or SIGINT with exit 0; a second `up` on a name that is up is
  refused naming the pid; a killed `up` leaves a stale record that the next
  `up` replaces and `list` removes; `list` output; `ssh-include` creating,
  prepending, backing up once and changing nothing on a second run; `codex
  proxy --name` through real OpenSSH with a config that Includes the session
  directory, answering the version probe and relaying the app-server session
  to `<server>/codex` with the session's token, then failing with the
  not-up line once `up` has stopped; `acp --name` relaying to `<server>/acp`
  and failing fast afterwards; three names up at once, with and without a
  path prefix or trailing slash, each reaching its own server; and the
  refused arguments (bad name or server, both or neither of `--name` and
  `--upstream`, `--token-file` with `--name`).

Coverage is measured across the module from `test/` with `-coverpkg=./...`;
CI fails when the total statement coverage is below 80%. `main.go` is only
exercised through the tests that run the built binary, so it does not count
towards the profile; it stays thin and the command bodies live in
`internal/session` and `internal/upstream`, where the in-process tests reach them. CI runs the same
commands and then builds the binary and runs `--help` and `--version`.

## Tooling

| Package | Version | Release date | Deciding criterion |
| --- | --- | --- | --- |
| `github.com/alecthomas/kong` | v1.16.1 | 2026-08-09 | Chosen over urfave/cli v3 and cobra: the command is a typed struct declaration and help, usage and `--version` are generated from it; its only module requirements are test-only. |
| `golang.org/x/crypto/ssh` | v0.58.0 | 2026-10-09 | The SSH server implementation; the maintained Go SSH library. |
| `github.com/coder/websocket` | v1.8.15 | 2026-06-15 | Chosen over gorilla/websocket, whose last release (v1.5.3) is 2024-06-14, more than twelve months ago. |
| `golangci-lint` | v2.15.0 | 2026-10-16 | Linter in the test gate with the default linter set and `gofmt`. |
