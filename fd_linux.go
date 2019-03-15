package vsock

import (
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// A listenFD is a type that wraps a file descriptor used to implement
// net.Listener.
type listenFD interface {
	io.Closer
	EarlyClose() error
	Accept4(flags int) (connFD, unix.Sockaddr, error)
	Bind(sa unix.Sockaddr) error
	Listen(n int) error
	Getsockname() (unix.Sockaddr, error)
	SetNonblocking(name string) error
}

var _ listenFD = &sysListenFD{}

// A sysListenFD is the system call implementation of listenFD.
type sysListenFD struct {
	// These fields should never be non-zero at the same time.
	fd int      // Used in blocking mode.
	f  *os.File // Used in non-blocking mode.
}

// newListenFD creates a sysListenFD in its default blocking mode.
func newListenFD() (*sysListenFD, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}

	return &sysListenFD{
		fd: fd,
	}, nil
}

// Blocking mode methods.

func (lfd *sysListenFD) Bind(sa unix.Sockaddr) error         { return unix.Bind(lfd.fd, sa) }
func (lfd *sysListenFD) Getsockname() (unix.Sockaddr, error) { return unix.Getsockname(lfd.fd) }
func (lfd *sysListenFD) Listen(n int) error                  { return unix.Listen(lfd.fd, n) }

func (lfd *sysListenFD) SetNonblocking(name string) error {
	// From now on, we must perform non-blocking I/O, so that our
	// net.Listener.Accept method can be interrupted by closing the socket.
	if err := unix.SetNonblock(lfd.fd, true); err != nil {
		return err
	}

	// Transition from blocking mode to non-blocking mode.
	lfd.f = os.NewFile(uintptr(lfd.fd), name)

	return nil
}

// EarlyClose is a blocking version of Close, only used for cleanup before
// entering non-blocking mode.
func (lfd *sysListenFD) EarlyClose() error { return unix.Close(lfd.fd) }

// Non-blocking mode methods.

func (lfd *sysListenFD) Accept4(flags int) (connFD, unix.Sockaddr, error) {
	rc, err := lfd.f.SyscallConn()
	if err != nil {
		return nil, nil, err
	}

	var (
		nfd int
		sa  unix.Sockaddr
	)

	doErr := rc.Read(func(fd uintptr) bool {
		nfd, sa, err = unix.Accept4(int(fd), flags)

		switch err {
		case unix.EAGAIN, unix.ECONNABORTED:
			// Return false to let the poller wait for readiness. See the
			// source code for internal/poll.FD.RawRead for more details.
			//
			// When the socket is in non-blocking mode, we might see EAGAIN if
			// the socket is not ready for reading.
			//
			// In addition, the network poller's accept implementation also
			// deals with ECONNABORTED, in case a socket is closed before it is
			// pulled from our listen queue.
			return false
		default:
			// No error or some unrecognized error, treat this Read operation
			// as completed.
			return true
		}
	})
	if doErr != nil {
		return nil, nil, doErr
	}
	if err != nil {
		return nil, nil, err
	}

	// Create a non-blocking connFD which will be used to implement net.Conn.
	cfd := &sysConnFD{fd: nfd}
	return cfd, sa, nil
}

func (lfd *sysListenFD) Close() error {
	// *os.File.Close will also close the runtime network poller file descriptor,
	// so that net.Listener.Accept can stop blocking.
	return lfd.f.Close()
}

// A connFD is a type that wraps a file descriptor used to implement net.Conn.
type connFD interface {
	io.ReadWriteCloser
	EarlyClose() error
	Connect(sa unix.Sockaddr) error
	Getsockname() (unix.Sockaddr, error)
	SetNonblocking(name string) error
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

var _ connFD = &sysConnFD{}

// newConnFD creates a sysConnFD in its default blocking mode.
func newConnFD() (*sysConnFD, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}

	return &sysConnFD{
		fd: fd,
	}, nil
}

// A sysConnFD is the system call implementation of connFD.
type sysConnFD struct {
	// These fields should never be non-zero at the same time.
	fd int      // Used in blocking mode.
	f  *os.File // Used in non-blocking mode.
}

// Blocking mode methods.

func (cfd *sysConnFD) Connect(sa unix.Sockaddr) error      { return unix.Connect(cfd.fd, sa) }
func (cfd *sysConnFD) Getsockname() (unix.Sockaddr, error) { return unix.Getsockname(cfd.fd) }

// EarlyClose is a blocking version of Close, only used for cleanup before
// entering non-blocking mode.
func (cfd *sysConnFD) EarlyClose() error { return unix.Close(cfd.fd) }

func (cfd *sysConnFD) SetNonblocking(name string) error {
	// From now on, we must perform non-blocking I/O, so that our deadline
	// methods work, and the connection can be interrupted by net.Conn.Close.
	if err := unix.SetNonblock(cfd.fd, true); err != nil {
		return err
	}

	// Transition from blocking mode to non-blocking mode.
	cfd.f = os.NewFile(uintptr(cfd.fd), name)

	return nil
}

// Non-blocking mode methods.

func (cfd *sysConnFD) Close() error {
	// *os.File.Close will also close the runtime network poller file descriptor,
	// so that read/write can stop blocking.
	return cfd.f.Close()
}

func (cfd *sysConnFD) Read(b []byte) (int, error) {
	n, err := cfd.f.Read(b)
	if err != nil {
		// "transport not connected" means io.EOF in Go.
		if perr, ok := err.(*os.PathError); ok && perr.Err == unix.ENOTCONN {
			return n, io.EOF
		}
	}

	return n, err
}

func (cfd *sysConnFD) Write(b []byte) (int, error)        { return cfd.f.Write(b) }
func (cfd *sysConnFD) SetDeadline(t time.Time) error      { return cfd.f.SetDeadline(t) }
func (cfd *sysConnFD) SetReadDeadline(t time.Time) error  { return cfd.f.SetReadDeadline(t) }
func (cfd *sysConnFD) SetWriteDeadline(t time.Time) error { return cfd.f.SetWriteDeadline(t) }
