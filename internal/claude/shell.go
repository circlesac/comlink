package claude

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The app's setup and probe commands are shell lines: builtins joined by
// `;`, `&&` and `||`, with `$HOME`, quotes and a few redirections. shell is
// the interpreter for exactly that: it runs nothing, it answers each builtin
// from the virtual tree and the route's identity.
type shell struct {
	route  *Route
	fs     *vfs
	cwd    string
	out    io.Writer
	errOut io.Writer
	args   []string                     // $0, $1, ... of a `sh -c` script
	bridge func(status int) (int, bool) // --bridge: serves on the channel; ok is false outside a channel
	status int                          // $?, the last command's status
	vars   map[string]string            // variables the line assigned
}

// redirs are the redirections of one simple command.
type redirs struct {
	stdin   string // < FILE
	outNull bool   // > /dev/null
	errNull bool   // 2> /dev/null
}

// run interprets line and returns its exit status. exec is true when the
// line ended in an `exec` builtin.
func (s *shell) run(line string) int {
	status, _ := s.exec(splitLine(line))
	return status
}

// exec runs segments in order, honouring `&&` and `||` and the `for ... do
// ... done` and `if ... then ... else ... fi` constructs the app's scripts
// use. stop is true when an exec or exit builtin ended the script.
func (s *shell) exec(segs []segment) (status int, stop bool) {
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		switch seg.op {
		case "&&":
			if status != 0 {
				continue
			}
		case "||":
			if status == 0 {
				continue
			}
		}
		switch {
		case strings.HasPrefix(seg.text, "for "):
			body, end := block(segs, i, "for ", "done")
			status, stop = s.forLoop(seg.text, body)
			i = end
		case strings.HasPrefix(seg.text, "if "):
			body, end := block(segs, i, "if ", "fi")
			status, stop = s.ifThen(seg.text, body)
			i = end
		default:
			status, stop = s.command(seg.text)
		}
		s.status = status
		if stop {
			return status, true
		}
	}
	return status, false
}

// block returns the segments between the construct starting at segs[start]
// (a `for` or `if`) and its closing keyword, nesting counted, and the index
// of that keyword (the last segment when it is missing).
func block(segs []segment, start int, open, closing string) (body []segment, end int) {
	depth := 0
	for i := start + 1; i < len(segs); i++ {
		t := segs[i].text
		switch {
		case strings.HasPrefix(t, open):
			depth++
		case t == closing:
			if depth == 0 {
				return segs[start+1 : i], i
			}
			depth--
		}
	}
	return segs[start+1:], len(segs) - 1
}

// forLoop runs `for VAR in WORDS` (head) over body, whose first segment
// starts with `do`.
func (s *shell) forLoop(head string, body []segment) (status int, stop bool) {
	w, _ := s.words(strings.TrimPrefix(head, "for "))
	if len(w) < 2 || w[1] != "in" {
		return 2, false
	}
	body = stripKeyword(body, "do")
	if s.vars == nil {
		s.vars = map[string]string{}
	}
	for _, item := range w[2:] {
		s.vars[w[0]] = item
		if status, stop = s.exec(body); stop {
			return status, true
		}
	}
	return status, false
}

// ifThen runs the condition of `if COND` (head) and then the `then` part of
// body, or its `else` part.
func (s *shell) ifThen(head string, body []segment) (status int, stop bool) {
	cond, stop := s.exec(splitLine(strings.TrimPrefix(head, "if ")))
	if stop {
		return cond, true
	}
	var thenPart, elsePart []segment
	depth := 0
	part := &thenPart
	for _, seg := range body {
		switch {
		case strings.HasPrefix(seg.text, "if "):
			depth++
		case seg.text == "fi":
			depth--
		case depth == 0 && (seg.text == "else" || strings.HasPrefix(seg.text, "else ")):
			part = &elsePart
			if seg.text != "else" {
				*part = append(*part, segment{text: strings.TrimPrefix(seg.text, "else ")})
			}
			continue
		}
		*part = append(*part, seg)
	}
	if cond == 0 {
		return s.exec(stripKeyword(thenPart, "then"))
	}
	if len(elsePart) == 0 {
		return 0, false
	}
	return s.exec(elsePart)
}

// stripKeyword removes the keyword that opens the first segment of body
// (`do`, `then`), alone or before a command.
func stripKeyword(body []segment, keyword string) []segment {
	if len(body) == 0 {
		return body
	}
	out := append([]segment(nil), body...)
	switch {
	case out[0].text == keyword:
		out = out[1:]
	case strings.HasPrefix(out[0].text, keyword+" "):
		out[0].text = strings.TrimPrefix(out[0].text, keyword+" ")
	}
	return out
}

