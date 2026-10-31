//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command dialfrom dials from a chosen local address and port range, as a
// relay behind a firewall that admits one source address and a few ports
// must.
//
//	go run ./tcp/dialfrom
//	go run ./tcp/dialfrom -network udp -ports 20000-20003
//
// Engine.DialWith takes DialOptions: what DialWithHandler takes, and the
// local end. LocalAddr is the address (and port, zero leaving it to the
// kernel or the range) the socket is bound to before it connects, which on
// a multi-homed host picks the interface; PortMin and PortMax are a range
// searched from a random port until a bind succeeds. The socket is bound
// without SO_REUSEADDR, as net.Dialer binds it, so a port in use is skipped,
// and a range with none free fails the dial with an error that matches
// fib.ErrNoLocalPort.
//
// A server echoes each client the address it sees it from; the client dials
// it a few times from the range, and then once from a range of one port that
// a socket of its own holds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
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
	flags := flag.NewFlagSet("dialfrom", flag.ContinueOnError)
	flags.SetOutput(out)
	network := flags.String("network", "tcp", "tcp or udp")
	local := flags.String("local", "127.0.0.1", "the local address to dial from")
	ports := flags.String("ports", "20000-20019", "the local port range to dial from")
	dials := flags.Int("dials", 3, "connections to dial from the range")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *network != "tcp" && *network != "udp" {
		return fmt.Errorf("-network %q: want tcp or udp", *network)
	}
	ip, err := netip.ParseAddr(*local)
	if err != nil {
		return err
	}
	lo, hi, err := portRange(*ports)
	if err != nil {
		return err
	}

	// The server tells each client where it sees it from. A TCP connection
	// is closed by the server, so that its TIME_WAIT is the server's and the
	// client's port is free again at once.
	server, err := fib.New(example.EngineConfig(), fib.Binding{Network: *network, Addr: "127.0.0.1:0", Handler: fib.HandlerFuncs{
		Data: func(c *fib.Connection, data []byte) {
			_ = c.Send([]byte(c.RemoteAddr().String()))
			if *network == "tcp" {
				c.CloseAfterSend()
			}
		},
	}})
	if err != nil {
		return err
	}
	stopServer := example.Start(server)
	defer stopServer()
	addr := example.Addr(server)

	client, stopClient := example.ClientEngine()
	defer stopClient()
	replies := make(chan string, 1)
	handler := fib.HandlerFuncs{Data: func(_ *fib.Connection, data []byte) { replies <- string(data) }}
	dial := func(opts fib.DialOptions) (*fib.Connection, error) {
		opts.Network, opts.Addr, opts.Timeout, opts.Handler = *network, addr, 5*time.Second, handler
		type result struct {
			c   *fib.Connection
			err error
		}
		done := make(chan result, 1)
		if err := client.DialWith(opts, func(c *fib.Connection, err error) { done <- result{c, err} }); err != nil {
			return nil, err
		}
		select {
		case r := <-done:
			return r.c, r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	fmt.Fprintf(out, "dialing %s %s from %s, ports %d-%d\n", *network, addr, ip, lo, hi)
	for i := 1; i <= *dials; i++ {
		c, err := dial(fib.DialOptions{LocalAddr: netip.AddrPortFrom(ip, 0), PortMin: lo, PortMax: hi})
		if err != nil {
			return err
		}
		if err := c.Send([]byte("where am I from?")); err != nil {
			return err
		}
		var seen string
		select {
		case seen = <-replies:
		case <-time.After(5 * time.Second):
			return errors.New("no reply from the server")
		}
		_ = c.Close()
		port, _ := strconv.Atoi(seen[strings.LastIndexByte(seen, ':')+1:])
		fmt.Fprintf(out, "dial %d: the server sees %s, in the range: %v\n", i, seen, port >= int(lo) && port <= int(hi))
	}

	// A range of one port, which a socket of the client's own holds.
	held, port, err := hold(*network, ip)
	if err != nil {
		return err
	}
	defer held.Close()
	_, err = dial(fib.DialOptions{LocalAddr: netip.AddrPortFrom(ip, 0), PortMin: port, PortMax: port})
	fmt.Fprintf(out, "dial from port %d, already in use: %v (ErrNoLocalPort: %v)\n", port, err, errors.Is(err, fib.ErrNoLocalPort))
	return nil
}

// portRange parses "lo-hi".
func portRange(s string) (lo, hi uint16, err error) {
	a, b, ok := strings.Cut(s, "-")
	l, err1 := strconv.ParseUint(a, 10, 16)
	h, err2 := strconv.ParseUint(b, 10, 16)
	if !ok || err1 != nil || err2 != nil || l == 0 || l > h {
		return 0, 0, fmt.Errorf("-ports %q: want lo-hi, 0 < lo <= hi", s)
	}
	return uint16(l), uint16(h), nil
}

// hold binds a socket of network to a port of ip the kernel picks, and
// returns it with the port.
func hold(network string, ip netip.Addr) (io.Closer, uint16, error) {
	at := netip.AddrPortFrom(ip, 0).String()
	if network == "udp" {
		c, err := net.ListenPacket("udp", at)
		if err != nil {
			return nil, 0, err
		}
		return c, uint16(c.LocalAddr().(*net.UDPAddr).Port), nil
	}
	l, err := net.Listen("tcp", at)
	if err != nil {
		return nil, 0, err
	}
	return l, uint16(l.Addr().(*net.TCPAddr).Port), nil
}
