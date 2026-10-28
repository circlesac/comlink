// Package shell splits and quotes words the way a POSIX shell does, enough to
// read the commands SSH clients send and to write ProxyCommand lines.
package shell

import (
	"strings"
)

// Split splits s into words using POSIX shell quoting: single quotes are
// literal, double quotes allow backslash before \ " $ ` and newline, and a
// backslash outside quotes escapes the next character. Variables are not
// expanded. It is enough to peel the launcher wrapper off the payload.
func Split(s string) []string {
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

// Quote quotes s for a POSIX shell. Plain words are returned as is.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		plain := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_-./:@%=+,", c) >= 0
		if !plain {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
