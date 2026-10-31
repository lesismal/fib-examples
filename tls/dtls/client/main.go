//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command client is the DTLS 1.2 echo client for ../server: it shakes hands,
// checks the server's certificate by its SHA-256 fingerprint, prints what was
// negotiated and the SRTP keying material it exports, sends -n messages and
// prints their echoes.
//
//	go run ./tls/dtls/client -fingerprint AB:CD:...
//
// Its socket is a UDP binding of its own engine. A UDP binding opens a
// connection for an address that sends to it; a client speaks first, so
// Engine.OpenUDPPeer opens the server's connection on the binding's socket,
// and the server's replies arrive on it.
package main

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib-examples/tls/dtls/session"
	fibtls "github.com/lesismal/fib/tls"
)

// fatal reports an error and exits; a test replaces it.
var fatal = example.Fatal

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fatal(err)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	flags.SetOutput(out)
	server := flags.String("addr", "127.0.0.1:4444", "server address")
	fingerprint := flags.String("fingerprint", "", "the server's sha-256 fingerprint; any certificate is accepted, and reported, when empty")
	n := flags.Int("n", 5, "messages to send")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ua, err := net.ResolveUDPAddr("udp4", *server)
	if err != nil {
		return err
	}
	to := netip.AddrPortFrom(ua.AddrPort().Addr().Unmap(), ua.AddrPort().Port())

	cert, err := fibtls.GenerateWebRTCCertificate()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "fingerprint sha-256 %s\n", fibtls.CertificateFingerprint(cert.Certificate[0]))
	config := &stdtls.Config{
		Certificates: []stdtls.Certificate{cert},
		// No CA vouches for a self-signed certificate: the check is the
		// fingerprint, as in WebRTC.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			got := fibtls.CertificateFingerprint(raw[0])
			if *fingerprint != "" && !strings.EqualFold(got, *fingerprint) {
				return fmt.Errorf("server certificate %s, want %s", got, *fingerprint)
			}
			return nil
		},
	}

	events := make(chan string, *n+2)
	failed := make(chan error, 1)
	// conn is set before the handshake starts, and output is first
	// called by Start.
	var conn *fib.Connection
	output := func(b []byte) { _ = conn.Send(b) }
	s, err := session.New(true, config, output, fibtls.DTLSOptions{SRTPProfiles: session.SRTPProfiles}, session.Callbacks{
		OnHandshake: func(s *session.Session) { events <- s.Describe() },
		OnData:      func(_ *session.Session, b []byte) { events <- string(b) },
		OnClose: func(_ *session.Session, err error) {
			select {
			case failed <- err:
			default:
			}
		},
	})
	if err != nil {
		return err
	}

	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: "0.0.0.0:0", Handler: fib.HandlerFuncs{
		Data: func(_ *fib.Connection, data []byte) { s.HandleDatagram(data) },
	}})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	if conn, err = engine.OpenUDPPeer(0, to); err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		return err
	}

	timeout := time.After(15 * time.Second)
	for i := 0; i <= *n; i++ {
		select {
		case e := <-events:
			if i == 0 {
				fmt.Fprintf(out, "%s: %s\n", to, e)
			} else {
				fmt.Fprintf(out, "echo: %s\n", e)
			}
			if i < *n {
				if err := s.Write([]byte(example.Message(i + 1))); err != nil {
					return err
				}
			}
		case err := <-failed:
			return fmt.Errorf("DTLS: %w", err)
		case <-timeout:
			return errors.New("timed out")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
