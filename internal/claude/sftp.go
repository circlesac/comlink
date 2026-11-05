package claude

import (
	"bufio"
	"encoding/binary"
	"io"
	"path"
	"strconv"
	"strings"
)

// SFTP version 3 (draft-ietf-secsh-filexfer-02), the subset the Claude app
// uses to inspect the remote home, upload its helper and plugins, write its
// token and read transcripts. Every operation is answered from the virtual
// tree.
const (
	fxpInit          = 1
	fxpVersion       = 2
	fxpOpen          = 3
	fxpClose         = 4
	fxpRead          = 5
	fxpWrite         = 6
	fxpLstat         = 7
	fxpFstat         = 8
	fxpSetstat       = 9
	fxpFsetstat      = 10
	fxpOpendir       = 11
	fxpReaddir       = 12
	fxpRemove        = 13
	fxpMkdir         = 14
	fxpRmdir         = 15
	fxpRealpath      = 16
	fxpStat          = 17
	fxpRename        = 18
	fxpReadlink      = 19
	fxpSymlink       = 20
	fxpStatus        = 101
	fxpHandle        = 102
	fxpData          = 103
	fxpName          = 104
	fxpAttrs         = 105
	fxpExtended      = 200
	fxpExtendedReply = 201

	fxOK            = 0
	fxEOF           = 1
	fxNoSuchFile    = 2
	fxFailure       = 4
	fxOpUnsupported = 8

	fxfRead  = 1
	fxfWrite = 2
	fxfCreat = 8
	fxfTrunc = 16

	// sftpMaxPacket bounds one request; the app's limits@openssh.com reply
	// says 256 KiB and it keeps to it.
	sftpMaxPacket = 1 << 20
)

// sftpHandle is an open file or directory.
type sftpHandle struct {
	path string
	dir  bool
	read bool // READDIR has listed the directory once
}

type sftpServer struct {
	fs      *vfs
	w       io.Writer
	handles map[string]*sftpHandle
	next    int
}

// serveSFTP answers SFTP requests on ch until it ends and returns 0.
func serveSFTP(ch io.ReadWriter, fs *vfs) int {
	s := &sftpServer{fs: fs, w: ch, handles: map[string]*sftpHandle{}}
	br := bufio.NewReaderSize(ch, 1<<16)
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return 0
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n == 0 || n > sftpMaxPacket {
			return 1
		}
		pkt := make([]byte, n)
		if _, err := io.ReadFull(br, pkt); err != nil {
			return 0
		}
		if err := s.handle(pkt[0], pkt[1:]); err != nil {
			return 1
		}
	}
}

// packet builds one reply: type, then the fields (uint32, uint64, string or
// raw bytes) and sends it framed.
func (s *sftpServer) send(typ byte, fields ...any) error {
	var b []byte
	b = append(b, typ)
	for _, f := range fields {
		switch v := f.(type) {
		case uint32:
			b = binary.BigEndian.AppendUint32(b, v)
		case uint64:
			b = binary.BigEndian.AppendUint64(b, v)
		case string:
			b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
			b = append(b, v...)
		case []byte:
			b = append(b, v...)
		}
	}
	framed := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	_, err := s.w.Write(append(framed, b...))
	return err
}

func (s *sftpServer) status(id uint32, code uint32) error {
	msg := map[uint32]string{fxOK: "Success", fxEOF: "End of file", fxNoSuchFile: "No such file", fxFailure: "Failure", fxOpUnsupported: "Operation unsupported"}[code]
	return s.send(fxpStatus, id, code, msg, "")
}

// attrs encodes n as SFTP attributes: size, owner, permissions and times.
func attrs(n node) []byte {
	perm := n.mode | 0o100000
	if n.dir {
		perm = n.mode | 0o40000
	}
	t := uint32(n.mtime.Unix())
	var b []byte
	b = binary.BigEndian.AppendUint32(b, 1|2|4|8)
	b = binary.BigEndian.AppendUint64(b, uint64(n.size))
	b = binary.BigEndian.AppendUint32(b, uid)
	b = binary.BigEndian.AppendUint32(b, uid)
	b = binary.BigEndian.AppendUint32(b, perm)
	b = binary.BigEndian.AppendUint32(b, t)
	b = binary.BigEndian.AppendUint32(b, t)
	return b
}

// reader walks a request's fields.
type reader struct {
	b   []byte
	err bool
}

