//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command server is a QUIC echo server without HTTP/3: fib's QUIC
// transport (package fib/http3/quic) on a UDP binding, echoing every
// stream a client opens, as it arrives, and ending it when the client ends
// its side.
//
//	go run ./quic/server
//
// A quic.Conn does no I/O of its own. fib reads the socket and gives each
// peer address a fib.Connection, which is the quic.PacketConn the
// connection sends through; the handler here feeds each connection the
// datagrams of its address: a client's first Initial goes to quic.Accept,
// the rest to Conn.HandleDatagrams, a whole burst at a time. Routing by
// address is enough for an example; a server whose clients change address
// under a NAT routes by connection ID instead, as fib/http3 does.
//
// QUIC is always TLS 1.3. Without -cert and -key the server issues itself
// a certificate and writes it where the client looks for it. The ALPN is
// "fib-echo", which the client must offer.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/http3/quic"
)

// alpn is the application protocol both ends agree on in the handshake.
const alpn = "fib-echo"

// fatal reports a startup error and exits; a test replaces it.
var fatal = example.Fatal

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fatal(err)
	}
}

// run serves until ctx ends. ready, if not nil, is told the address once the
// server is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:9004", "listen address")
	certFlags := certs.ServerFlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	tlsConfig, err := certFlags.Config()
	if err != nil {
		return err
	}
	tlsConfig.NextProtos = []string{alpn}
	resetKey := quic.NewResetKey()
	srv := &server{
		out: out,
		config: quic.Config{
			TLSConfig: tlsConfig,
			// Lets a client that lost its state learn at once that the
			// connection is gone, rather than wait for its idle timeout.
			ResetKey: &resetKey,
		},
		conns: map[*fib.Connection]*quic.Conn{},
	}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *addr, Handler: srv})
	if err != nil {
		return err
	}
	return example.Serve(ctx, out, ready, "QUIC echo server listening on %s (ALPN "+alpn+")", engine)
}

// server is the fib.Handler of the UDP binding: it hands each peer
// address's datagrams to that address's QUIC connection.
type server struct {
	out    io.Writer
	config quic.Config

	mu    sync.Mutex
	conns map[*fib.Connection]*quic.Conn
}

func (s *server) OnOpen(*fib.Connection)                 {}
func (s *server) OnPriorityData(*fib.Connection, []byte) {}

func (s *server) OnData(c *fib.Connection, data []byte) { s.OnDatagrams(c, [][]byte{data}) }

// OnDatagrams takes the datagrams that arrived from one address together,
// so the connection answers them in as few datagrams as it can.
func (s *server) OnDatagrams(c *fib.Connection, datagrams [][]byte) {
	s.mu.Lock()
	qc := s.conns[c]
	s.mu.Unlock()
	for i, d := range datagrams {
		if qc != nil {
			qc.HandleDatagrams(datagrams[i:])
			return
		}
		if vn := quic.VersionNegotiation(d); vn != nil {
			_ = c.Send(vn) // a QUIC version this server does not speak
			continue
		}
		if !quic.IsInitial(d) {
			continue // not a connection's start, and no connection here
		}
		if qc = quic.Accept(c, c.RemoteAddr(), s.config, &echoConn{out: s.out}, d); qc != nil {
			s.mu.Lock()
			s.conns[c] = qc
			s.mu.Unlock()
		}
	}
}

// OnClose forgets the connection of an address once fib closes it, which
// it does when the QUIC connection is over and releases its path.
func (s *server) OnClose(c *fib.Connection, _ error) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// OnEngineClose tells each client the server is going, with
// CONNECTION_CLOSE, while the socket can still send.
func (s *server) OnEngineClose(c *fib.Connection) {
	s.mu.Lock()
	qc := s.conns[c]
	s.mu.Unlock()
	if qc != nil {
		qc.Close(0, "server shutting down")
	}
}

// echoConn is the quic.Handler of one connection. Its methods run one at a
// time per connection, with no lock held, so they may write to streams.
type echoConn struct{ out io.Writer }

func (h *echoConn) OnHandshake(c *quic.Conn) {
	fmt.Fprintf(h.out, "conn %s: open, ALPN %q\n", c.RemoteAddr(), c.ConnectionState().NegotiatedProtocol)
}

// OnStreamData echoes a stream's data as it comes, and ends the stream
// with the client's end. The first call for a stream is how it becomes
// known.
func (h *echoConn) OnStreamData(st *quic.Stream, data []byte, fin bool) {
	if err := st.Write(data, fin); err != nil {
		fmt.Fprintf(h.out, "stream %d: echo failed: %v\n", st.ID(), err)
	}
}

func (h *echoConn) OnStreamReset(*quic.Stream, uint64) {}
func (h *echoConn) OnStopSending(*quic.Stream, uint64) {}
func (h *echoConn) OnStreamsAvailable(*quic.Conn)      {}

func (h *echoConn) OnClose(c *quic.Conn, err error) {
	fmt.Fprintf(h.out, "conn %s: closed: %v\n", c.RemoteAddr(), err)
}

var (
	_ fib.DatagramsHandler   = (*server)(nil)
	_ fib.EngineCloseHandler = (*server)(nil)
	_ quic.Handler           = (*echoConn)(nil)
)
