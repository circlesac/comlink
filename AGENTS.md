# Repository guidance

comlink is an SSH front door for coding-agent clients. A client that only knows how to reach a remote machine over SSH — today the Codex desktop app's SSH connections — runs `ssh <alias>`; the alias's `ProxyCommand` starts `comlink`, which speaks the server side of SSH on its own stdin/stdout, answers the few shell commands the client sends, and relays the agent protocol to a WebSocket endpoint. There is no sshd, no shell and no listening port. For ACP clients, which launch their agent as a local subprocess, `comlink acp` is that subprocess and bridges its stdin/stdout to the remote agent's WebSocket.

## Boundaries

- comlink does not implement any agent protocol. It recognises the command that opens an agent session and moves bytes between the SSH channel (or, for ACP, stdio) and the WebSocket. The protocol lives in the endpoint (for example a Cloudflare Worker that implements the Codex app-server protocol).
- Each supported client is a route: how its commands are recognised and which endpoint and framing its session uses. The Codex app is the first route. ACP clients (for example Zed) are the second client: they start the agent as a local stdio subprocess, so `comlink acp` bridges stdio to the WebSocket without SSH. Temple reaches endpoints over WebSocket directly and does not need comlink. Add a client by adding a route, not by special-casing the server.
- Commands are matched against what the client actually sends. Keep the captured commands under `test/testdata/` as the fixtures the tests run against, and refresh them from a real client when it changes (see README).
- An unrecognised command is logged to stderr and exits 127. It is never run.
- The upstream token is a secret: never print it, log it, or put it in an error. Read it from a file or the environment.

## Codex desktop app contract (ChatGPT.app 26.1023, Codex 0.165.0-alpha)

Verified 2026-10-28 by recording an SSH host the app connected to:

- The app runs system `ssh` with `BatchMode=yes`, the host alias from `~/.ssh/config`, and for the session also `-o ExitOnForwardFailure=yes -R 127.0.0.1:<port>:127.0.0.1:<port>`. The `tcpip-forward` global request must be accepted or ssh exits.
- Every command is wrapped: `sh -c '<login-shell launcher>' sh '<payload>'`. The payload is the last argument. It may start with `printf '%b' '<octal escapes>';` — a random marker the app reads before the real output; comlink must write those bytes to stdout first. It may continue with `PATH=...; export PATH;` and other setup that comlink ignores.
- Payloads and their answers:
  - `if command -v codex >/dev/null 2>&1; then exit 0; fi; exit 86` → exit 0.
  - `codex --version` → stdout `codex-cli <version>`, exit 0. The app refuses versions below its minimum; the reported version is configurable.
  - app-server bootstrap (contains `CODEX_SSH_SKIP_APP_SERVER_BOOT` / `app-server --listen`) → exit 0.
  - `pkill ... app-server ... --listen` → exit 0.
  - a payload ending in `exec codex app-server proxy` (possibly preceded by a remote-forward listener check and an ssh-agent symlink) → write the marker, then relay. On this route the client speaks WebSocket (HTTP Upgrade to `ws://codex-app-server/rpc`, then JSON-RPC text frames) over the channel; terminate that WebSocket locally and relay each message to the upstream WebSocket with `Authorization: Bearer <token>`.
- `subsystem sftp`, `shell` and `pty-req` are refused.

## ACP bridge contract

- The client runs `comlink acp --upstream <wss://host/acp> [--token-file PATH]` as its agent command and speaks ACP (JSON-RPC 2.0) on stdin/stdout, one message per line. The upstream serves ACP over a WebSocket with `Authorization: Bearer <token>`, one message per text frame (the remote-transport RFD).
- Each stdin line becomes one text frame without its newline (a trailing `\r` is dropped, empty lines are skipped, nothing is parsed, so a line that is not JSON is still sent). Each text frame becomes one stdout line followed by `\n`. Messages up to 64 MiB pass either way; a longer stdin line closes the upstream with 1009, and a binary frame from the upstream closes it with 1003; both exit 1.
- stdin EOF closes the upstream with 1000 and exits 0 once the close handshake completes. An upstream close exits 0 for 1000/1001 and 1 otherwise, without waiting for stdin. A failed connect is one stderr line naming the upstream without query or userinfo, and exit 1. A failed stdout write closes the upstream with 1001 and exits 1.
- stdout carries nothing but protocol lines; diagnostics go to stderr.

## Conventions

- Go, one module, one binary. Pull requests are squash-merged into `main`.
- Layout: `main.go` declares the CLI and wires the packages; `internal/sshd` is the SSH side, hands each exec request to a route and renders the ssh-config block; `internal/codex` is the Codex app route; `internal/acp` is the ACP stdio bridge; `internal/upstream` dials the upstream WebSocket for both and redacts its URL in errors. `internal/` holds no tests. All tests live in `test/` (one package, one file per subject) and reach the code only through its exported entry points, mostly by driving `sshd.Serve` with the Codex route over a real SSH connection and `acp.Bridge.Run` over pipes; nothing is exported or hooked for the tests' sake.
- `gofmt` and `golangci-lint` run in the test gate with `go vet ./...` and `go test -race -coverpkg=./... -coverprofile=coverage.out ./...`; CI fails when total statement coverage is below 80%.
- CI runs on `runs-on: ${{ fromJSON(vars.LINUX_RUNNER) }}`.
- No real personal or company identifiers in docs, tests or fixtures.