func (r *reader) uint32() uint32 {
	if len(r.b) < 4 {
		r.err = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) uint64() uint64 {
	if len(r.b) < 8 {
		r.err = true
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *reader) string() string {
	n := r.uint32()
	if r.err || uint32(len(r.b)) < n {
		r.err = true
		return ""
	}
	v := string(r.b[:n])
	r.b = r.b[n:]
	return v
}

// resolve makes p absolute: a relative path is under the home directory.
func (s *sftpServer) resolve(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = s.fs.home + "/" + p
	}
	return clean(p)
}

func (s *sftpServer) handle(typ byte, payload []byte) error {
	r := &reader{b: payload}
	if typ == fxpInit {
		return s.send(fxpVersion, uint32(3), "posix-rename@openssh.com", "1")
	}
	id := r.uint32()
	if r.err {
		return s.status(id, fxFailure)
	}
	switch typ {
	case fxpExtended:
		name := r.string()
		switch name {
		case "limits@openssh.com":
			return s.send(fxpExtendedReply, id, uint64(262144), uint64(261120), uint64(261120), uint64(4095))
		case "posix-rename@openssh.com":
			from, to := s.resolve(r.string()), s.resolve(r.string())
			if !s.fs.rename(from, to) {
				return s.status(id, fxNoSuchFile)
			}
			return s.status(id, fxOK)
		}
		return s.status(id, fxOpUnsupported)
	case fxpLstat, fxpStat:
		n, ok := s.fs.stat(s.resolve(r.string()))
		if !ok {
			return s.status(id, fxNoSuchFile)
		}
		return s.send(fxpAttrs, id, attrs(n))
	case fxpRealpath:
		p := s.resolve(r.string())
		return s.send(fxpName, id, uint32(1), p, p, attrs(node{dir: true, mode: 0o755}))
	case fxpOpen:
		p := s.resolve(r.string())
		flags := r.uint32()
		n, ok := s.fs.stat(p)
		switch {
		case ok && n.dir:
			return s.status(id, fxFailure)
		case !ok && flags&fxfCreat == 0:
			return s.status(id, fxNoSuchFile)
		case !ok || flags&fxfTrunc != 0:
			s.fs.put(p, nil, 0o600)
		}
		return s.send(fxpHandle, id, s.open(p, false))
	case fxpOpendir:
		p := s.resolve(r.string())
		if n, ok := s.fs.stat(p); !ok || !n.dir {
			return s.status(id, fxNoSuchFile)
		}
		return s.send(fxpHandle, id, s.open(p, true))
	case fxpClose:
		delete(s.handles, r.string())
		return s.status(id, fxOK)
	case fxpRead:
		h, ok := s.handles[r.string()]
		off, n := r.uint64(), r.uint32()
		if !ok || h.dir {
			return s.status(id, fxFailure)
		}
		f, _ := s.fs.stat(h.path)
		if off >= uint64(len(f.data)) {
			return s.status(id, fxEOF)
		}
		end := min(uint64(len(f.data)), off+uint64(n))
		return s.send(fxpData, id, string(f.data[off:end]))
	case fxpWrite:
		h, ok := s.handles[r.string()]
		off := r.uint64()
		data := r.string()
		if !ok || h.dir || r.err {
			return s.status(id, fxFailure)
		}
		s.fs.writeAt(h.path, int64(off), []byte(data))
		return s.status(id, fxOK)
	case fxpFstat:
		h, ok := s.handles[r.string()]
		if !ok {
			return s.status(id, fxFailure)
		}
		n, ok := s.fs.stat(h.path)
		if !ok {
			return s.status(id, fxNoSuchFile)
		}
		return s.send(fxpAttrs, id, attrs(n))
	case fxpReaddir:
		h, ok := s.handles[r.string()]
		if !ok || !h.dir {
			return s.status(id, fxFailure)
		}
		if h.read {
			return s.status(id, fxEOF)
		}
		h.read = true
		names, _ := s.fs.list(h.path)
		var b []byte
		b = binary.BigEndian.AppendUint32(b, id)
		b = binary.BigEndian.AppendUint32(b, uint32(len(names)+2))
		for _, name := range append([]string{".", ".."}, names...) {
			n, _ := s.fs.stat(path.Join(h.path, name))
			b = binary.BigEndian.AppendUint32(b, uint32(len(name)))
			b = append(b, name...)
			long := modeString(n) + " 1 " + strconv.Itoa(uid) + " " + strconv.Itoa(uid) + " " + strconv.FormatInt(n.size, 10) + " " + n.mtime.Format("Jan _2 15:04") + " " + name
			b = binary.BigEndian.AppendUint32(b, uint32(len(long)))
			b = append(b, long...)
			b = append(b, attrs(n)...)
		}
		return s.send(fxpName, b)
	case fxpSetstat, fxpFsetstat:
		// Only the permission bits matter: the app makes its helper executable.
		p := r.string()
		if typ == fxpFsetstat {
			h, ok := s.handles[p]
			if !ok {
				return s.status(id, fxFailure)
			}
			p = h.path
		}
		flags := r.uint32()
		if flags&1 != 0 {
			r.uint64()
		}
		if flags&2 != 0 {
			r.uint64()
		}
		if flags&4 != 0 {
			if perm := r.uint32(); !r.err {
				s.fs.chmod(p, perm)
			}
		}
		return s.status(id, fxOK)
	case fxpSymlink:
		return s.status(id, fxOK)
	case fxpRemove, fxpRmdir:
		if !s.fs.remove(s.resolve(r.string())) {
			return s.status(id, fxNoSuchFile)
		}
		return s.status(id, fxOK)
	case fxpMkdir:
		s.fs.mkdirAll(s.resolve(r.string()))
		return s.status(id, fxOK)
	case fxpRename:
		from, to := s.resolve(r.string()), s.resolve(r.string())
		if !s.fs.rename(from, to) {
			return s.status(id, fxNoSuchFile)
		}
		return s.status(id, fxOK)
	case fxpReadlink:
		return s.status(id, fxNoSuchFile)
	}
	return s.status(id, fxOpUnsupported)
}

func (s *sftpServer) open(p string, dir bool) string {
	s.next++
	h := strconv.Itoa(s.next)
	s.handles[h] = &sftpHandle{path: p, dir: dir}
	return h
}
