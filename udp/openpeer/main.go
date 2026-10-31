//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command openpeer shows Engine.OpenUDPPeer: a UDP binding that speaks first.
//
//	go run ./udp/openpeer                        # to a loopback echo it starts itself
//	go run ./udp/nontls/server                   # or to the UDP echo server,
//	go run ./udp/openpeer -to 127.0.0.1:9001     # from another terminal
//
// A UDP binding opens a connection for each address that sends it a
// datagram, and its handler answers on it. A protocol that speaks first,
// from the port it listens on, needs that connection before the peer has
// sent anything: ICE checks a peer's addresses from its one socket, and asks
// a STUN or TURN server what that socket looks like from outside.
// OpenUDPPeer opens it on the binding's socket, with the binding's handler,
// so the reply arrives at the port the request left from, on the same
// connection, exactly as if the peer had spoken first.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
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
	flags := flag.NewFlagSet("openpeer", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the binding's address, which every datagram leaves from")
	to := flags.String("to", "", "the peer to speak to first; a loopback echo started here when empty")
	n := flags.Int("n", 3, "datagrams to send")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		echo, err := startEcho()
		if err != nil {
			return err
		}
		defer echo.Close()
		*to = echo.LocalAddr().String()
	}
	peer, err := netip.ParseAddrPort(*to)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}

	replies := make(chan string, *n)
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *addr, Handler: fib.HandlerFuncs{
		// OnOpen runs for the connection OpenUDPPeer opens, as it would
		// for a peer's first datagram.
		Open: func(c *fib.Connection) { fmt.Fprintf(out, "opened %s -> %s\n", c.LocalAddr(), c.RemoteAddr()) },
		Data: func(c *fib.Connection, data []byte) {
			replies <- fmt.Sprintf("reply %q from %s, on %s", data, c.RemoteAddr(), c.LocalAddr())
		},
	}})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()

	// Binding 0 is the one above. OpenUDPPeer waits for the socket's event
	// loop, so it is called here, never from that loop's own callbacks.
	conn, err := engine.OpenUDPPeer(0, peer)
	if err != nil {
		return err
	}
	for i := 1; i <= *n; i++ {
		if err := conn.Send([]byte(example.Message(i))); err != nil {
			return err
		}
		select {
		case r := <-replies:
			fmt.Fprintln(out, r)
		case <-time.After(5 * time.Second):
			return fmt.Errorf("no reply from %s", peer)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// startEcho starts a plain UDP echo on loopback, standing in for a peer
// that has never heard of this program.
func startEcho() (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	go func() {
		b := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDPAddrPort(b[:n], from)
		}
	}()
	return conn, nil
}
