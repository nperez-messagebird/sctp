package sctp

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	syscall "golang.org/x/sys/unix"
)

const (
	STREAM_TEST_CLIENTS = 10
	STREAM_TEST_STREAMS = 100
)

var r *rand.Rand

func init() {
	r = rand.New(rand.NewSource(time.Now().UnixNano()))
}

func randomString(length int) string {
	var rMu sync.Mutex
	rMu.Lock()
	defer rMu.Unlock()
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = chars[r.Intn(len(chars))]
	}
	return string(result)
}

type resolveSCTPAddrTest struct {
	network       SCTPAddressFamily
	litAddrOrName string
	addr          *SCTPAddr
	err           error
}

var ipv4loop = net.IPv4(127, 0, 0, 1)

var resolveSCTPAddrTests = []resolveSCTPAddrTest{
	{SCTP4, "127.0.0.1:0", &SCTPAddr{AddressFamily: SCTP4, IPAddrs: []net.IPAddr{net.IPAddr{IP: ipv4loop}}, Port: 0}, nil},
	{SCTP4, "127.0.0.1:65535", &SCTPAddr{AddressFamily: SCTP4, IPAddrs: []net.IPAddr{net.IPAddr{IP: ipv4loop}}, Port: 65535}, nil},

	{SCTP6, "[::1]:0", &SCTPAddr{AddressFamily: SCTP6, IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("::1")}}, Port: 0}, nil},
	{SCTP6, "[::1]:65535", &SCTPAddr{AddressFamily: SCTP6, IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("::1")}}, Port: 65535}, nil},

	{SCTP6, "[::1%lo0]:0", &SCTPAddr{AddressFamily: SCTP6, IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("::1"), Zone: "lo0"}}, Port: 0}, nil},
	{SCTP6, "[::1%lo0]:65535", &SCTPAddr{AddressFamily: SCTP6, IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("::1"), Zone: "lo0"}}, Port: 65535}, nil},
	{SCTP4, "0.0.0.0:12345", &SCTPAddr{AddressFamily: SCTP4, IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4zero, Zone: ""}}, Port: 12345}, nil},
	{SCTP4, "127.0.0.1/10.0.0.1:0", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}, net.IPAddr{IP: net.IPv4(10, 0, 0, 1)}}, Port: 0}, nil},
	{SCTP4, "127.0.0.1/10.0.0.1:65535", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}, net.IPAddr{IP: net.IPv4(10, 0, 0, 1)}}, Port: 65535}, nil},
	{SCTP6, "::1%lo0/127.0.0.1:1234", &SCTPAddr{AddressFamily: SCTP6, IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("::1"), Zone: "lo0"}, net.IPAddr{IP: ipv4loop, Zone: ""}}, Port: 1234}, nil},
}

func TestSCTPAddrString(t *testing.T) {
	for _, tt := range resolveSCTPAddrTests {
		s := tt.addr.String()
		if tt.litAddrOrName != s {
			t.Errorf("expected %q, got %q", tt.litAddrOrName, s)
		}
	}
}

func TestResolveSCTPAddr(t *testing.T) {
	for _, tt := range resolveSCTPAddrTests {
		addr, err := ResolveSCTPAddr(tt.network, tt.litAddrOrName)
		if !reflect.DeepEqual(addr, tt.addr) || !reflect.DeepEqual(err, tt.err) {
			t.Errorf("ResolveSCTPAddr(%q, %q) = %#v, %v, want %#v, %v", tt.network, tt.litAddrOrName, addr, err, tt.addr, tt.err)
			continue
		}
		if err == nil {
			addr2, err := ResolveSCTPAddr(addr.AddressFamily, addr.String())
			if !reflect.DeepEqual(addr2, tt.addr) || err != tt.err {
				t.Errorf("(%q, %q): ResolveSCTPAddr(%q, %q) = %#v, %v, want %#v, %v", tt.network, tt.litAddrOrName, addr.Network(), addr.String(), addr2, err, tt.addr, tt.err)
			}
		}
	}
}

var sctpListenerNameTests = []*SCTPAddr{
	&SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}}},
	&SCTPAddr{},
	nil,
	&SCTPAddr{Port: 7777},
}

