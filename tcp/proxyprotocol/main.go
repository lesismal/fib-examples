//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command proxyprotocol serves HTTP behind a proxy that sends a PROXY
// protocol header, and reports the client's address, not the proxy's, with
// Connection.SetRemoteAddrPort.
//
//	go run ./tcp/proxyprotocol
//
// A TCP proxy, HAProxy or a cloud load balancer, connects to the server from
// its own address, so every request seems to come from it. With the PROXY
// protocol (version 1 here, a text line) it first sends the address of the
// client it is relaying:
//
//	PROXY TCP4 192.0.2.10 198.51.100.1 51234 8080\r\n
//
// The server reads that line in a handler of its own, makes the address the
// connection's with SetRemoteAddrPort, and then hands the connection to the
// HTTP handler with SetHandler, as package mux hands one on: from then on
// RemoteAddr, and with it every request's RemoteAddr, is the client's. A
// connection without the header is closed, since behind such a proxy it did
// not come through it.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
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
	flags := flag.NewFlagSet("proxyprotocol", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address")
	if err := flags.Parse(args); err != nil {
		return err
	}

	r := fibhttp.NewRouter()
	r.Get("/", func(c *fibhttp.Context) {
		_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "you are %s", c.Request.RemoteAddr))
	})
	server, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: &proxyHeader{next: fibhttp.NewHandler(r)}})
	if err != nil {
		return err
	}
	stopServer := example.Start(server)
	defer stopServer()

	proxy, err := startProxy(example.Addr(server), out)
	if err != nil {
		return err
	}
	defer proxy.Close()

	// Through the proxy, the server sees the client.
	client := &stdhttp.Client{Timeout: 10 * time.Second, Transport: &stdhttp.Transport{DisableKeepAlives: true}}
	req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, "http://"+proxy.Addr().String()+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Fprintf(out, "server, through the proxy: %s\n", body)

	// Straight to the server, without a header, the connection is refused.
	req, _ = stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, "http://"+example.Addr(server)+"/", nil)
	if _, err := client.Do(req); err != nil {
		fmt.Fprintln(out, "server, straight: closed, no PROXY header")
	} else {
		return errors.New("a connection without a PROXY header was served")
	}
	return nil
}

// proxyHeader reads a connection's PROXY protocol v1 line, and hands the
// connection, with its client's address, to next.
type proxyHeader struct{ next fib.Handler }

func (h *proxyHeader) OnOpen(*fib.Connection) {}

func (h *proxyHeader) OnData(c *fib.Connection, data []byte) {
	// The line may arrive in pieces: keep what came so far.
	var buf []byte
	if held, ok := c.Attachment().([]byte); ok {
		buf = append(held, data...)
	} else {
		buf = data
	}
	end := bytes.Index(buf, []byte("\r\n"))
	if end < 0 {
		if len(buf) >= 107 { // the longest v1 line
			c.Close()
			return
		}
		c.SetAttachment(bytes.Clone(buf))
		return
	}
	client, err := parseProxyV1(string(buf[:end]))
	if err != nil {
		c.Close()
		return
	}
	// Before anything reads the address; the next handler's state goes in
	// the attachment, so give it back empty.
	c.SetRemoteAddrPort(client)
	c.SetAttachment(nil)
	c.SetHandler(h.next)
	h.next.OnOpen(c)
	if rest := buf[end+2:]; len(rest) > 0 {
		h.next.OnData(c, rest)
	}
}

func (h *proxyHeader) OnPriorityData(c *fib.Connection, data []byte) { h.OnData(c, data) }
func (h *proxyHeader) OnClose(*fib.Connection, error)                {}

// parseProxyV1 returns the source address of a PROXY protocol v1 line,
// "PROXY TCP4|TCP6 src dst sport dport".
func parseProxyV1(line string) (netip.AddrPort, error) {
	f := strings.Fields(line)
	if len(f) != 6 || f[0] != "PROXY" || (f[1] != "TCP4" && f[1] != "TCP6") {
		return netip.AddrPort{}, fmt.Errorf("not a PROXY line: %q", line)
	}
	ip, err := netip.ParseAddr(f[2])
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.ParseUint(f[4], 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(ip, uint16(port)), nil
}

// startProxy is the load balancer: it relays each connection to upstream,
// first sending the client's address.
func startProxy(upstream string, out io.Writer) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer in.Close()
				up, err := net.DialTimeout("tcp", upstream, 5*time.Second)
				if err != nil {
					return
				}
				defer up.Close()
				src, dst := in.RemoteAddr().(*net.TCPAddr), up.RemoteAddr().(*net.TCPAddr)
				fmt.Fprintf(out, "proxy: client %s, upstream connection from %s\n", src, up.LocalAddr())
				fmt.Fprintf(up, "PROXY TCP4 %s %s %d %d\r\n", src.IP, dst.IP, src.Port, dst.Port)
				go func() { _, _ = io.Copy(up, in) }()
				_, _ = io.Copy(in, up)
			}()
		}
	}()
	return ln, nil
}