// segment is one simple command of a line and the operator before it.
type segment struct {
	op   string // "", ";", "&&" or "||"
	text string
}

// splitLine splits line at `;`, `&&`, `||` and newlines outside quotes.
func splitLine(line string) []segment {
	var segs []segment
	var cur strings.Builder
	op := ""
	flush := func(next string) {
		if t := strings.TrimSpace(cur.String()); t != "" {
			segs = append(segs, segment{op: op, text: t})
		}
		cur.Reset()
		op = next
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				cur.WriteString(line[i:])
				i = len(line)
				continue
			}
			cur.WriteString(line[i : i+end+2])
			i += end + 1
		case c == '"':
			j := i + 1
			for ; j < len(line) && line[j] != '"'; j++ {
				if line[j] == '\\' && j+1 < len(line) {
					j++
				}
			}
			if j >= len(line) {
				cur.WriteString(line[i:])
				i = len(line)
				continue
			}
			cur.WriteString(line[i : j+1])
			i = j
		case c == '\\' && i+1 < len(line):
			cur.WriteByte(c)
			cur.WriteByte(line[i+1])
			i++
		case c == '&' && i+1 < len(line) && line[i+1] == '&':
			flush("&&")
			i++
		case c == '|' && i+1 < len(line) && line[i+1] == '|':
			flush("||")
			i++
		case c == ';' || c == '\n':
			flush(";")
		default:
			cur.WriteByte(c)
		}
	}
	flush("")
	return segs
}