func TestSCTPListenerName(t *testing.T) {
	for _, tt := range sctpListenerNameTests {
		ln, err := NewSCTPListener(tt, InitMsg{}, OneToOne, false)
		if err != nil {
			if tt == nil {
				continue
			}
			t.Fatal(err)
		}
		defer ln.Close()
		la := ln.LocalAddr()
		if a, ok := la.(*SCTPAddr); !ok || a.Port == 0 {
			t.Fatalf("got %v; expected a proper address with non-zero port number", la)
		}
	}
}

func TestSCTPConcurrentAccept(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	addr, _ := ResolveSCTPAddr(SCTP4, "127.0.0.1:0")
	ln, err := NewSCTPListener(addr, InitMsg{}, OneToMany, false)
	if err != nil {
		t.Fatal(err)
	}

	raddr, err := ln.SCTPLocalAddr(0)
	if err != nil {
		t.Fatal(err)
	}

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					break
				}
				c.Close()
			}
			wg.Done()
		}()
	}
	attempts := 10 * N
	fails := 0
	for i := 0; i < attempts; i++ {
		c, err := NewSCTPConnection(raddr.AddressFamily, InitMsg{}, OneToOne, false)
		if err != nil {
			fails++
		} else {
			c.Close()
		}
	}
	ln.Close()
	// BUG Accept() doesn't return even if we closed ln
	//	wg.Wait()
	if fails > 0 {
		t.Fatalf("# of failed Dials: %v", fails)
	}
}

func TestSCTPCloseRecv(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	addr, _ := ResolveSCTPAddr(SCTP4, "127.0.0.1:0")
	ln, err := NewSCTPListener(addr, InitMsg{}, OneToOne, false)
	if err != nil {
		t.Fatal(err)
	}

	raddr, err := ln.SCTPLocalAddr(0)
	if err != nil {
		t.Fatal(err)
	}

	var conn net.Conn
	var wg sync.WaitGroup
	connReady := make(chan struct{}, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var xerr error
		conn, xerr = ln.Accept()
		if xerr != nil {
			t.Fatal(xerr)
		}
		connReady <- struct{}{}
		buf := make([]byte, 256)
		_, xerr = conn.Read(buf)
		t.Logf("got error while read: %v", xerr)
		if xerr != io.EOF && xerr != syscall.EBADF {
			t.Fatalf("read failed: %v", xerr)
		}
	}()

	c, err := NewSCTPConnection(raddr.AddressFamily, InitMsg{}, OneToOne, false)
	if err != nil {
		t.Fatalf("failed to dial: %s", err)
	}

	if err := c.Connect(raddr); err != nil {
		t.Fatalf("failed to dial: %s", err)
	}

	<-connReady
	err = conn.Close()
	if err != nil {
		t.Fatalf("close failed: %v", err)
	}
	wg.Wait()
}

func TestSCTPConcurrentOneToMany(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	addr, _ := ResolveSCTPAddr(SCTP4, "127.0.0.1:0")
	ln, err := NewSCTPListener(addr, InitMsg{}, OneToMany, false)
	if err != nil {
		t.Fatal(err)
	}

	raddr, err := ln.SCTPLocalAddr(0)
	if err != nil {
		t.Fatal(err)
	}

	ln.SetEvents(SCTP_EVENT_DATA_IO | SCTP_EVENT_ASSOCIATION)

	const N = 10
	for i := 0; i < N; i++ {
		go func() {
			for {
				buf := make([]byte, 512)
				n, _, flags, err := ln.SCTPRead(buf)
				if err != nil {
					break
				}

				if flags&MSG_NOTIFICATION > 0 {
					notif, _ := SCTPParseNotification(buf[:n])
					switch notif.Type() {
					case SCTP_ASSOC_CHANGE:
						assocChange := notif.GetAssociationChange()
						if assocChange.State == SCTP_COMM_UP {
							ln.SCTPWrite([]byte{0}, &SndRcvInfo{Flags: SCTP_EOF, AssocID: assocChange.AssocID})
						}
					}
				}
			}
		}()
	}
	attempts := 10 * N
	fails := 0
	for i := 0; i < attempts; i++ {
		c, err := NewSCTPConnection(raddr.AddressFamily, InitMsg{}, OneToOne, false)
		if err != nil {
			fails++
		}
		if err := c.Connect(raddr); err != nil {
			fails++
		}
	}
	ln.Close()
	if fails > 0 {
		t.Fatalf("# of failed Dials: %v", fails)
	}
}

