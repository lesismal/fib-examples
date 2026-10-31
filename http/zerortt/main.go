//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command zerortt sends HTTPS requests over HTTP/1.1 as TLS 1.3 early data
// (0-RTT), and picks the TLS 1.3 cipher suite the client offers.
//
//	go run ./http/zerortt
//	go run ./http/zerortt -suite chacha20
//
// A client that has a session ticket from a server can send a request in its
// first flight, before the handshake is over, which saves it a round trip.
// The server turns early data on with Config.EarlyData; the client with
// ClientConfig.EarlyData, a SessionCache (it keeps one of its own when given
// none), and a connection that cannot become HTTP/2, since the framing of
// what would go early is known only once ALPN is: DisableHTTP2 here.
//
// Early data can be replayed by anyone who recorded it, so the client sends
// only a request that is safe to repeat (GET, HEAD, OPTIONS, TRACE, with no
// body), and the server serves only those from it, marked "Early-Data: 1"
// (RFC 8470); anything else it answers 425 Too Early, and the client sends
// it again once the handshake is over. Config.EarlyDataUnsafe serves every
// method and leaves the replay check to the handler.
//
// TLS 1.3 has three suites, and fib's handshake offers them AES-GCM first
// where the CPU has AES instructions and ChaCha20-Poly1305 first elsewhere.
// ClientConfig.TLS13CipherSuites narrows or reorders them; crypto/tls has no
// field for it. A TLS 1.3 server takes one of its client's suites, so the
// choice is the client's.
package main

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
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

// suites are the TLS 1.3 suites by the names -suite takes.
var suites = map[string]uint16{
	"aes128":   stdtls.TLS_AES_128_GCM_SHA256,
	"aes256":   stdtls.TLS_AES_256_GCM_SHA384,
	"chacha20": stdtls.TLS_CHACHA20_POLY1305_SHA256,
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("zerortt", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address")
	suite := flags.String("suite", "", "the one TLS 1.3 suite the client offers: aes128, aes256 or chacha20; empty offers all three")
	n := flags.Int("n", 3, "GET requests, each on a new connection")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var offer []uint16
	if *suite != "" {
		id, ok := suites[*suite]
		if !ok {
			return fmt.Errorf("-suite %q: want aes128, aes256 or chacha20", *suite)
		}
		offer = []uint16{id}
	}

	// A self-signed certificate, kept in memory for the client to trust.
	tlsConfig, err := certs.ServerFlagsWithOut(flag.NewFlagSet("", flag.ContinueOnError), "").Config()
	if err != nil {
		return err
	}
	config := fibhttp.DefaultConfig()
	// Enough: it turns early data on for the TLS handler it is wrapped in.
	config.EarlyData = true
	handler := fibtls.NewServer(fibhttp.ConfigureTLS(tlsConfig), fibhttp.NewHandlerWithConfig(config, fibhttp.HandlerFunc(echo)))
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: handler})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	url := "https://" + example.Addr(engine) + "/"
	fmt.Fprintf(out, "server: %s, early data on\n", url)

	clientEngine, stopClient := example.ClientEngine()
	defer stopClient()
	roots := x509.NewCertPool()
	cert, _ := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	roots.AddCert(cert)
	clientConfig := fibhttp.DefaultClientConfig()
	clientConfig.Timeout = 10 * time.Second
	clientConfig.TLSConfig = &stdtls.Config{RootCAs: roots}
	clientConfig.EarlyData = true
	clientConfig.DisableHTTP2 = true
	clientConfig.TLS13CipherSuites = offer
	// Keep a connection only briefly, long enough for the session ticket
	// that follows a response, so that each request makes a new one and
	// resumes the session the one before it was given. A ticket is good
	// for one resumption.
	clientConfig.MaxIdleConnsPerHost = 1
	clientConfig.IdleConnTimeout = 50 * time.Millisecond
	client := fibhttp.NewClient(clientEngine, clientConfig)
	defer client.Close()

	send := func(method string, body io.Reader) error {
		req, err := stdhttp.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return err
		}
		resp, err := client.Go(req).Wait()
		if err != nil {
			return err
		}
		text, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(out, "client: %s: %s %s\n", method, resp.Status, strings.TrimSpace(string(text)))
		return nil
	}
	for range *n {
		if err := send(stdhttp.MethodGet, nil); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond) // the connection idles out
	}
	// Not safe to repeat: the client waits for the handshake to send it.
	return send(stdhttp.MethodPost, strings.NewReader("an order"))
}

// echo says how the request arrived.
func echo(c *fibhttp.Context) {
	state := "no TLS"
	if t := c.Request.TLS; t != nil {
		state = fmt.Sprintf("%s, resumed %v", stdtls.CipherSuiteName(t.CipherSuite), t.DidResume)
	}
	early := c.Request.Header.Get("Early-Data")
	if early == "" {
		early = "no"
	}
	_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "%s %s, early data: %s, %s", c.Request.Proto, c.Request.Method, early, state))
}