// words splits a simple command into words, expanding $HOME, ${HOME}, ~,
// $?, $N, the line's own variables and ${name:-default}, and takes the
// redirections out. Other variables expand to nothing, as they would in the
// app's own environment here.
func (s *shell) words(text string) ([]string, redirs) {
	var out []string
	var rd redirs
	var cur strings.Builder
	inWord := false
	pendingStdin := false // a lone < : the next word is the file
	flush := func() {
		if inWord {
			w := cur.String()
			switch {
			case pendingStdin:
				rd.stdin = w
				pendingStdin = false
			case w == "<":
				pendingStdin = true
			case strings.HasPrefix(w, "<"):
				rd.stdin = w[1:]
			case isRedirection(w):
				target := strings.TrimLeft(w, "0123456789")
				if strings.HasPrefix(w, "2") && strings.HasSuffix(target, "/dev/null") {
					rd.errNull = true
				} else if strings.HasSuffix(target, "/dev/null") {
					rd.outNull = true
				}
			default:
				out = append(out, w)
			}
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case c == '\'':
			inWord = true
			end := strings.IndexByte(text[i+1:], '\'')
			if end < 0 {
				cur.WriteString(text[i+1:])
				i = len(text)
				continue
			}
			cur.WriteString(text[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			inWord = true
			i++
			for ; i < len(text) && text[i] != '"'; i++ {
				switch {
				case text[i] == '\\' && i+1 < len(text) && strings.IndexByte("\\\"$`", text[i+1]) >= 0:
					i++
					cur.WriteByte(text[i])
				case text[i] == '$':
					v, n := s.expand(text[i:])
					cur.WriteString(v)
					i += n - 1
				default:
					cur.WriteByte(text[i])
				}
			}
		case c == '\\' && i+1 < len(text):
			inWord = true
			cur.WriteByte(text[i+1])
			i++
		case c == '$':
			inWord = true
			v, n := s.expand(text[i:])
			cur.WriteString(v)
			i += n - 1
		case c == '~' && !inWord && (i+1 == len(text) || text[i+1] == '/' || text[i+1] == ' '):
			inWord = true
			cur.WriteString(s.fs.home)
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	flush()
	return out, rd
}

func isRedirection(w string) bool {
	w = strings.TrimLeft(w, "0123456789")
	return strings.HasPrefix(w, ">") || strings.HasPrefix(w, "<")
}

var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

// expand expands the variable reference at the start of t ($ included) and
// returns its value and the number of bytes consumed.
func (s *shell) expand(t string) (string, int) {
	if len(t) < 2 {
		return "$", 1
	}
	if strings.HasPrefix(t, "$((") {
		// Arithmetic: integers, variables and + - * / are enough for i=$((i+1)).
		end := strings.Index(t, "))")
		if end < 0 {
			return "$", 1
		}
		return strconv.Itoa(s.arith(t[3:end])), end + 2
	}
	if t[1] == '(' {
		// Command substitution: nothing runs here, so it is empty.
		depth := 0
		for i := 1; i < len(t); i++ {
			switch t[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return "", i + 1
				}
			}
		}
		return "$", 1
	}
	if t[1] == '{' {
		end := strings.IndexByte(t, '}')
		if end < 0 {
			return "$", 1
		}
		ref := t[2:end]
		name, def, hasDefault := strings.Cut(ref, ":-")
		v := s.lookup(name)
		if v == "" && hasDefault {
			v = def
		}
		return v, end + 1
	}
	if t[1] >= '0' && t[1] <= '9' {
		return s.lookup(t[1:2]), 2
	}
	if t[1] == '?' {
		return s.lookup("?"), 2
	}
	m := nameRe.FindString(t[1:])
	if m == "" {
		return "$", 1
	}
	return s.lookup(m), 1 + len(m)
}

// arith evaluates a small arithmetic expression: terms of integers and
// variables joined by + and -, each term a product of * and /.
func (s *shell) arith(expr string) int {
	total := 0
	for _, term := range strings.FieldsFunc(strings.ReplaceAll(expr, "-", "+-"), func(r rune) bool { return r == '+' }) {
		sign, product := 1, 1
		for _, f := range strings.Split(strings.TrimSpace(term), "*") {
			f = strings.TrimSpace(f)
			if strings.HasPrefix(f, "-") {
				sign = -sign
				f = f[1:]
			}
			n, err := strconv.Atoi(f)
			if err != nil {
				n, _ = strconv.Atoi(s.lookup(f))
			}
			product *= n
		}
		total += sign * product
	}
	return total
}

func (s *shell) lookup(name string) string {
	if v, ok := s.vars[name]; ok {
		return v
	}
	switch name {
	case "?":
		return strconv.Itoa(s.status)
	case "HOME":
		return s.fs.home
	case "TMPDIR":
		return "/tmp/"
	case "PATH":
		return "/usr/bin:/bin"
	}
	if n, err := strconv.Atoi(name); err == nil && n < len(s.args) {
		return s.args[n]
	}
	return ""
}

// command runs one simple command and returns its status and whether the
// line stops here (exec, exit).
func (s *shell) command(text string) (status int, stop bool) {
	w, rd := s.words(text)
	if len(w) == 1 && isAssignment(w[0]) {
		// s=$?: the line's own variable.
		name, value, _ := strings.Cut(w[0], "=")
		if s.vars == nil {
			s.vars = map[string]string{}
		}
		s.vars[name] = value
		return 0, false
	}
	// Leading assignments (LC_ALL=C, CODEX_HOME=...) and env only set the
	// environment of the command that follows.
	for len(w) > 0 && (w[0] == "env" || isAssignment(w[0])) {
		w = w[1:]
	}
	if len(w) == 0 {
		return 0, false
	}
	name, args := w[0], w[1:]
	if rd.outNull || rd.errNull {
		sub := *s
		if rd.outNull {
			sub.out = io.Discard
		}
		if rd.errNull {
			sub.errOut = io.Discard
		}
		sub.vars = s.vars
		return sub.run(strings.TrimSpace(strings.Join(quoteAll(w), " ") + stdinWord(rd))), false
	}
	if name == "(exit" {
		// (exit N): a subshell that only sets the status.
		n := 0
		if len(args) > 0 {
			n, _ = strconv.Atoi(strings.TrimSuffix(args[0], ")"))
		}
		return n, false
	}
	switch name {
	case "exec":
		if len(args) == 0 {
			return 0, false
		}
		st, _ := s.command(strings.Join(quoteAll(args), " "))
		return st, true
	case "exit":
		if len(args) > 0 {
			n, _ := strconv.Atoi(args[0])
			return n, true
		}
		return 0, true
	case "sh":
		// sh -c SCRIPT [NAME ARGS...]: the script runs here, with its args.
		if len(args) >= 2 && args[0] == "-c" {
			sub := &shell{route: s.route, fs: s.fs, cwd: s.cwd, out: s.out, errOut: s.errOut, args: args[2:], bridge: s.bridge}
			return sub.run(args[1]), false
		}
		return 127, false
	case "true", ":", "set":
		return 0, false
	case "printf":
		if len(args) == 0 {
			return 1, false
		}
		_, _ = io.WriteString(s.out, shellPrintf(args[0], args[1:]))
		return 0, false
	case "false":
		return 1, false
	case "echo":
		_, _ = io.WriteString(s.out, strings.Join(args, " ")+"\n")
		return 0, false
	case "printenv":
		if len(args) == 0 {
			return 1, false
		}
		if v := s.lookup(args[0]); v != "" && args[0] != "PATH" {
			_, _ = io.WriteString(s.out, v+"\n")
			return 0, false
		}
		return 1, false
	case "uname":
		_, _ = io.WriteString(s.out, "Linux x86_64\n")
		return 0, false
	case "id":
		_, _ = io.WriteString(s.out, strconv.Itoa(uid)+"\n")
		return 0, false
	case "test", "[":
		return s.test(args), false
	case "chmod", "touch":
		return 0, false
	case "find":
		// find DIR ...: a missing directory fails, anything else finds nothing.
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			if n, ok := s.fs.stat(args[0]); !ok || !n.dir {
				return 1, false
			}
		}
		return 0, false
	case "wc", "head", "cat":
		// Only on the file redirected or named: wc -c prints its size and
		// head -c N or cat its content.
		file := rd.stdin
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && !isNumber(a) {
				file = a
			}
		}
		n, ok := s.fs.stat(file)
		if file == "" || !ok || n.dir {
			_, _ = io.WriteString(s.errOut, "sh: "+file+": No such file or directory\n")
			return 1, false
		}
		if name == "wc" {
			_, _ = io.WriteString(s.out, fmt.Sprintf("%8d\n", n.size))
			return 0, false
		}
		data := n.data
		if name == "head" {
			for i, a := range args {
				if a == "-c" && i+1 < len(args) {
					if limit, err := strconv.Atoi(args[i+1]); err == nil && limit < len(data) {
						data = data[:limit]
					}
				}
			}
		}
		_, _ = s.out.Write(data)
		return 0, false
	case "mkdir":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				s.fs.mkdirAll(a)
			}
		}
		return 0, false
	case "rm":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				s.fs.remove(a)
			}
		}
		return 0, false
	case "cd":
		dir := s.fs.home
		if len(args) > 0 {
			dir = args[0]
		}
		if n, ok := s.fs.stat(dir); !ok || !n.dir {
			return 1, false
		}
		s.cwd = clean(dir)
		return 0, false
	case "pwd":
		_, _ = io.WriteString(s.out, s.cwd+"\n")
		return 0, false
	case "mktemp":
		if len(args) == 0 {
			return 1, false
		}
		var b [5]byte
		_, _ = rand.Read(b[:])
		p := strings.Replace(args[len(args)-1], "XXXXXXXXXX", hex.EncodeToString(b[:]), 1)
		s.fs.put(p, nil, 0o600)
		_, _ = io.WriteString(s.out, p+"\n")
		return 0, false
	case "ls":
		return s.ls(args), false
	}
	if strings.Contains(name, "/.claude/remote/srv/") && strings.HasSuffix(name, "/server") {
		return s.server(args)
	}
	if strings.Contains(name, "/.claude/remote/ccd-cli/") {
		if len(args) > 0 && args[0] == "--version" {
			_, _ = io.WriteString(s.out, cliVersion(name)+" (Claude Code)\n")
		}
		return 0, false
	}
	if _, ok := s.fs.stat(name); ok {
		return 0, false
	}
	// The client sees this as the command's stderr; `|| true` in the app's
	// own lines swallows the status, as it does for a real host.
	_, _ = io.WriteString(s.errOut, "comlink: unsupported command: "+name+"\n")
	return 127, false
}