func TestOneToManyPeelOff(t *testing.T) {

	const (
		SERVER_ROUTINE_COUNT = 10
		CLIENT_ROUTINE_COUNT = 100
	)
	var wg sync.WaitGroup
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	addr, _ := ResolveSCTPAddr(SCTP4, "127.0.0.1:0")
	ln, err := NewSCTPListener(addr, InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS}, OneToMany, false)
	if err != nil {
		t.Fatal(err)
	}

	laddr, _ := ln.LocalAddr().(*SCTPAddr)

	ln.SetEvents(SCTP_EVENT_ASSOCIATION)

	go func() {
		test := 999
		count := 0
		for {
			t.Logf("[%d]Reading from server socket...\n", test)
			buf := make([]byte, 512)
			n, oob, flags, err := ln.SCTPRead(buf)
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatalf("[%d]Got an error reading from main socket", test)
			}

			if flags&MSG_NOTIFICATION > 0 {
				t.Logf("[%d]Got a notification. Bytes read: %v\n", test, n)
				notif, _ := SCTPParseNotification(buf[:n])
				switch notif.Type() {
				case SCTP_ASSOC_CHANGE:
					t.Logf("[%d]Got an association change notification\n", test)
					assocChange := notif.GetAssociationChange()
					if assocChange.State == SCTP_COMM_UP {
						t.Logf("[%d]SCTP_COMM_UP. Creating socket for association: %v\n", test, assocChange.AssocID)
						newSocket, err := ln.PeelOff(assocChange.AssocID)
						if err != nil {
							t.Fatalf("Failed to peel off socket: %v", err)
						}
						t.Logf("[%d]Peeled off socket: %#+v\n", test, newSocket)
						if err := newSocket.SetEvents(SCTP_EVENT_DATA_IO); err != nil {
							t.Logf("[%d]Failed to subscribe to data io for peeled off socket: %v -> %#+v\n", test, err, newSocket)
						}
						count++
						go socketReaderMirror(newSocket, t, test-count)
						continue
					}
				}
			}

			if flags&MSG_EOR > 0 {
				info := oob.GetSndRcvInfo()
				t.Logf("[%d]Got data on main socket, but it wasn't a notification: %#+v \n", test, info)
				wn, werr := ln.SCTPWrite(buf[:n],
					&SndRcvInfo{
						AssocID: info.AssocID,
						Stream:  info.Stream,
						PPID:    info.PPID,
					},
				)
				if werr != nil {
					t.Errorf("[%d]failed to write %s, len: %d, err: %v, bytes written: %d, info: %+v", test, string(buf[:n]), len(buf[:n]), werr, wn, info)
					return
				}
				continue
			}
			t.Logf("[%d]No clue wtf is happening", test)
		}
	}()

	for i := CLIENT_ROUTINE_COUNT; i > 0; i-- {
		wg.Add(1)
		go func(client int, l *SCTPAddr) {
			defer wg.Done()
			t.Logf("[%d]Creating new client connection\n", client)
			c, err := NewSCTPConnection(l.AddressFamily, InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS}, OneToOne, false)
			if err != nil {
				t.Fatalf("[%d]Failed to connect to SCTP server: %v", client, err)
			}
			if err := c.Connect(l); err != nil {
				t.Fatalf("[%d]Failed to connect to SCTP server: %v", client, err)
			}

			c.SetEvents(SCTP_EVENT_DATA_IO)
			for q := range []int{0, 1} {
				rstring := randomString(10)
				rstream := uint16(r.Intn(STREAM_TEST_STREAMS))
				t.Logf("[%d]Writing to client socket. Data:%v, Stream:%v, MsgCount:%v \n", client, rstring, rstream, q)
				_, err = c.SCTPWrite(
					[]byte(rstring),
					&SndRcvInfo{
						Stream: rstream,
						PPID:   uint32(q),
					},
				)
				if err != nil {
					t.Fatalf("Failed to send data to SCTP server: %v", err)
				}

				t.Logf("[%d]Reading from client socket...\n", client)
				buf := make([]byte, 512)
				n, oob, _, err := c.SCTPRead(buf)
				if err != nil {
					t.Fatalf("Failed to read from client socket: %v", err)
				}
				if oob == nil {
					t.Fatal("WTF. OOB is nil?!")
				}
				t.Logf("[%d]***Read from client socket\n", client)
				if oob.GetSndRcvInfo().Stream != rstream {
					t.Fatalf("Data received on a stream(%v) we didn't send(%v) on",
						oob.GetSndRcvInfo().Stream,
						rstream)
				}
				if string(buf[:n]) != rstring {
					t.Fatalf("Data from server doesn't match what client sent\nSent: %v\nReceived: %v",
						rstring,
						string(buf[:n]),
					)
				}
				t.Logf("[%d]Client read success! MsgCount: %v\n", client, q)
			}
			c.Close()

		}(i, laddr)
	}
	wg.Wait()
	ln.Close()
}

