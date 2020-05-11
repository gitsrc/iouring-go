package iouring

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

const (
	pollin = 0x0001

	// SOReuseport is the socket option to reuse socket port.
	SOReuseport int = 0x0F

	// TCPFastopen is the socket option to open a TCP fast open.
	TCPFastopen int = 0x17

	pollListen int = iota
	pollConn
	pollRead
	pollWrite
)

// FastOpenAllowed return nil if fast open is enabled.
func FastOpenAllowed() error {
	b, err := ioutil.ReadFile("/proc/sys/net/ipv4/tcp_fack")
	if err != nil {
		return err
	}
	allowed, err := strconv.Atoi(strings.Replace(string(b), "\n", "", -1))
	if err != nil {
		return err
	}

	if allowed != 3 {
		return fmt.Errorf("set /proc/sys/net/ipv4/tcp_fastopen to 3")
	}

	return nil
}

type connInfo struct {
	fd       int
	connType int
	id       uint64
	sqeIds   chan uint64
}

type addr struct {
	net string
	s   string
}

// Network implements the net.Addr interface.
func (a *addr) Network() string {
	return a.net
}

// String implements the net.Addr interface.
func (a *addr) String() string {
	return a.s
}

type ringListener struct {
	r       *Ring
	f       *os.File
	a       *addr
	stop    chan struct{}
	newConn chan net.Conn
	connGet chan chan net.Conn
}

// run is used to interact with the ring
func (l *ringListener) run() {
	id := l.r.ID()
	fd := int(l.f.Fd())
	cInfo := &connInfo{
		fd:       fd,
		connType: pollListen,
		id:       id,
	}
	sqe, commit := l.r.SubmitEntry()
	sqe.Opcode = PollAdd
	sqe.Fd = int32(fd)
	sqe.UFlags = int32(pollin)
	sqe.UserData = id
	commit()

	conns := map[uint64]*connInfo{id: cInfo}
	cqSize := uint(1)

	for {
		select {
		case <-l.stop:
			return
		default:
			err := l.r.Enter(cqSize, 1, EnterGetEvents, nil)
			if err != nil {
				// TODO: These errors should probably just be
				// logged.
				panic(err.Error())
			}
			l.walkCq(conns)
		}
	}
}

func (l *ringListener) walkCq(conns map[uint64]*connInfo) {
	head := atomic.LoadUint32(l.r.cq.Head)
	tail := atomic.LoadUint32(l.r.cq.Tail)
	mask := atomic.LoadUint32(l.r.cq.Mask)
	if head&mask == tail&mask {
		return
	}

	seenIdx := head & mask
	seen := false
	seenEnd := false
	for i := seenIdx; i <= uint32(len(l.r.cq.Entries)-1); i++ {
		if l.r.cq.Entries[i].Flags&CqSeenFlag == CqSeenFlag {
			seen = true
		} else if !seenEnd {
			seen = false
			seenEnd = true
		}
		if seen == true && !seenEnd {
			seenIdx = i
		}
		cInfo, ok := conns[l.r.cq.Entries[i].UserData]
		if !ok {
			continue
		}
		switch cInfo.connType {
		case pollListen:
			l.onListen(conns, cInfo)
			atomic.CompareAndSwapUint32(l.r.cq.Head, head, seenIdx)
			return
		case pollConn:
			l.onConn(conns, cInfo)
			atomic.CompareAndSwapUint32(l.r.cq.Head, head, seenIdx)
			return
		}
	}

	// Handle wrapping.
	seenIdx = uint32(0)
	seen = false
	seenEnd = false
	for i := uint32(0); i <= tail&mask; i++ {
		if l.r.cq.Entries[i].Flags&CqSeenFlag == CqSeenFlag {
			seen = true
			// If something else has processed the CQE just
			// continue.
			continue
		} else if !seenEnd {
			seen = false
			seenEnd = true
		}
		if seen == true && !seenEnd {
			seenIdx = i
		}
		cInfo, ok := conns[l.r.cq.Entries[i].UserData]
		if !ok {
			break
		}
		switch cInfo.connType {
		case pollListen:
			l.onListen(conns, cInfo)
			break
		case pollConn:
			l.onConn(conns, cInfo)
			break
		}
	}
	atomic.CompareAndSwapUint32(l.r.cq.Head, head, seenIdx)
}

func (l *ringListener) onConn(conns map[uint64]*connInfo, cInfo *connInfo) {
	// For new connections the ringListener first handles setting up the
	// connection.
	id := l.r.ID()
	sqe, commit := l.r.SubmitEntry()
	sqe.Opcode = ReadFixed
	sqe.Fd = int32(cInfo.fd)
	//sqe.Len = uint32(len(read))
	//sqe.Addr = (uint64)(uintptr(unsafe.Pointer(&read[0])))
	sqe.UFlags = 0
	sqe.UserData = id
	commit()
}

