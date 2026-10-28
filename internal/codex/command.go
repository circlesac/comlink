package codex

import (
	"strings"
)

// kind is what comlink does with an exec request. Nothing is ever run.
type kind int

const (
	kindUnknown   kind = iota // refused with exit 127
	kindProxy                 // relay the agent session to the upstream WebSocket
	kindBootstrap             // app-server bootstrap: acknowledged with exit 0
	kindKill                  // pkill of a stale app-server: acknowledged with exit 0
	kindVersion               // codex --version: answered with the reported version
	kindProbe                 // command -v codex: acknowledged with exit 0
)

func (k kind) String() string {
	switch k {
	case kindProxy:
		return "proxy"
	case kindBootstrap:
		return "bootstrap"
	case kindKill:
		return "kill"
	case kindVersion:
		return "version"
	case kindProbe:
		return "probe"
	default:
		return "unknown"
	}
}

// command is a parsed exec request.
type command struct {
	Kind    kind
	Marker  []byte // bytes the client expects on stdout before any output
	Payload string // the shell payload the client wanted run, marker included
}

// parseCommand extracts the payload from an exec string and classifies it.
//
// The Codex app wraps every command as `sh -c '<launcher>' sh '<payload>'`;
// the payload is the last word. Anything else is treated as the payload as
// written so `ssh alias codex --version` works for manual testing.
func parseCommand(exec string) command {
	payload := exec
	if words := shellSplit(exec); len(words) >= 5 && words[0] == "sh" && words[1] == "-c" && words[3] == "sh" {
		payload = words[len(words)-1]
	}
	c := command{Payload: payload}
	rest := payload
	if marker, after, ok := splitMarker(payload); ok {
		c.Marker = marker
		rest = after
	}
	c.Kind = classify(rest)
	return c
}

// shellSplit splits s into words using POSIX shell quoting: single quotes are
// literal, double quotes allow backslash before \ " $ ` and newline, and a
// backslash outside quotes escapes the next character. Variables are not
// expanded. It is enough to peel the launcher wrapper off the payload.
func shellSplit(s string) []string {
	var (
		words  []string
		cur    strings.Builder
		inWord bool
	)
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n':
			flush()
		case '\'':
			inWord = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
				break
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case '"':
			inWord = true
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\\\"$`\n", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
		case '\\':
			inWord = true
			if i+1 < len(s) {
				i++
				if s[i] != '\n' {
					cur.WriteByte(s[i])
				}
			}
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	flush()
	return words
}

func classify(payload string) kind {
	switch {
	case strings.Contains(payload, "app-server proxy"):
		return kindProxy
	case strings.Contains(payload, "CODEX_SSH_SKIP_APP_SERVER_BOOT"), strings.Contains(payload, "app-server --listen"):
		return kindBootstrap
	case strings.Contains(payload, "pkill"):
		return kindKill
	case strings.Contains(payload, "--version"):
		return kindVersion
	case strings.Contains(payload, "command -v"):
		return kindProbe
	default:
		return kindUnknown
	}
}

// splitMarker recognises a leading `printf '%b' '<escapes>';` and returns the
// decoded marker bytes and the remainder of the payload.
func splitMarker(payload string) (marker []byte, rest string, ok bool) {
	const prefix = "printf '%b' '"
	if !strings.HasPrefix(payload, prefix) {
		return nil, payload, false
	}
	body := payload[len(prefix):]
	end := strings.IndexByte(body, '\'')
	if end < 0 {
		return nil, payload, false
	}
	rest = strings.TrimLeft(body[end+1:], " \t")
	rest = strings.TrimPrefix(rest, ";")
	return decodePrintfB(body[:end]), strings.TrimLeft(rest, " \t\n"), true
}

// decodePrintfB decodes the escapes printf's %b directive understands:
// \\ \a \b \f \n \r \t \v \e, and octal \NNN or \0NNN. An unknown escape is
// kept as written.
func decodePrintfB(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out = append(out, s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case '\\':
			out = append(out, '\\')
		case 'a':
			out = append(out, '\a')
		case 'b':
			out = append(out, '\b')
		case 'e':
			out = append(out, 0x1b)
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'v':
			out = append(out, '\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			if c == '0' && j+1 < len(s) && isOctal(s[j+1]) {
				j++ // \0NNN: the zero is a prefix, not a digit
			}
			n, k := 0, 0
			for ; k < 3 && j+k < len(s) && isOctal(s[j+k]); k++ {
				n = n*8 + int(s[j+k]-'0')
			}
			out = append(out, byte(n))
			i = j + k - 1
		default:
			out = append(out, '\\', c)
		}
	}
	return out
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
