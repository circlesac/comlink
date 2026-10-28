package sshserver

import (
	"errors"
	"io"
	"net"
	"time"
)

// StdioConn presents a reader/writer pair (the ProxyCommand's stdin and
// stdout) as a net.Conn so the SSH server can run over it.
type StdioConn struct {
	In  io.Reader
	Out io.Writer
}

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

func (c *StdioConn) Read(b []byte) (int, error)  { return c.In.Read(b) }
func (c *StdioConn) Write(b []byte) (int, error) { return c.Out.Write(b) }
func (c *StdioConn) LocalAddr() net.Addr         { return stdioAddr{} }
func (c *StdioConn) RemoteAddr() net.Addr        { return stdioAddr{} }
func (c *StdioConn) SetDeadline(time.Time) error { return nil }

func (c *StdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *StdioConn) SetWriteDeadline(time.Time) error { return nil }

func (c *StdioConn) Close() error {
	var errs []error
	if cl, ok := c.In.(io.Closer); ok {
		errs = append(errs, cl.Close())
	}
	if cl, ok := c.Out.(io.Closer); ok {
		errs = append(errs, cl.Close())
	}
	return errors.Join(errs...)
}
