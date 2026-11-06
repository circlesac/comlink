package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// chats remembers which upstream ACP session holds each of the app's chats,
// so that a chat the app resumes in a new process (after a lost connection
// or the app's restart) continues the same agent session. Each chat is one
// file named by a hash of the upstream and the app's session id, holding the
// ACP session id; an empty directory keeps nothing.
type chats struct{ dir string }

func (s chats) path(upstream, chat string) string {
	h := sha256.Sum256([]byte(upstream + "\n" + chat))
	return filepath.Join(s.dir, hex.EncodeToString(h[:]))
}

// get returns the ACP session that holds chat on upstream, or "".
func (s chats) get(upstream, chat string) string {
	if s.dir == "" {
		return ""
	}
	b, err := os.ReadFile(s.path(upstream, chat))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// put records that session holds chat on upstream. The file is written to a
// temporary file and renamed into place.
func (s chats) put(upstream, chat, session string) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".chat-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(session + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path(upstream, chat))
}

// transcript is one chat's recorded conversation, in the line format of a
// Claude Code transcript.
type transcript struct {
	chat string
	cwd  string
	data []byte
}

// appendLine adds one transcript line to chat's record on upstream.
func (s chats) appendLine(upstream, chat string, line []byte) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(upstream, chat)+".jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// transcripts returns the recorded chats on upstream. A record is matched
// to its chat by the session id in its first line and the file's name.
func (s chats) transcripts(upstream string) []transcript {
	if s.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []transcript
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		first, _, _ := strings.Cut(string(data), "\n")
		var head struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if json.Unmarshal([]byte(first), &head) != nil || head.SessionID == "" || head.Cwd == "" {
			continue
		}
		if filepath.Base(s.path(upstream, head.SessionID))+".jsonl" != e.Name() {
			continue
		}
		out = append(out, transcript{chat: head.SessionID, cwd: head.Cwd, data: data})
	}
	return out
}

// projectDir is the directory name Claude Code keeps a working directory's
// transcripts under: the path with every character that is not a letter or
// a digit replaced by '-'.
func projectDir(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum {
			b[i] = '-'
		}
	}
	return string(b)
}