// shellPrintf formats args with format the way printf does for %s, %d,
// %% and the escapes \n, \t and \\; the format is reused while arguments
// remain.
func shellPrintf(format string, args []string) string {
	var b strings.Builder
	for {
		for i := 0; i < len(format); i++ {
			c := format[i]
			switch {
			case c == '\\' && i+1 < len(format):
				i++
				switch format[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(format[i])
				}
			case c == '%' && i+1 < len(format):
				i++
				switch format[i] {
				case '%':
					b.WriteByte('%')
				default:
					if len(args) > 0 {
						b.WriteString(args[0])
						args = args[1:]
					}
				}
			default:
				b.WriteByte(c)
			}
		}
		if len(args) == 0 || !strings.Contains(format, "%") {
			return b.String()
		}
	}
}

func isNumber(w string) bool {
	_, err := strconv.Atoi(w)
	return err == nil
}

// stdinWord is the redirection word that carries rd's stdin on.
func stdinWord(rd redirs) string {
	if rd.stdin == "" {
		return ""
	}
	return " <" + quoteAll([]string{rd.stdin})[0]
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	return i > 0 && nameRe.FindString(w) == w[:i]
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return out
}

// test is the test builtin: the forms the app uses, -z, -f, -x, -d, -s,
// =, != and ! before any of them. Anything else is false.
func (s *shell) test(args []string) int {
	if len(args) > 0 && args[len(args)-1] == "]" {
		args = args[:len(args)-1]
	}
	if len(args) > 0 && args[0] == "!" {
		if s.test(args[1:]) == 0 {
			return 1
		}
		return 0
	}
	switch {
	case len(args) == 2:
		n, ok := s.fs.stat(args[1])
		switch args[0] {
		case "-z":
			return boolStatus(args[1] == "")
		case "-n":
			return boolStatus(args[1] != "")
		case "-f":
			return boolStatus(ok && !n.dir)
		case "-x":
			return boolStatus(ok && n.mode&0o100 != 0)
		case "-d":
			return boolStatus(ok && n.dir)
		case "-s":
			return boolStatus(ok && n.size > 0)
		case "-e", "-r", "-w":
			return boolStatus(ok)
		}
	case len(args) == 3 && args[1] == "=":
		return boolStatus(args[0] == args[2])
	case len(args) == 3 && args[1] == "!=":
		return boolStatus(args[0] != args[2])
	case len(args) == 1:
		return boolStatus(args[0] != "")
	}
	return 1
}

