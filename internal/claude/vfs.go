package claude

import (
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// fileCap is the most content a virtual file keeps. The app uploads its
// helper binary and plugin files over SFTP; comlink keeps their size, so the
// app finds them where it put them, and drops their bytes.
const fileCap = 1 << 20

// node is a file or directory of the virtual tree.
type node struct {
	dir   bool
	data  []byte // content, kept up to fileCap bytes
	size  int64  // size as written, data or not
	mode  uint32 // permission bits
	mtime time.Time
}

// vfs is the file tree the Claude app sees: the home directory, the
// ~/.claude/remote tree the app expects after its own setup, and whatever
// the app itself creates over SFTP or the probe commands. Nothing is on
// disk. Paths are cleaned; a path is matched as written, so the app sees the
// same tree under every spelling it uses.
type vfs struct {
	mu    sync.Mutex
	nodes map[string]*node
	home  string
}

func newVFS(home string) *vfs {
	fs := &vfs{nodes: map[string]*node{}, home: home}
	for _, dir := range []string{home, home + "/.claude", home + "/.claude/remote", home + "/.claude/remote/run", home + "/.claude/remote/srv", home + "/.claude/remote/ccd-cli", home + "/.claude/remote/plugins", home + "/.claude/projects"} {
		fs.mkdirAll(dir)
	}
	return fs
}

func clean(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

// mkdirAll creates dir and its parents as directories, leaving existing
// nodes alone.
func (fs *vfs) mkdirAll(dir string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	dir = clean(dir)
	for p := dir; ; p = path.Dir(p) {
		if _, ok := fs.nodes[p]; !ok {
			fs.nodes[p] = &node{dir: true, mode: 0o700, mtime: time.Now()}
		}
		if p == "/" {
			break
		}
	}
}

// stat returns a copy of the node at p and whether it exists.
func (fs *vfs) stat(p string) (node, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n, ok := fs.nodes[clean(p)]
	if !ok {
		return node{}, false
	}
	return *n, true
}

// put creates or replaces the file at p with data (its parents are created).
func (fs *vfs) put(p string, data []byte, mode uint32) {
	p = clean(p)
	fs.mkdirAll(path.Dir(p))
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := &node{size: int64(len(data)), mode: mode, mtime: time.Now()}
	if len(data) <= fileCap {
		n.data = append([]byte(nil), data...)
	}
	fs.nodes[p] = n
}

// ensure makes the file at p exist with at least mode's bits: an upload
// the app made there is kept.
func (fs *vfs) ensure(p string, mode uint32) {
	p = clean(p)
	fs.mkdirAll(path.Dir(p))
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if n, ok := fs.nodes[p]; ok && !n.dir {
		n.mode |= mode
		return
	}
	fs.nodes[p] = &node{mode: mode, mtime: time.Now()}
}

// chmod sets the permission bits of the node at p.
func (fs *vfs) chmod(p string, mode uint32) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n, ok := fs.nodes[clean(p)]
	if ok {
		n.mode = mode & 0o777
	}
	return ok
}

// writeAt writes data into the file at p at offset off, creating it when it
// is missing. Content past fileCap is counted but not kept.
func (fs *vfs) writeAt(p string, off int64, data []byte) {
	p = clean(p)
	fs.mkdirAll(path.Dir(p))
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n, ok := fs.nodes[p]
	if !ok || n.dir {
		n = &node{mode: 0o600}
		fs.nodes[p] = n
	}
	end := off + int64(len(data))
	if end > n.size {
		n.size = end
	}
	n.mtime = time.Now()
	if end > fileCap {
		n.data = nil
		return
	}
	if int64(len(n.data)) < end {
		n.data = append(n.data, make([]byte, end-int64(len(n.data)))...)
	}
	copy(n.data[off:], data)
}

// remove deletes the node at p and, for a directory, everything under it.
func (fs *vfs) remove(p string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	p = clean(p)
	if _, ok := fs.nodes[p]; !ok {
		return false
	}
	for q := range fs.nodes {
		if q == p || strings.HasPrefix(q, p+"/") {
			delete(fs.nodes, q)
		}
	}
	return true
}

// rename moves the node at from, and everything under it, to to.
func (fs *vfs) rename(from, to string) bool {
	from, to = clean(from), clean(to)
	if _, ok := fs.stat(from); !ok {
		return false
	}
	fs.mkdirAll(path.Dir(to))
	fs.mu.Lock()
	defer fs.mu.Unlock()
	moved := map[string]*node{}
	for q, n := range fs.nodes {
		if q == from || strings.HasPrefix(q, from+"/") {
			moved[to+strings.TrimPrefix(q, from)] = n
			delete(fs.nodes, q)
		}
	}
	for q, n := range moved {
		fs.nodes[q] = n
	}
	return true
}

// list returns the names of the direct children of dir, sorted.
func (fs *vfs) list(dir string) ([]string, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	dir = clean(dir)
	n, ok := fs.nodes[dir]
	if !ok || !n.dir {
		return nil, false
	}
	var names []string
	prefix := dir + "/"
	if dir == "/" {
		prefix = "/"
	}
	for q := range fs.nodes {
		if q != dir && strings.HasPrefix(q, prefix) && !strings.Contains(q[len(prefix):], "/") {
			names = append(names, q[len(prefix):])
		}
	}
	sort.Strings(names)
	return names, true
}

// glob returns the paths pattern names: the path itself when it exists, or
// the entries of its directory its last component matches ('*' and '?'
// as in path.Match). Nothing when neither.
func (fs *vfs) glob(pattern string) []string {
	pattern = clean(pattern)
	if _, ok := fs.stat(pattern); ok {
		return []string{pattern}
	}
	dir, base := path.Split(pattern)
	if !strings.ContainsAny(base, "*?[") {
		return nil
	}
	names, ok := fs.list(dir)
	if !ok {
		return nil
	}
	var out []string
	for _, name := range names {
		if ok, _ := path.Match(base, name); ok {
			out = append(out, clean(dir)+"/"+name)
		}
	}
	return out
}