func socketReaderMirror(sock *SCTPConn, t *testing.T, goroutine int) {
	for {
		t.Logf("[%d]Reading peel off server socket...\n", goroutine)
		buf := make([]byte, 512)
		n, oob, flags, err := sock.SCTPRead(buf)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF || err == syscall.ENOTCONN {
				t.Logf("[%d]Got EOF...\n", goroutine)
				sock.Close()
				break
			}
			t.Fatalf("[%d]Failed to read from socket: %#+v", goroutine, err)
		}

		if flags&MSG_NOTIFICATION > 0 {
			t.Logf("[%d]Notification received. Byte count: %v, OOB: %#+v, Flags: %v\n", goroutine, n, oob, flags)
			if notif, err := SCTPParseNotification(buf[:n]); err == nil {
				t.Logf("[%d]Notification type: %v\n", goroutine, notif.Type().String())
			}
		}
		t.Logf("[%d]Writing peel off server socket...\n", goroutine)
		info := oob.GetSndRcvInfo()
		wn, werr := sock.SCTPWrite(buf[:n],
			&SndRcvInfo{
				AssocID: info.AssocID,
				Stream:  info.Stream,
				PPID:    info.PPID,
			},
		)
		if werr != nil {
			t.Errorf("[%d]failed to write %s, len: %d, err: %v, bytes written: %d, info: %+v", goroutine, string(buf[:n]), len(buf[:n]), werr, wn, info)
			return
		}
	}
}

