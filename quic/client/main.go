//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command client opens a stream per message on one QUIC connection to the
// QUIC echo server and prints each echo.
//
//	go run ./quic/client
//
// The client engine dials a UDP connection to the server, and quic.Dial
// starts the handshake through it; the connection's fib.Handler feeds the
// QUIC connection the datagrams that come back. Each message goes on a
// bidirectional stream of its own, written with fin, and the echo is
// whatever arrives on that stream until the server's fin: streams carry
// bytes, not messages, so the end of the stream is the end of the echo.
//
// The client trusts the certificate the server wrote (-ca), or any with
// -insecure, and offers the ALPN "fib-echo".
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/http3/quic"
)

const alpn = "fib-echo"

// fatal reports an error and exits; a test replaces it.
var fatal = example.Fatal

// timeout bounds the handshake and the wait for each echo; a test shortens
// it.
var timeout = 5 * time.Second

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fatal(err)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:9004", "server address")
	count := flags.Int("n", 5, "messages to send, one stream each")
	certFlags := certs.ClientFlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	tlsConfig, err := certFlags.Config()
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	tlsConfig.ServerName = host
	tlsConfig.NextProtos = []string{alpn}

	engine, stop := example.ClientEngine()
	defer stop()

	h := &client{handshake: make(chan struct{}), closed: make(chan error, 1), echoes: map[uint64]*echo{}}
	dialed := make(chan error, 1)
	err = engine.DialWithHandler("udp", *addr, timeout, path{h}, func(fc *fib.Connection, err error) {
		if err != nil {
			dialed <- err
			return
		}
		qc, err := quic.Dial(fc, fc.RemoteAddr(), quic.Config{TLSConfig: tlsConfig, HandshakeTimeout: timeout}, h)
		if err != nil {
			fc.Close()
		} else {
			h.qc.Store(qc)
		}
		dialed <- err
	})
	if err == nil {
		err = <-dialed
	}
	if err != nil {
		return err
	}
	qc := h.qc.Load()
	// Close sends CONNECTION_CLOSE, which tells the server at once rather
	// than at its idle timeout; wait for it to go before the engine stops.
	defer func() {
		qc.Close(0, "done")
		select {
		case <-h.closed:
		case <-time.After(time.Second):
		}
	}()

	select {
	case <-h.handshake:
		fmt.Fprintf(out, "connected, ALPN %q, TLS 1.3 cipher suite %#04x\n",
			qc.ConnectionState().NegotiatedProtocol, qc.ConnectionState().CipherSuite)
	case err := <-h.closed:
		return fmt.Errorf("handshake failed: %v", err)
	case <-time.After(timeout):
		return errors.New("no handshake: is the server running?")
	}

	for i := 1; i <= *count; i++ {
		st, err := qc.OpenStream()
		if err != nil {
			return err
		}
		e := h.expect(st.ID())
		if err := st.Write([]byte(example.Message(i)), true); err != nil {
			return err
		}
		select {
		case <-e.done:
			fmt.Fprintf(out, "echo on stream %d: %s\n", st.ID(), e.buf.Bytes())
		case err := <-h.closed:
			return fmt.Errorf("connection closed: %v", err)
		case <-time.After(timeout):
			return fmt.Errorf("no echo on stream %d", st.ID())
		}
	}
	return nil
}

// echo is what has come back on one stream, done once the server ended it.
type echo struct {
	buf  bytes.Buffer
	done chan struct{}
}

// client is the quic.Handler of the connection.
type client struct {
	qc        atomic.Pointer[quic.Conn]
	handshake chan struct{}
	closed    chan error

	mu     sync.Mutex
	echoes map[uint64]*echo
}

// expect registers a stream before anything is written on it, so that
// its echo has somewhere to go.
func (c *client) expect(id uint64) *echo {
	e := &echo{done: make(chan struct{})}
	c.mu.Lock()
	c.echoes[id] = e
	c.mu.Unlock()
	return e
}

func (c *client) OnHandshake(*quic.Conn) { close(c.handshake) }

func (c *client) OnStreamData(st *quic.Stream, data []byte, fin bool) {
	c.mu.Lock()
	e := c.echoes[st.ID()]
	c.mu.Unlock()
	if e == nil {
		return
	}
	e.buf.Write(data)
	if fin {
		close(e.done)
	}
}

func (c *client) OnStreamReset(*quic.Stream, uint64) {}
func (c *client) OnStopSending(*quic.Stream, uint64) {}
func (c *client) OnStreamsAvailable(*quic.Conn)      {}

func (c *client) OnClose(_ *quic.Conn, err error) {
	select {
	case c.closed <- err:
	default:
	}
}

// path is the fib.Handler of the client's UDP connection: it feeds the
// QUIC connection the datagrams the server sends.
type path struct{ c *client }

func (p path) OnOpen(*fib.Connection)                 {}
func (p path) OnPriorityData(*fib.Connection, []byte) {}
func (p path) OnClose(*fib.Connection, error)         {}

func (p path) OnData(_ *fib.Connection, data []byte) {
	if qc := p.c.qc.Load(); qc != nil {
		qc.HandleDatagram(data)
	}
}

func (p path) OnDatagrams(_ *fib.Connection, datagrams [][]byte) {
	if qc := p.c.qc.Load(); qc != nil {
		qc.HandleDatagrams(datagrams)
	}
}
