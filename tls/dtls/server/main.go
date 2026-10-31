//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command server is a DTLS 1.2 echo server on a fib UDP binding, with fib's
// sans-IO DTLS (the one WebRTC runs on), negotiating an SRTP profile as a
// WebRTC peer does.
//
//	go run ./tls/dtls/server
//	go run ./tls/dtls/client -fingerprint <the fingerprint it prints>
//
// Certificates are self-signed, as WebRTC's are, and each side checks the
// other's by its SHA-256 fingerprint, which WebRTC carries in SDP and these
// examples on the command line. The server asks the client for a
// certificate too, and prints the fingerprint it gets.
//
// The binding's handler keeps one DTLS connection per client address,
// created on its first datagram. A client must first echo the stateless
// HelloVerifyRequest cookie, so the server holds nothing for an address
// that does not receive at it. Each side prints what the handshake
// negotiated and 16 bytes of the SRTP keying material it exports (RFC
// 5764), the same on both ends.
package main

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib-examples/tls/dtls/session"
	fibtls "github.com/lesismal/fib/tls"
)

// fatal reports a startup error and exits; a test replaces it.
var fatal = example.Fatal

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fatal(err)
	}
}

// client is one client's DTLS connection and the fib connection its
// datagrams last came on, which replies go out through.
type client struct {
	s    *session.Session
	mu   sync.Mutex
	conn *fib.Connection
}

func (c *client) send(b []byte) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	_ = conn.Send(b) // Send copies: b is valid only during the call
}

// run serves until ctx ends. ready, if not nil, is told the address once the
// server is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:4444", "listen address (UDP)")
	idle := flags.Duration("idle", time.Minute, "forget a client after this long without a datagram")
	if err := flags.Parse(args); err != nil {
		return err
	}
	out = &lockedWriter{w: out} // clients print from their own goroutines

	cert, err := fibtls.GenerateWebRTCCertificate()
	if err != nil {
		return err
	}
	config := &stdtls.Config{
		Certificates: []stdtls.Certificate{cert},
		// Any certificate, checked by fingerprint rather than by a CA:
		// here it is only printed, where WebRTC compares it with SDP's.
		ClientAuth: stdtls.RequireAnyClientCert,
	}

	var mu sync.Mutex
	clients := map[netip.AddrPort]*client{}
	handler := fib.HandlerFuncs{
		Data: func(c *fib.Connection, data []byte) {
			from := c.RemoteAddrPort()
			mu.Lock()
			cl := clients[from]
			if cl == nil {
				cl = &client{conn: c}
				opts := fibtls.DTLSOptions{
					SRTPProfiles: session.SRTPProfiles,
					// The cookie covers the client's address.
					CookieContext: []byte(from.String()),
				}
				s, err := session.New(false, config, cl.send, opts, session.Callbacks{
					OnHandshake: func(s *session.Session) { fmt.Fprintf(out, "%s: %s\n", from, s.Describe()) },
					OnData:      func(s *session.Session, b []byte) { _ = s.Write(b) }, // echo
					OnClose: func(*session.Session, error) {
						fmt.Fprintf(out, "%s: closed\n", from)
						mu.Lock()
						delete(clients, from)
						mu.Unlock()
					},
				})
				if err != nil {
					mu.Unlock()
					return
				}
				cl.s = s
				clients[from] = cl
			}
			mu.Unlock()
			cl.mu.Lock()
			cl.conn = c
			cl.mu.Unlock()
			cl.s.HandleDatagram(data)
		},
		// A client that went quiet: its DTLS state goes with it.
		Close: func(c *fib.Connection, _ error) {
			mu.Lock()
			cl := clients[c.RemoteAddrPort()]
			delete(clients, c.RemoteAddrPort())
			mu.Unlock()
			if cl != nil {
				cl.s.Close()
			}
		},
	}

	fibConfig := example.EngineConfig()
	fibConfig.UDPIdleTimeout = *idle
	engine, err := fib.New(fibConfig, fib.Binding{Network: "udp", Addr: *addr, Handler: handler})
	if err != nil {
		return err
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	fmt.Fprintf(out, "fingerprint sha-256 %s\n", fibtls.CertificateFingerprint(leaf.Raw))
	return example.Serve(ctx, out, ready, "DTLS echo server listening on %s (UDP)", engine)
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
