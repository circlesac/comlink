# comlink

Front door that relays coding-agent clients to WebSocket agent endpoints:
over SSH for the Codex desktop app, over stdio for ACP clients.

A client that only knows how to reach a remote machine over SSH (today the
Codex desktop app's SSH connections) runs `ssh <alias>`. The alias's
`ProxyCommand` starts `comlink codex --stdio`, which speaks the server side of
SSH on its own stdin/stdout, answers the few shell commands the client sends and relays
the agent session to a WebSocket endpoint. There is no sshd, no shell and no
listening port. The Codex app finds hosts through `~/.ssh/config`, so
`comlink codex --server URL` registers a host named after the server (or
`--name NAME`) for as long as it runs (see [Codex app](#codex-app)). ACP clients launch their
agent as a local subprocess when they need it, so they need no
registration: their agent command is `comlink acp --server URL`, which
bridges stdio to the remote agent's WebSocket and ends with the client (see
[ACP clients](#acp-clients)). The contracts comlink implements are in
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
- The upstream token is a secret: it is read from a file, the environment or
  the output of a token command, and never printed, logged or placed in an
  error.

## Commands

```
comlink codex --server URL [--name NAME] [--token-file PATH | --token-command CMD]
comlink codex --stdio (--name NAME | --server URL [--token-file PATH | --token-command CMD]) [--report-version V]
comlink codex list
comlink acp --server URL [--token-file PATH | --token-command CMD]
```

Commands are grouped by client. `comlink codex --server URL [--name NAME]`
registers a Codex session and holds it in the foreground. `comlink codex
--stdio` serves one SSH connection on stdin/stdout; ssh runs it as the
session's `ProxyCommand` and you normally never run it yourself (see
[How the Codex app connects](#how-the-codex-app-connects)). Both are
`comlink codex` itself, with no subcommand; which flags go together is
checked when it runs: without `--stdio`, `--server` is required, `--name`
is optional (the session picks a free name and says which) and
`--report-version` is refused; with it, exactly one of
`--name` and `--server`, and `--token-file` or `--token-command` only with
`--server`.
`comlink codex list` prints the names that are up and does not take the
flags of `comlink codex`. There is no setup command: a session adds the one
line to `~/.ssh/config` that lets the Codex app find every session when it
is missing.
`comlink acp` is the agent command of an ACP client (see
[ACP clients](#acp-clients)). `--server` is always a comlink server's base
URL: the Codex commands use `<server>/codex` and `comlink acp` uses
`<server>/acp`. `comlink --help`, `comlink codex --help` and `--help` on
each command print their usage; `comlink --version` prints the version.

## Layout

```
main.go                CLI declaration and wiring
internal/sshd/         SSH server, client-agnostic: host key, global and session requests; hands exec to a route; renders an ssh config block around a given ProxyCommand
internal/codex/        Codex app route: command matching and the WebSocket relay
internal/acp/          ACP stdio bridge: stdin lines to text frames and text frames to stdout lines
internal/session/      Codex sessions: the record, token and ssh config files under the state directory (each renamed into place whole), liveness by file lock, adding the ~/.ssh/config Include at session start, the static ~/.ssh/config scan of a session name, picking a free name without --name, the flag checks of comlink codex, and the up / list / resolve / --stdio endpoint bodies behind main.go
internal/upstream/     the <server>/codex and <server>/acp endpoints of a base URL; upstream WebSocket dial with the bearer token, the read limit and URL redaction for errors and printed output; token loading from a file or $COMLINK_TOKEN
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

## Codex app

A comlink server serves both protocols under one base URL: the Codex
app-server protocol at `<server>/codex` and ACP at `<server>/acp`. A server
that implements only one of them just rejects the other when a client uses
it. The Codex app only connects to hosts it finds in `~/.ssh/config`, so it
needs a named, registered session. Start one and leave it running:

```sh
mkdir -p ~/.config/comlink
(umask 077; printf '%s' "$TOKEN" > ~/.config/comlink/token)
comlink codex --server wss://example-agent.example.com \
  --token-file ~/.config/comlink/token
```

The session is named after the first label of the server's host,
lowercased: `wss://example-agent.example.com` gives `example-agent`, and
`ws://Worker:8787` gives `worker`. That name is the SSH alias the Codex app
shows. It prints what is available and stays in the foreground:

```
example-agent is up (pid 4242)
  name:       example-agent, the first label of the server's host (--name sets another)
  server:     wss://example-agent.example.com
  Codex app:  SSH connection "example-agent" (ssh example-agent)
Press Ctrl-C to stop.
```

When that name is taken the session picks the next of `example-agent-2`,
`example-agent-3`, ... and says why on the `name:` line, so several sessions
on one server need no flags:

```
example-agent-2 is up (pid 4243)
  name:       example-agent-2 (example-agent is already up; --name sets another)
```

A name is taken when a comlink session with it is up, or when your ssh
configuration already applies to it: the Include line below sits at the top
of `~/.ssh/config`, so while the session ran, `ssh <name>` would get
comlink's block instead of yours. To tell, the session reads
`~/.ssh/config` and the files it Includes (no ssh is run) for a `Host` line
with a pattern that matches the name, with `*` and `?` wildcards and `!`
negation (`(example-agent has ssh configuration; ...)`). Like ssh, it
matches the name exactly as written, case and all, so `Host Agent-Review`
applies to `--name Agent-Review` and `Host EXAMPLE-AGENT` not to
`example-agent` (derived names are lowercase); arguments
are split as ssh splits them, with double or single quotes and backslash
escapes. A `Host` line that matches every name, such as `Host *`, does not
count, so options under it like `ControlPath ~/.ssh/cm/%r@%h:%p` or
`HostName %h.example.com` never make a name look taken. `Include` lines are
followed where ssh would include them for the name (at top level, under a
`Host` line that matches it, `Host *` included, and under a `Match` block)
and resolved the way ssh does (relative to `~/.ssh`, `~` for your home,
globs expanded, missing files skipped, at most 16 deep); comlink's own
session files are skipped. Two things are not
detected: `Match` blocks, which are not evaluated because they can run
commands (`Match exec`), and a short name that reaches a host only through
DNS search domains. When two names in a row both have ssh
configuration, a pattern such as `Host example-agent*` most likely matches
every suffix, so the session stops there, exits non-zero with one line
ending `; pass --name NAME` and writes no session file. The Include line
below may already have been added by then: it is added at the first name
without ssh configuration, so it is there when that name turned out to be
up and the next two are configured.

When the server's host gives no usable alias, the base name is `comlink`
instead: an IP address (`ws://127.0.0.1:8787`, `ws://[::1]:8787`),
`localhost` or `*.localhost`, a first label `localhost`, or one that is not
a valid alias. Such an alias would capture your own `ssh` to that host. The
`name:` line says so, as in `comlink (the server host 127.0.0.1 is an IP
address; --name sets another)`.

Pass `--name NAME` to choose the alias yourself. It is used as is: a name
that is up is refused, and one your ssh configuration already applies to
gets a one-line warning once the session is up:

```sh
comlink codex --server wss://example-agent.example.com --name agent-review \
  --token-file ~/.config/comlink/token
```

```
comlink: warning: "agent-review" already has ssh configuration (the Host line at /home/alice/.ssh/config:4 matches it); this session's Host block takes precedence while it runs
```

Before it registers anything, the session makes sure `~/.ssh/config` has
`Include /home/alice/.local/state/comlink/ssh/*.conf` (the absolute path of
the session directory), the line that lets ssh and the Codex app find every
session. When the line is missing it is put at the top of the file, so it
applies before any `Host` block, creating the file with mode 0600 if there
is none, and the session says so in one line on stderr:

```
comlink: added "Include /home/alice/.local/state/comlink/ssh/*.conf" to the top of /home/alice/.ssh/config; previous content backed up to /home/alice/.ssh/config.comlink-bak
```

The previous content is kept byte for byte below it and copied to
`~/.ssh/config.comlink-bak` the first time the file changes, and the file's
mode is kept. When `~/.ssh/config` is a symbolic link (for example into a
dotfiles checkout), the link is left as it is and the file it finally
points to gets the line; the backup then sits next to that file, as
`<target>.comlink-bak`, and the stderr line names both paths
(`<link> -> <target>`). A link to a file that does not exist yet creates
that file. When the line is already there at top level (before the first
`Host` or `Match` line; under a block ssh applies it only to that block),
in any spelling ssh accepts (the keyword in any case, `=`, quotes, and the
pattern absolute, as `~/.local/state/comlink/ssh/*.conf` or relative to
`~/.ssh`), nothing is written and nothing is printed. The file is rewritten by
renaming a complete copy into place, so ssh never reads half of it; a
hard-linked `~/.ssh/config` is therefore detached from its other links,
which keep the old content (a symlink is followed and kept, as above). When the
line cannot be added (a link loop, a file or backup that cannot be
written), the session exits non-zero with one error line before it creates
any session file. The line stays when the session ends; with no session up
it matches nothing, so it has no effect.

Add `example-agent` as an SSH connection in the app. While the session
runs, the session directory holds the alias's `Host` block:

```
Host example-agent
  User comlink
  ProxyCommand /usr/local/bin/comlink codex --stdio --name example-agent
  HostKeyAlias comlink-example-agent
  StrictHostKeyChecking accept-new
```

Ctrl-C (SIGINT), SIGTERM or SIGHUP (closing its terminal) ends it with
exit 0 and removes everything it
registered (the Include line in `~/.ssh/config` stays). Several names can
be up at once, each with its own server or sharing one. A name is a run of letters,
digits, `-`, `_` and `.` that does not start with `-`; it is the ssh alias
(which `ssh` would take for an option if it started with `-`) and part of
the file names, so anything else is refused. The server URL is printed
without its userinfo, query or fragment, here and in `comlink codex list`,
in case they hold a secret. The token comes from `--token-file`, or from
`--token-command`, else `$COMLINK_TOKEN`, else the server is dialled without
an `Authorization` header; a token file that group or others can read is
used with a one-line warning naming the file. Check an alias by hand with
`ssh example-agent codex --version`.

### Token commands

`--token-command CMD` (exclusive with `--token-file`) is for short-lived
tokens, such as an OAuth access token a login CLI keeps fresh. CMD runs
through `sh -c` (`cmd /C` on Windows, with the command line passed as is)
with no stdin and its stderr passed through, and its trimmed stdout is the
token. It runs in the working directory and with the `PATH` that comlink was
started with; a Codex session records both with the command, so the
ProxyCommand that the Codex app's `ssh` starts, which has the app's
environment rather than your shell's, runs it the same way. It runs each
time comlink dials: once when `comlink acp` starts; for a Codex session once
when the session starts (so a broken command fails there, after the name and
server are checked) and again each time the app opens its app-server
session, never for the app's probe, version and setup commands, which do not
reach the server. A session therefore stores the command rather than a
token. A command that exits non-zero, runs longer than 30 seconds (its whole
process group is then killed), prints nothing, prints more than one line or
more than 64 KiB is an error: comlink does not dial, and the error never
carries the command's output. `comlink acp` and the session start exit 1;
in the Codex app the app-server session fails with one log line, as for an
upstream that cannot be reached:

```sh
comlink codex --server wss://agent.example.com --token-command 'example-cli auth token'
```

### How the Codex app connects

The app never talks to comlink directly. It runs `ssh example-agent`, and
ssh, finding the alias's `Host` block through the Include, starts the block's
`ProxyCommand`, `comlink codex --stdio --name example-agent`, and speaks SSH
over that process's stdin and stdout instead of a TCP connection. That
process is the whole SSH server for one connection: it answers the few
commands the app sends (the `codex` probe, `codex --version`, the app-server
bootstrap and `pkill`) without running anything, and when the app opens its
app-server session it relays that session's WebSocket messages to the
session's `<server>/codex` with the session's token. It exits when ssh
closes the connection, so each connection the app opens starts its own.

`--stdio --name NAME` reads the session's server and token. When NAME is
not up it exits non-zero at once with one stderr line, `session "NAME" is
not up: run 'comlink codex --name NAME --server URL' first`, which ssh
shows. `--stdio --server URL [--token-file PATH | --token-command CMD]` takes the place of
`--name` for a server that is not registered as a session (with a
hand-written `ProxyCommand`); it dials `<server>/codex`.

`comlink codex list` prints each name that is up with its pid and server URL
(never the token), one per line, and removes records whose session process
is gone.

Everything lives under `$XDG_STATE_HOME/comlink` (default
`~/.local/state/comlink`); a session exits non-zero when `XDG_STATE_HOME` is
unset and the home directory cannot be determined:

```
run/NAME.json    the record: name, server, the /codex endpoint, pid (mode 0600)
run/NAME.token   the token, and nothing else (mode 0600; never in the record or the ssh config)
run/NAME.token-command
                 instead of NAME.token with --token-command: the command with the working
                 directory and PATH it runs with (JSON), run for each app-server session (mode 0600)
run/NAME.lock    held with flock by the session process for its lifetime
ssh/NAME.conf    the Host block above (mode 0600)
host_ed25519     the host key comlink codex --stdio pins (see below)
```

The record, the token (or token command) file and the ssh config file are each written to a
temporary file in the same directory (whose name matches neither `*.json`
nor `*.conf`) and renamed into place, so ssh, the Codex app and `codex
--stdio --name` never read a half-written file.

A session is up while its lock is held. The kernel releases the lock when
the session process ends, however it ends, so a record left behind by a
killed session is stale even if its pid has since been reused; a new
session on that name removes it and continues, and `list` removes it. The
session holds the lock exclusively; a look at a name (`list`, `--stdio
--name`) only takes it shared for an instant, so looks never make a stale
name seem up or keep a new session from starting, and `list` removes a
stale name's files while it holds the lock shared, so a session that comes
up meanwhile keeps its files. The
pid in the record is for display. On a build without flock (not unix)
liveness falls back to whether the recorded pid exists.

`comlink codex --stdio` generates an ed25519 host key on first run and keeps it at
`$XDG_STATE_HOME/comlink/host_ed25519` so `StrictHostKeyChecking accept-new`
pins a stable key. The block's `HostKeyAlias comlink-<alias>`
stores that key in `known_hosts` under its own name, so it never collides
with a real host called `<alias>`. `--report-version` (with `--stdio`) sets
the Codex version reported to `codex --version` (default `0.165.0`); the app
refuses versions below its minimum.

## ACP clients

An ACP client such as Zed starts its agent as a local subprocess when it
needs it and speaks newline-delimited JSON-RPC on the subprocess's
stdin/stdout. When the agent is a comlink server, which serves ACP over a
WebSocket at `<server>/acp` (one message per text frame,
`Authorization: Bearer <token>`), configure comlink as the agent command:

```sh
comlink acp --server wss://agent.example.com \
  --token-file ~/.config/comlink/token
```

The client's agent command is `comlink` with those arguments. The process
lives as long as the client keeps it, so there is nothing to register and
nothing else to set up (no session, no host key, no ssh config). `--server`
is joined like the Codex session's: `wss://h/x` and `wss://h/x/` both dial
`wss://h/x/acp`.

The token rules are the same as for a Codex session: `--token-file` or
`--token-command`, else `$COMLINK_TOKEN`, else no `Authorization` header,
with a warning for a token file others can read. comlink does not parse the messages: each stdin line
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
  back into the original arguments, the accepted and refused aliases, and
  the refusal of an argument holding a newline or carriage return.
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
  agent subprocess with the token from a file, `$COMLINK_TOKEN` or
  `--token-command`, dialling
  `<server>/acp` for bases with and without a path prefix or trailing slash
  and writing nothing under the home or state directory.
- `relay_test.go` also runs a failing token command only for the relay: the
  version and codex probes still answer, the proxy exits 1 with one log line
  and the upstream is not dialled.
- `openssh_test.go`: when an `ssh` binary is on `PATH`, the version probe
  (also with `--report-version`), a refusal and the proxy relay through
  real OpenSSH with the built binary as its `ProxyCommand` (`codex --stdio
  --server`); and the built binary run directly: the token file permission
  warning, a missing token file and the missing home directory.
- `lock_test.go` (unix): with the test holding a stale name's lock shared,
  as a concurrent look would, `Load` still reports it not up and `List`
  still removes it, while a name a session holds stays up.
- `session_test.go`: the Codex session registry in process. The `/codex`
  and `/acp` endpoints for bases with and without a path prefix or trailing
  slash, endpoint errors and `upstream.Redact` without the URL's userinfo or
  query, the name rule (no leading `-`), the state directory from `XDG_STATE_HOME` or the
  home directory, `Start`/`Load`/`List`/`Close` with the file modes, no
  temporary file left beside them and the token kept out of the record, a
  token command stored instead of a token and run by every `Resolve` (a
  failing one failing it), a
  refused second start (without a pid when the live session's record is
  missing or unreadable), `Load` reporting a held name without its token
  file as not up, a failed write undone, stale records (lock not
  held) removed by `List` and replaced by `Start` while `Load` reports them
  as not up, `Up` driven to its ready line and ended through its context,
  `Up` without a name deriving it from the server's host (lowercased, with
  a port, a path or a trailing dot) and printing the `name:` line, an
  explicit name used as is, `comlink` with the reason on the `name:` line
  for an IPv4 or IPv6 address, `localhost`, `*.localhost`,
  `localhost.<domain>` or a label that is not a valid alias, and the next
  suffix (`padawan-3`, `comlink-2`) when names are up,
  `Resolve`, `PrintList`, the flag combinations of `comlink codex`
  (`Flags.Check`, `Flags.CheckUnset`) and the endpoint `--stdio` relays to
  (`Flags.Endpoint`, with `--server` without a state directory and with
  `--name` for a session that is or is not up), and the `~/.ssh/config`
  Include that `Up` adds: created private, prepended with the rest
  byte-identical and the mode kept, backed up once, kept after the session,
  nothing written or printed when present at top level in any spelling ssh
  accepts (also `~/...` and relative to `~/.ssh`) and the line added on top when it sits under a `Host` or `Match`
  block, a
  quoted pattern for a state directory holding whitespace, nothing touched
  for a bad alias or server, the failure cases with no session file
  written, and a config that is a symlink (absolute, a relative chain,
  dangling, a loop): the links stay as they were and the target gets the
  line and the backup; and the static scan for the name's `Host` lines: a
  derived name matched by a `Host` line (one of several patterns, an
  upper-case keyword, `=`, double and single quotes, `?`) or one in an
  Included file (absolute, relative to `~/.ssh`, `~`, a glob under a
  `Match` block, under `Host *` or the name's own `Host` line, an Include
  loop) skipped for `devbox-2` with the reason, a pattern that matches the
  suffix too refused with one `--name` line and nothing written, an
  explicit name warned once naming the Host line and up, `--name Agent-X`
  configured by `Host Agent-X` and `Host Agent-*` but not by `Host agent-x`
  or `Host AGENT-X`, and a name only
  `Host *` (even with `%h` options), a negation, an upper-case pattern,
  words that quotes or escapes keep together, an Include under another
  `Host` line, a `Match` block (the documented gap), missing Includes or
  comlink's own stale file apply to up without a word; `Host comlink*`
  refused for a server without a usable label, `Host comlink-*` leaving
  `comlink` free but refusing `comlink-2` and `comlink-3` when `comlink`
  is up and warning an explicit `comlink-x`; and an explicit name that is
  configured and up refused without the warning.
- `token_test.go`: the token from a file or `$COMLINK_TOKEN`, trimmed, the
  permission warning naming the path and a missing file; a token command's
  trimmed stdout with the caller's environment and no stdin, run in the
  pinned working directory and `PATH`, its failure, empty, multi-line and
  oversized output as errors without the output, a timeout that returns
  although a background child holds stdout, and `--token-file` and
  `--token-command` being exclusive.
- `up_test.go`: the built binary with a temporary `HOME` and
  `XDG_STATE_HOME`. `codex --name NAME --server URL` writes the record, token and ssh config with private modes, nothing else
  and no token outside the token file, prints the alias and that it
  created `~/.ssh/config` with the Include line, and removes exactly its
  session files on SIGTERM, SIGINT or SIGHUP with exit 0, keeping the line;
  a server URL with userinfo, a query and a fragment shown without them by
  the session and `codex list` and kept whole in the record; a second session on a name that is up is refused naming the pid;
  a killed session leaves a stale record that the next one replaces and
  `codex list` removes; `codex list` output; a session adding the Include line
  to a missing or existing `~/.ssh/config` (prepended, the rest
  byte-identical, the mode kept, backed up once), writing and printing
  nothing when the line is there at top level in any spelling ssh accepts
  and adding it on top when it sits under a `Host` block, writing
  through a symlinked `~/.ssh/config`, and exiting non-zero with one error
  line and no session file for a looping link or a config it cannot back
  up; a `~/.ssh/config` with `Host padawan*` refusing a
  session without `--name` for `wss://padawan.example.com` with one line,
  nothing in the state directory and the config unchanged, one with
  `Host padawan` making it `padawan-2` with the reason and nothing written
  for `padawan`, and `--name padawan` warning once, naming the Host line,
  and coming up;
  `codex --stdio --name`
  through real OpenSSH with a config that Includes the session directory, answering
  the version probe and relaying the app-server session to `<server>/codex`
  with the session's token, then failing with the not-up line once the
  session has stopped; a session with `--token-command` storing the command,
  running it at start and for every app-server session, also with connections
  whose `PATH` lacks the helper, and a broken command
  stopping the session before it registers; sessions without `--name` named after their
  server's host (uppercase, with a port and a path): `padawan`, then
  `padawan-2` and `padawan-3` saying why, all listed, `padawan` again once
  it stopped, and `comlink` for an IP address; three names up at once, with and without a path
  prefix or trailing slash, each reaching its own server through OpenSSH;
  and the refused arguments (bad name, one starting with `-`, or server, a session without
  `--server`, `--report-version` without `--stdio`, both or
  neither of `--name` and `--server` with `--stdio`, `--token-file` or
  `--token-command` with `--stdio --name`, the flags of `codex` given to `list`, and the removed
  `proxy`, `up` and `ssh-include` subcommands of `codex`, top-level `up`,
  `list` and `ssh-include`, `--upstream` and `acp --name`).

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