func TestNonBlockingServerOneToMany(t *testing.T) {
	addr, _ := ResolveSCTPAddr(SCTP4, "127.0.0.1:0")
	ln, err := NewSCTPListener(addr, InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS}, OneToMany, true)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	raddr := ln.LocalAddr().(*SCTPAddr)
	t.Logf("Listening on: %v\n", raddr)

	ln.SetEvents(SCTP_EVENT_DATA_IO)

	t.Logf("Starting main server loop...\n")
	go func() {
		type ready struct {
			SndRcvInfo *SndRcvInfo
			Data       []byte
		}
		b := make(map[int32]map[uint16]bytes.Buffer)
		c := make([]*ready, 0)
		for {
			buf := make([]byte, 64)
			t.Logf("Server read\n")
			n, oob, flags, err := ln.SCTPRead(buf)
			if err != nil {
				switch err {
				case syscall.EAGAIN:
					goto WRITE
				case syscall.EBADF:
					return
				case syscall.ENOTCONN:
					return
				default:
					t.Fatalf("Server socket error: %v", err)
				}
			}

			t.Logf("DATA: %v, N: %d, OOB: %#+v, FLAGS: %d, ERR: %v\n", buf[:n], n, oob, flags, err)

			if flags&MSG_EOR > 0 {
				info := oob.GetSndRcvInfo()
				assocId := info.AssocID
				if _, ok := b[assocId]; !ok {
					b[assocId] = make(map[uint16]bytes.Buffer)
				}
				bucket := b[assocId]

				stream := bucket[info.Stream]
				stream.Write(buf[:n])

				data := stream.Bytes()
				dataCopy := make([]byte, stream.Len())
				copy(dataCopy, data)

				stream.Reset()

				sndrcv := &SndRcvInfo{Stream: info.Stream, AssocID: info.AssocID}
				c = append(c, &ready{SndRcvInfo: sndrcv, Data: dataCopy})
				t.Logf("Write data queued: %#+v\n", c)

			} else {
				info := oob.GetSndRcvInfo()
				assocId := info.AssocID
				if _, ok := b[assocId]; !ok {
					b[assocId] = make(map[uint16]bytes.Buffer)
				}
				bucket := b[assocId]

				stream := bucket[info.Stream]
				stream.Write(buf[:n])

				t.Logf("No EOR\n")
			}
		WRITE:
			for {
				if len(c) > 0 {
					var r *ready
					r = c[0]
					c = c[1:]
					t.Logf("Writing: %v, %#+v\n", r.Data, r.SndRcvInfo)
					_, err := ln.SCTPWrite(r.Data, r.SndRcvInfo)
					if err != nil {
						if err == syscall.EWOULDBLOCK {
							t.Logf("WRITE EWOULDBLOCK\n")
							c = append(c, r)
							break
						}
						t.Logf("Something went wrong?: %v", err)
					}
				} else {
					t.Logf("No queued writes\n")
					break
				}
			}

			<-time.Tick(time.Millisecond * 10)
			t.Logf("tick!\n")
		}
	}()

	t.Logf("Starting client connections...\n")
	var wg sync.WaitGroup
	for i := 0; i < STREAM_TEST_CLIENTS; i++ {
		wg.Add(1)
		go func(test int) {
			defer wg.Done()

			conn, err := NewSCTPConnection(SCTP6,
				InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS},
				OneToOne, false)
			if err != nil {
				t.Errorf("failed to dial address %s, test #%d: %v", raddr.String(), test, err)
				return
			}
			t.Logf("Connecting to: %v...", raddr)
			if err := conn.Connect(raddr); err != nil {
				t.Fatalf("Failed to connect to server: %v", err)
			}
			t.Logf("Success!\n")
			defer conn.Close()
			conn.SetEvents(SCTP_EVENT_DATA_IO)
			for ppid := uint16(0); ppid < STREAM_TEST_STREAMS; ppid++ {
				info := &SndRcvInfo{
					Stream: uint16(ppid),
					PPID:   uint32(ppid),
				}
				randomLen := r.Intn(5) + 1
				text := fmt.Sprintf("[%s,%d,%d]", randomString(randomLen), test, ppid)
				t.Logf("Sending data to server: %v\n", text)
				n, err := conn.SCTPWrite([]byte(text), info)
				if err != nil {
					t.Errorf("failed to write %s, len: %d, err: %v, bytes written: %d, info: %+v", text, len(text), err, n, info)
					return
				}
				var b bytes.Buffer
				for {
					buf := make([]byte, 64)
					cn, oob, flags, err := conn.SCTPRead(buf)
					t.Logf("Client read data count: %d", cn)
					if err != nil {
						if err == io.EOF || err == io.ErrUnexpectedEOF {
							if cn == 0 {
								break
							}
							t.Logf("EOF on server connection. Total bytes received: %d, bytes received: %d", len(b.Bytes()), cn)
						} else {
							t.Errorf("Client connection read err: %v. Total bytes received: %d, bytes received: %d", err, len(b.Bytes()), cn)
							return
						}
					}

					b.Write(buf[:cn])

					if flags&MSG_EOR > 0 {
						if oob.GetSndRcvInfo().Stream != ppid {
							t.Errorf("Mismatched PPIDs: %d != %d", oob.GetSndRcvInfo().Stream, ppid)
							return
						}
						rtext := string(b.Bytes())
						b.Reset()
						if rtext != text {
							t.Fatalf("Mismatched payload: %s != %s", []byte(rtext), []byte(text))
						}
						t.Logf("Data read from server matched what we sent")

						break
					}
				}
			}
		}(i)
	}

	wg.Wait()
	ln.Close()
}

