//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command altsvc follows Alt-Svc from HTTPS over TCP to HTTP/3, and falls
// back to TCP where UDP does not get through, with http3.AltSvcClient.
//
//	go run ./http3/altsvc
//
// http3.Client speaks HTTP/3 and http.Client HTTP/1.1 and HTTP/2; an
// AltSvcClient carries an origin from one to the other as a browser does.
// The first request to an origin goes over TCP. A response with
// "Alt-Svc: h3=":port"" (RFC 7838) sends the requests after it over HTTP/3,
// to the port named, while the certificate is still checked for the
// origin's own name.
//
// Two origins: one serves HTTP/3 on the UDP port of the same number, and
// one advertises HTTP/3 on a port where nothing answers, as on a network
// that blocks UDP. A request to it that is safe to repeat goes again over
// TCP at once, and the origin stays on TCP for 30 seconds, so the
// network costs one handshake timeout rather than one per request.
package main

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
	"github.com/lesismal/fib/http3"
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
	flags := flag.NewFlagSet("altsvc", flag.ContinueOnError)
	flags.SetOutput(out)
	n := flags.Int("n", 3, "requests to each origin")
	handshake := flags.Duration("handshake-timeout", time.Second, "how long an HTTP/3 handshake may take before the client falls back")
	if err := flags.Parse(args); err != nil {
		return err
	}

	tlsConfig, err := certs.ServerFlagsWithOut(flag.NewFlagSet("", flag.ContinueOnError), "").Config()
	if err != nil {
		return err
	}
	// The origin that speaks HTTP/3: TLS over TCP and QUIC over UDP on one
	// port number, an engine binding each.
	port, err := freePort()
	if err != nil {
		return err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	handler := echo(http3.AltSvc(port))
	h3Origin, err := fib.New(example.EngineConfig(),
		fib.Binding{Network: "tcp", Addr: addr, Handler: fibtls.NewServer(fibhttp.ConfigureTLS(tlsConfig), fibhttp.NewHandler(handler))},
		fib.Binding{Network: "udp", Addr: addr, Handler: http3.NewHandler(tlsConfig, handler)},
	)
	if err != nil {
		return err
	}
	stopH3 := example.Start(h3Origin)
	defer stopH3()

	// The origin behind a network that drops UDP: it advertises HTTP/3 on
	// its port, and nothing there answers.
	blockedPort, err := freePort()
	if err != nil {
		return err
	}
	tcpAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(blockedPort))
	tcpOnly, err := fib.New(example.EngineConfig(), fib.Binding{Addr: tcpAddr,
		Handler: fibtls.NewServer(fibhttp.ConfigureTLS(tlsConfig), fibhttp.NewHandler(echo(http3.AltSvc(blockedPort))))})
	if err != nil {
		return err
	}
	stopTCP := example.Start(tcpOnly)
	defer stopTCP()

	engine, stopClient := example.ClientEngine()
	defer stopClient()
	roots := x509.NewCertPool()
	cert, _ := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	roots.AddCert(cert)
	tcpConfig := fibhttp.DefaultClientConfig()
	tcpConfig.Timeout = 10 * time.Second
	tcpConfig.TLSConfig = &stdtls.Config{RootCAs: roots}
	h3Config := http3.DefaultClientConfig()
	h3Config.Timeout = 10 * time.Second
	h3Config.HandshakeTimeout = *handshake
	// No ServerName: each origin's own name is what its certificate is
	// checked for, whichever address Alt-Svc sends the request to.
	h3Config.TLSConfig = &stdtls.Config{RootCAs: roots}
	client := http3.NewAltSvcClient(fibhttp.NewClient(engine, tcpConfig), http3.NewClient(engine, h3Config))
	defer client.Close()

	for _, origin := range []struct{ name, addr string }{{"h3 origin", addr}, {"UDP blocked", tcpAddr}} {
		for i := 1; i <= *n; i++ {
			req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, "https://"+origin.addr+"/", nil)
			if err != nil {
				return err
			}
			start := time.Now()
			resp, err := client.Go(req).Wait()
			if err != nil {
				return err
			}
			body, _ := io.ReadAll(resp.Body)
			fmt.Fprintf(out, "%s, request %d: %s over %s in %v, Alt-Svc %q\n", origin.name, i, body, resp.Proto,
				time.Since(start).Round(10*time.Millisecond), resp.Header.Get("Alt-Svc"))
		}
	}
	return nil
}

// echo answers with the protocol the request came over, and advertises
// HTTP/3 on responses over TCP.
func echo(altSvc string) fibhttp.HandlerFunc {
	return func(c *fibhttp.Context) {
		header := stdhttp.Header{"Content-Type": {"text/plain"}}
		if c.Request.ProtoMajor < 3 {
			header.Set("Alt-Svc", altSvc)
		}
		_ = c.WriteResponse(fibhttp.Response{StatusCode: stdhttp.StatusOK, Header: header, Body: []byte("answered")})
	}
}

// freePort finds a port free on loopback for both TCP and UDP.
func freePort() (int, error) {
	for range 20 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pc, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		ln.Close()
		if err == nil {
			pc.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no port free for both TCP and UDP")
}
