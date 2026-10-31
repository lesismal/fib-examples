//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command transport configures the QUIC connections under an HTTP/3 server
// and client, and watches two of the settings at work in the datagrams the
// server receives: Retry, which makes a client prove its address, and path
// MTU discovery, which grows the datagrams past QUIC's 1200 bytes.
//
//	go run ./http3/transport
//	go run ./http3/transport -retry=false -no-pmtud
//
// Config.RetryHandshakes is how many handshakes the server may have under
// way before it answers a new client's first Initial with a Retry, whose
// token the client must send back: a round trip for the client, and no TLS
// work or state for an Initial from a forged address. Below zero, as with
// -retry, every client is retried. A client whose handshake completes is
// given a NEW_TOKEN token, and its next connection from the same address
// skips the Retry (TransportConfig.DisableNewTokens turns that off).
// MaxHandshakes, past which Initials are dropped, bounds the rest.
//
// Path MTU discovery (RFC 8899) is on by default, at both ends: once the
// handshake is confirmed a connection probes for the largest datagram its
// path carries, up to MaxProbeDatagramSize (1472 bytes over IPv4, what an
// Ethernet MTU leaves), on a socket set not to fragment
// (fib.Connection.SetDontFragment).
//
// Config.Transport and ClientConfig.Transport, a TransportConfig, set the
// rest: keep-alive PINGs, receive windows, how long an acknowledgement may
// wait, flood budgets, key updates by time, and connection IDs.
package main

import (
	"bytes"
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
	"github.com/lesismal/fib/http3"
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
	flags := flag.NewFlagSet("transport", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's UDP address")
	retry := flags.Bool("retry", true, "send every new client a Retry")
	noPMTUD := flags.Bool("no-pmtud", false, "turn path MTU discovery off at both ends")
	size := flags.Int("size", 1<<20, "bytes the client uploads")
	if err := flags.Parse(args); err != nil {
		return err
	}

	tlsConfig, err := certs.ServerFlagsWithOut(flag.NewFlagSet("", flag.ContinueOnError), "").Config()
	if err != nil {
		return err
	}
	transport := http3.TransportConfig{
		KeepAlivePeriod:     10 * time.Second,
		StreamReceiveWindow: 4 << 20,
		ConnReceiveWindow:   32 << 20,
		MaxAckDelay:         10 * time.Millisecond,
		ActiveConnectionIDs: 8,
		KeyUpdateInterval:   time.Hour,
	}
	config := http3.DefaultConfig()
	config.TLSConfig = tlsConfig
	config.MaxBodyBytes = int64(*size)
	config.MaxIdleTimeout = 30 * time.Second
	config.Transport = transport
	config.DisablePathMTUDiscovery = *noPMTUD
	if *retry {
		config.RetryHandshakes = -1
	}
	r := fibhttp.NewRouter()
	r.Post("/upload", func(c *fibhttp.Context) {
		n, _ := io.Copy(io.Discard, c.Request.Body)
		_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "%d bytes", n))
	})
	sniff := &sniffer{ServerHandler: http3.NewHandlerWithConfig(config, r)}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *addr, Handler: sniff})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	url := "https://" + example.Addr(engine) + "/upload"

	clientEngine, stopClient := example.ClientEngine()
	defer stopClient()
	roots := x509.NewCertPool()
	cert, _ := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	roots.AddCert(cert)
	clientConfig := http3.DefaultClientConfig()
	clientConfig.Timeout = 30 * time.Second
	clientConfig.TLSConfig = &stdtls.Config{RootCAs: roots}
	clientConfig.Transport = transport
	clientConfig.DisablePathMTUDiscovery = *noPMTUD
	client := http3.NewClient(clientEngine, clientConfig)
	defer client.Close()

	for i := 1; i <= 2; i++ {
		req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, url, bytes.NewReader(make([]byte, *size)))
		resp, err := client.Go(req).Wait()
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(out, "client: POST /upload %d: %s %s, %s\n", i, resp.Proto, resp.Status, body)
		// The probes go once the handshake is confirmed; give them a
		// moment to be acknowledged before the next request.
		time.Sleep(100 * time.Millisecond)
	}
	s := sniff.snapshot()
	fmt.Fprintf(out, "server: client Initials without a token: %d, with one: %d\n", s.plain, s.withToken)
	fmt.Fprintf(out, "server: largest datagram from the client: %d bytes\n", s.largest)
	return nil
}

// sniffer is the HTTP/3 handler with a look at every datagram first. The
// embedded handler's other methods, OnOpen, OnClose, OnEngineClose and the
// rest, are its own.
type sniffer struct {
	*http3.ServerHandler
	mu    sync.Mutex
	stats sniffStats
}

type sniffStats struct {
	plain, withToken, largest int
}

func (s *sniffer) OnData(c *fib.Connection, data []byte) {
	s.look(data)
	s.ServerHandler.OnData(c, data)
}

func (s *sniffer) OnDatagrams(c *fib.Connection, datagrams [][]byte) {
	for _, d := range datagrams {
		s.look(d)
	}
	s.ServerHandler.OnDatagrams(c, datagrams)
}

func (s *sniffer) snapshot() sniffStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// look notes the datagram's size, and whether it opens with an Initial
// packet and that packet carries a token (RFC 9000 section 17.2.2).
func (s *sniffer) look(d []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.largest = max(s.stats.largest, len(d))
	token, ok := initialToken(d)
	switch {
	case !ok:
	case token == 0:
		s.stats.plain++
	default:
		s.stats.withToken++
	}
}

// initialToken reports the token length of the QUIC version 1 Initial
// packet d begins with, and false when it begins with none.
func initialToken(d []byte) (int, bool) {
	// Long header, Initial: 1 1 00 xxxx, then version 0x00000001.
	if len(d) < 7 || d[0]&0xf0 != 0xc0 || d[1] != 0 || d[2] != 0 || d[3] != 0 || d[4] != 1 {
		return 0, false
	}
	p := d[5:]
	for range 2 { // destination and source connection IDs
		if len(p) < 1 || len(p) < 1+int(p[0]) {
			return 0, false
		}
		p = p[1+int(p[0]):]
	}
	if len(p) < 1 {
		return 0, false
	}
	// A variable-length integer: its first two bits give its length.
	n := 1 << (p[0] >> 6)
	if len(p) < n {
		return 0, false
	}
	v := int(p[0] & 0x3f)
	for _, b := range p[1:n] {
		v = v<<8 | int(b)
	}
	return v, true
}