func TestStreamsOneToOne(t *testing.T) {
	addr, _ := ResolveSCTPAddr(SCTP4, "127.0.0.1:0")
	ln, err := NewSCTPListener(addr, InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS}, OneToOne, false)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr = ln.LocalAddr().(*SCTPAddr)

	go func() {
		for {
			c, err := ln.Accept()
			sconn := c.(*SCTPConn)
			if err != nil {
				t.Errorf("failed to accept: %v", err)
				return
			}
			defer sconn.Close()

			sconn.SetEvents(SCTP_EVENT_DATA_IO | SCTP_EVENT_ASSOCIATION)

			go func() {
				totalrcvd := 0
				var b bytes.Buffer
				for {
					buf := make([]byte, 64)
					n, oob, flags, err := sconn.SCTPRead(buf)
					if err != nil {
						if err == io.EOF || err == io.ErrUnexpectedEOF {
							if n == 0 {
								break
							}
							t.Logf("EOF on server connection. Total bytes received: %d, bytes received: %d", totalrcvd, n)
						} else {
							t.Errorf("Server connection read err: %v. Total bytes received: %d, bytes received: %d", err, totalrcvd, n)
							return
						}
					}

					b.Write(buf[:n])

					if flags&MSG_NOTIFICATION > 0 {
						if !(flags&MSG_EOR > 0) {
							t.Log("buffer not large enough for notification")
							continue
						}
					} else if flags&MSG_EOR > 0 {
						info := oob.GetSndRcvInfo()
						data := b.Bytes()
						n, err = sconn.SCTPWrite(data, &SndRcvInfo{
							Stream: info.Stream,
							PPID:   info.PPID,
						})
						if err != nil {
							t.Error(err)
							return
						}
					} else {
						t.Logf("No flags match?: %v", flags&MSG_EOR)
					}

					b.Reset()
				}
			}()
		}
	}()

	wait := make(chan struct{})
	i := 0
	for ; i < STREAM_TEST_CLIENTS; i++ {
		go func(test int) {
			defer func() { wait <- struct{}{} }()
			conn, err := NewSCTPConnection(addr.AddressFamily,
				InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS},
				OneToOne, false)
			if err != nil {
				t.Errorf("failed to dial address %s, test #%d: %v", addr.String(), test, err)
				return
			}
			conn.Connect(addr)
			defer conn.Close()
			conn.SetEvents(SCTP_EVENT_DATA_IO)
			for ppid := uint16(0); ppid < STREAM_TEST_STREAMS; ppid++ {
				info := &SndRcvInfo{
					Stream: uint16(ppid),
					PPID:   uint32(ppid),
				}
				randomLen := r.Intn(5) + 1
				text := fmt.Sprintf("[%s,%d,%d]", randomString(randomLen), test, ppid)
				n, err := conn.SCTPWrite([]byte(text), info)
				if err != nil {
					t.Errorf("failed to write %s, len: %d, err: %v, bytes written: %d, info: %+v", text, len(text), err, n, info)
					return
				}
				var b bytes.Buffer
				for {
					buf := make([]byte, 64)
					cn, oob, flags, err := conn.SCTPRead(buf)
					if err != nil {
						if err == io.EOF || err == io.ErrUnexpectedEOF {
							if cn == 0 {
								break
							}
							t.Logf("EOF on server connection. Total bytes received: %d, bytes received: %d", len(b.Bytes()), cn)
						} else {
							t.Errorf("Client connection read err: %v. Total bytes received: %d, bytes received: %d", err, len(b.Bytes()), cn)
							return
						}
					}

					b.Write(buf[:cn])

					if flags&MSG_NOTIFICATION > 0 {
						if !(flags&MSG_EOR > 0) {
							t.Log("buffer not large enough for notification")
							continue
						}
					} else if flags&MSG_EOR > 0 {
						if oob.GetSndRcvInfo().Stream != ppid {
							t.Errorf("Mismatched PPIDs: %d != %d", oob.GetSndRcvInfo().Stream, ppid)
							return
						}
						rtext := string(b.Bytes())
						b.Reset()
						if rtext != text {
							t.Fatalf("Mismatched payload: %s != %s", []byte(rtext), []byte(text))
						}

						break
					}
				}
			}
		}(i)
	}
	for ; i > 0; i-- {
		select {
		case <-wait:
		case <-time.After(time.Second * 30):
			close(wait)
			t.Fatal("timed out")
		}
	}
}