func boolStatus(b bool) int {
	if b {
		return 0
	}
	return 1
}

// ls lists a directory the way `ls -ln` does (one line per entry with the
// numeric owner, for the app's listing of its run directory) or names the
// files given, globs expanded; a path that is missing or a glob that
// matches nothing fails.
func (s *shell) ls(args []string) int {
	long := false
	var paths []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			long = long || strings.Contains(a, "l")
			continue
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		paths = []string{s.cwd}
	}
	status := 0
	for _, p := range paths {
		matches := s.fs.glob(p)
		if len(matches) == 0 {
			_, _ = io.WriteString(s.errOut, "ls: "+p+": No such file or directory\n")
			status = 1
			continue
		}
		for _, m := range matches {
			n, _ := s.fs.stat(m)
			if !n.dir {
				s.lsLine(long, m, path.Base(m), n)
				continue
			}
			names, _ := s.fs.list(m)
			if long {
				_, _ = io.WriteString(s.out, "total 0\n")
			}
			for _, name := range names {
				n, _ := s.fs.stat(m + "/" + name)
				s.lsLine(long, m+"/"+name, name, n)
			}
		}
	}
	return status
}

func (s *shell) lsLine(long bool, full, name string, n node) {
	if !long {
		_, _ = io.WriteString(s.out, full+"\n")
		return
	}
	_, _ = io.WriteString(s.out, fmt.Sprintf("%s 1 %d %d %d %s %s\n", modeString(n), uid, uid, n.size, n.mtime.Format("Jan _2 15:04"), name))
}

// server answers the helper binary's own command line: --version, the
// install, stop and serve steps, --probe-cli and --bridge.
func (s *shell) server(args []string) (status int, stop bool) {
	if len(args) == 0 {
		return 1, false
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	switch args[0] {
	case "--version":
		_, _ = io.WriteString(s.out, "claude-ssh "+s.route.helperVersion()+" (built "+buildDate+")\n")
		return 0, false
	case "--probe-cli":
		return 0, false
	case "--install":
		result := fmt.Sprintf(`{"serverVersion":%q,"os":"linux","arch":"x64","libc":"","cliPath":%q,"cliWasPresent":true}`, s.route.helperVersion(), flag("--cli-dir")+"/"+flag("--cli-version"))
		_, _ = io.WriteString(s.out, "__INSTALL_RESULT__"+result+"\n")
		return 0, false
	case "--stop":
		_, _ = io.WriteString(s.out, "none\n")
		return 0, false
	case "--serve":
		return 0, false
	case "--bridge":
		if s.bridge == nil {
			return 1, true
		}
		return s.bridge(0)
	}
	return 2, false
}

// cliVersion is the version in a ccd-cli/<version> path.
func cliVersion(p string) string {
	v := p[strings.LastIndex(p, "/")+1:]
	v, _, _ = strings.Cut(v, "-")
	return v
}

// buildDate is what `server --version` reports as the helper's build time.
var buildDate = time.Date(2026, 10, 30, 16, 35, 57, 0, time.UTC).Format(time.RFC3339)

func modeString(n node) string {
	const rwx = "rwxrwxrwx"
	b := []byte("----------")
	if n.dir {
		b[0] = 'd'
	}
	for i := range 9 {
		if n.mode&(1<<uint(8-i)) != 0 {
			b[i+1] = rwx[i]
		}
	}
	return string(b)
}