// onListen is called when processing a cqe for a listening socket.
func (l *ringListener) onListen(conns map[uint64]*connInfo, cInfo *connInfo) {
	var (
		newConnInfo connInfo
		offset      int64
		rc          = ringConn{
			stop: make(chan struct{}, 2),
			poll: make(chan struct{}, 128),
			r:    l.r,
		}
	)
	for {
		// Wait for a new connection to arrive and add it to the ring.
		newFd, sa, err := syscall.Accept4(cInfo.fd, syscall.SOCK_NONBLOCK)
		if err != nil {
			// TODO: Log this or something?
			panic(err.Error())
		}
		rc.fd = newFd
		rc.laddr = l.a
		rc.raddr = &addr{net: l.a.net}
		switch sockType := sa.(type) {
		case *syscall.SockaddrInet4:
			rc.raddr.s = fmt.Sprintf("%s:%d", sockType.Addr, sockType.Port)
		case *syscall.SockaddrInet6:
			rc.raddr.s = fmt.Sprintf("%s:%d", sockType.Addr, sockType.Port)
		case *syscall.SockaddrUnix:
			rc.raddr.s = sockType.Name
		}
		rc.offset = &offset
		break
	}

	// Add the new connection back to the ring.
	sqe, commit := l.r.SubmitEntry()
	sqe.Opcode = PollAdd
	sqe.Fd = int32(rc.fd)
	sqe.UFlags = int32(pollin)
	sqe.UserData = newConnInfo.id
	commit()
	go rc.run()

	// Add the old connection back as well.
	delete(conns, cInfo.id)
	id := l.r.ID()
	sqe, commit = l.r.SubmitEntry()
	sqe.Opcode = PollAdd
	sqe.Fd = int32(cInfo.fd)
	sqe.UFlags = int32(pollin)
	sqe.UserData = id
	commit()
	conns[id] = cInfo

	// Wait for the new connection to be accepted.
	// TODO: If this is unbuffered it will block, alternatively it could be
	// sent in a separate goroutine to ensure the main ring code isn't
	// blocking.
	l.newConn <- &rc
}

// Close implements the net.Listener interface.
func (l *ringListener) Close() error {
	close(l.stop)
	return l.f.Close()

}

// Addr implements the net.Listener interface.
func (l *ringListener) Addr() net.Addr {
	return l.a
}

// Accept implements the net.Listener interface.
func (l *ringListener) Accept() (net.Conn, error) {
	return <-l.newConn, nil
}

// SockoptListener returns a net.Listener that is Ring based.
func (r *Ring) SockoptListener(network, address string, sockopts ...int) (net.Listener, error) {
	var (
		err      error
		fd       int
		sockAddr syscall.Sockaddr
	)
	l := &ringListener{
		r:       r,
		a:       &addr{net: network},
		stop:    make(chan struct{}),
		newConn: make(chan net.Conn, 1024),
		connGet: make(chan chan net.Conn),
	}

	switch network {
	case "tcp", "tcp4":
		fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		netAddr, err := net.ResolveTCPAddr(network, address)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		l.a.net = netAddr.Network()
		l.a.s = netAddr.String()

		var ipAddr [4]byte
		copy(ipAddr[:], netAddr.IP)
		sockAddr = &syscall.SockaddrInet4{
			Port: netAddr.Port,
			Addr: ipAddr,
		}
	case "tcp6":
		fd, err = syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		netAddr, err := net.ResolveTCPAddr(network, address)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		l.a.net = netAddr.Network()
		l.a.s = netAddr.String()

		ipAddr := [16]byte{}
		copy(ipAddr[:], netAddr.IP)
		sockAddr = &syscall.SockaddrInet6{
			Port: netAddr.Port,
			Addr: ipAddr,
		}
	case "unix":
		fd, err = syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		sockAddr = &syscall.SockaddrUnix{
			Name: address,
		}
	case "udp", "udp4":
		fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		netAddr, err := net.ResolveUDPAddr(network, address)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		ipAddr := [4]byte{}
		copy(ipAddr[:], netAddr.IP)
		sockAddr = &syscall.SockaddrInet4{
			Port: netAddr.Port,
			Addr: ipAddr,
		}
	case "udp6":
		fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		netAddr, err := net.ResolveUDPAddr(network, address)
		if err != nil {
			return nil, fmt.Errorf("could not open socket")
		}
		l.a.net = netAddr.Network()
		l.a.s = netAddr.String()

		ipAddr := [16]byte{}
		copy(ipAddr[:], netAddr.IP)
		sockAddr = &syscall.SockaddrInet6{
			Port: netAddr.Port,
			Addr: ipAddr,
		}
	default:
		return nil, fmt.Errorf("unknown network family: %s", network)
	}
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}

	for _, sockopt := range sockopts {
		if sockopt == SOReuseport {
			err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, sockopt, 1)
			if err != nil {
				syscall.Close(fd)
				return nil, err
			}
		} else if sockopt == TCPFastopen {
			if err := FastOpenAllowed(); err != nil {
				return nil, err
			}
			err = syscall.SetsockoptInt(fd, syscall.SOL_TCP, sockopt, 1)
			if err != nil {
				syscall.Close(fd)
				return nil, err
			}
		}
	}

	if err := syscall.Bind(fd, sockAddr); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	r.debug = true
	f := os.NewFile(uintptr(fd), "l")
	l.f = f
	go l.run()

	return l, nil
}