func TestStreamsOneToMany(t *testing.T) {
	addr, _ := ResolveSCTPAddr(SCTP4, "127.0.0.1:0")
	ln, err := NewSCTPListener(addr, InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS}, OneToMany, false)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr = ln.LocalAddr().(*SCTPAddr)

	ln.SetEvents(SCTP_EVENT_DATA_IO)

	t.Log("Spinning up server goroutine")
	go func() {
		var b bytes.Buffer
		for {
			buf := make([]byte, 64)
			n, oob, flags, err := ln.SCTPRead(buf)
			t.Logf("Server read data count: %d", n)
			if err != nil {
				t.Errorf("Server connection read err: %v", err)
				return
			}

			b.Write(buf[:n])

			if flags&MSG_EOR > 0 {
				info := oob.GetSndRcvInfo()
				data := b.Bytes()
				t.Logf("Server received data: %s", string(data))
				n, err = ln.SCTPWrite(data, &SndRcvInfo{
					Stream:  info.Stream,
					PPID:    info.PPID,
					AssocID: info.AssocID,
				})

				b.Reset()

				if err != nil {
					t.Error(err)
					return
				}
			} else {
				t.Logf("No flags match?: %v", flags&MSG_EOR)
			}

		}
	}()

	wait := make(chan struct{})
	i := 0
	t.Log("Spinning up clients")
	for ; i < STREAM_TEST_CLIENTS; i++ {
		go func(test int) {
			defer func() { wait <- struct{}{} }()
			t.Log("Creating client connection")
			conn, err := NewSCTPConnection(addr.AddressFamily,
				InitMsg{NumOstreams: STREAM_TEST_STREAMS, MaxInstreams: STREAM_TEST_STREAMS},
				OneToOne, false)
			if err != nil {
				t.Errorf("failed to dial address %s, test #%d: %v", addr.String(), test, err)
				return
			}
			conn.Connect(addr)
			defer conn.Close()
			conn.SetEvents(SCTP_EVENT_DATA_IO)
			for ppid := uint16(0); ppid < STREAM_TEST_STREAMS; ppid++ {
				info := &SndRcvInfo{
					Stream: uint16(ppid),
					PPID:   uint32(ppid),
				}
				randomLen := r.Intn(5) + 1
				text := fmt.Sprintf("[%s,%d,%d]", randomString(randomLen), test, ppid)
				t.Logf("Sending data to server: %v", text)
				n, err := conn.SCTPWrite([]byte(text), info)
				if err != nil {
					t.Errorf("failed to write %s, len: %d, err: %v, bytes written: %d, info: %+v", text, len(text), err, n, info)
					return
				}
				var b bytes.Buffer
				for {
					buf := make([]byte, 64)
					cn, oob, flags, err := conn.SCTPRead(buf)
					t.Logf("Client read data count: %d", cn)
					if err != nil {
						if err == io.EOF || err == io.ErrUnexpectedEOF {
							if cn == 0 {
								break
							}
							t.Logf("EOF on server connection. Total bytes received: %d, bytes received: %d", len(b.Bytes()), cn)
						} else {
							t.Errorf("Client connection read err: %v. Total bytes received: %d, bytes received: %d", err, len(b.Bytes()), cn)
							return
						}
					}

					b.Write(buf[:cn])

					if flags&MSG_EOR > 0 {
						if oob.GetSndRcvInfo().Stream != ppid {
							t.Errorf("Mismatched PPIDs: %d != %d", oob.GetSndRcvInfo().Stream, ppid)
							return
						}
						rtext := string(b.Bytes())
						b.Reset()
						if rtext != text {
							t.Fatalf("Mismatched payload: %s != %s", []byte(rtext), []byte(text))
						}
						t.Log("Data read from server matched what we sent")

						break
					}
				}
			}
		}(i)
	}
	for ; i > 0; i-- {
		select {
		case <-wait:
		case <-time.After(time.Second * 10):
			close(wait)
			t.Fatal("timed out")
		}
	}
	ln.Close()
}
