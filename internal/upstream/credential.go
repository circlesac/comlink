package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// Credential is where an upstream's bearer token comes from: Token as
// given, or the standard output of Command, run each time a token is needed
// so a short-lived token is fresh for every connection. Dir and Path are the
// working directory and PATH the command was given with, so it runs the same
// from a session's ProxyCommand (started by the Codex app's ssh, with the
// app's environment) as in the terminal that started the session. Neither
// Token nor Command set means no Authorization header.
type Credential struct {
	Token   string
	Command string
	Dir     string
	Path    string
}

// TokenCommandTimeout bounds one run of a token command.
const TokenCommandTimeout = 30 * time.Second

// tokenOutputLimit is the most stdout a token command may print.
const tokenOutputLimit = 64 << 10

// LoadCredential returns the credential of --token-file and
// --token-command: the command when it is given (it is not run here) with
// the current working directory and PATH, else the token LoadToken reads
// from file or $COMLINK_TOKEN. The two flags are exclusive.
func LoadCredential(file, command string, logger *log.Logger) (Credential, error) {
	if command != "" {
		if file != "" {
			return Credential{}, errors.New("--token-file and --token-command are exclusive")
		}
		dir, err := os.Getwd()
		if err != nil {
			return Credential{}, fmt.Errorf("working directory for --token-command: %w", err)
		}
		return Credential{Command: command, Dir: dir, Path: os.Getenv("PATH")}, nil
	}
	token, err := LoadToken(file, logger)
	return Credential{Token: token}, err
}

// Bearer returns the token to send: Token, or the trimmed stdout of Command
// run through the shell (sh -c; cmd /C on Windows) in Dir with PATH set to
// Path, no stdin, the rest of the caller's environment and its stderr passed
// through. On a timeout the command's whole process group is killed. A
// command that fails, times out, prints nothing or prints more than one line
// is an error, so a broken helper never dials without a token. Errors never
// carry the output.
func (c Credential) Bearer() (string, error) {
	if c.Command == "" {
		return c.Token, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), TokenCommandTimeout)
	defer cancel()
	cmd := shellCommand(ctx, c.Command)
	// A grandchild that keeps stdout open would otherwise hold Run past the timeout.
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = c.Dir
	if c.Path != "" {
		cmd.Env = append(withoutPath(os.Environ()), "PATH="+c.Path)
	}
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return "", fmt.Errorf("token command timed out after %s", TokenCommandTimeout)
	case err != nil:
		// Not wrapped: kong would take *exec.ExitError's ExitCode as comlink's.
		return "", fmt.Errorf("token command failed: %v", err)
	case out.overflow:
		return "", fmt.Errorf("token command printed more than %d bytes", tokenOutputLimit)
	}
	token := strings.TrimSpace(out.String())
	switch {
	case token == "":
		return "", errors.New("token command printed no token")
	case strings.ContainsAny(token, "\r\n"):
		return "", errors.New("token command printed more than one line")
	}
	return token, nil
}

// withoutPath is env without its PATH entries.
func withoutPath(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	return out
}

// limitedBuffer keeps at most tokenOutputLimit bytes and remembers whether
// more were written, without failing the command's writes. It does not
// embed bytes.Buffer: exec would copy through its ReadFrom, around Write.
type limitedBuffer struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := tokenOutputLimit - b.buf.Len(); len(p) > room {
		b.overflow = true
		b.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string { return b.buf.String() }
