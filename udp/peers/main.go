//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command peers gives each peer of a UDP listener an idle timeout of its
// own with Connection.SetUDPIdleTimeout, and sets the listener's socket not
// to fragment with SetDontFragment.
//
//	go run ./udp/peers
//
// UDP has no close, so a UDP binding closes a peer's connection once the
// peer has been silent for Config.UDPIdleTimeout (OnClose gets
// ErrUDPIdleTimeout). One timeout rarely suits every peer: a probe that
// sends once is done at once, and a peer that keeps state of its own,
// a QUIC connection, say, times itself out. The server here reads what each
// peer says first and sets its timeout: "brief" gets a short one, "default"
// the engine's, and "keep" none at all.
//
// SetDontFragment has the socket's datagrams sent whole or not at all: a
// datagram too large for the path is dropped rather than delivered in
// pieces, as path MTU discovery, QUIC's, needs to learn what the path
// carries. A peer of a listener sends through the listener's socket, so it
// holds for every peer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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

// opened is a peer's connection's start, kept as its attachment.
type opened struct {
	at   time.Time
	kind string
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("peers", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's UDP address")
	idle := flags.Duration("idle", time.Second, "Config.UDPIdleTimeout, every peer's unless it sets its own")
	brief := flags.Duration("brief", 200*time.Millisecond, `the timeout of a peer that says "brief"`)
	if err := flags.Parse(args); err != nil {
		return err
	}

	events := make(chan string, 16)
	config := example.EngineConfig()
	config.UDPIdleTimeout = *idle
	engine, err := fib.New(config, fib.Binding{Network: "udp", Addr: *addr, Handler: fib.HandlerFuncs{
		Data: func(c *fib.Connection, data []byte) {
			if c.Attachment() == nil {
				// A peer's first datagram: set its timeout by what it says.
				kind := string(data)
				switch kind {
				case "brief":
					c.SetUDPIdleTimeout(*brief)
				case "keep":
					c.SetUDPIdleTimeout(-1) // closed only by Close
				}
				c.SetAttachment(&opened{at: time.Now(), kind: kind})
				err := c.SetDontFragment()
				events <- fmt.Sprintf("server: %q peer opened, SetDontFragment: %v", kind, err)
			}
			_ = c.Send(data)
		},
		Close: func(c *fib.Connection, err error) {
			if o, ok := c.Attachment().(*opened); ok {
				events <- fmt.Sprintf("server: %q peer closed after about %v: %v", o.kind, time.Since(o.at).Round(*idle/4), err)
			}
		},
	}})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	server := example.Addr(engine)

	// Three peers, each on a socket of its own, say one thing and go quiet.
	peers := map[string]*net.UDPConn{}
	for _, kind := range []string{"brief", "default", "keep"} {
		raddr, _ := net.ResolveUDPAddr("udp", server)
		conn, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			return err
		}
		defer conn.Close()
		peers[kind] = conn
		if err := say(conn, kind); err != nil {
			return err
		}
		fmt.Fprintln(out, <-events)
	}
	// The engine looks at its peers every quarter of its own timeout, so a
	// peer goes up to that late.
	for range 2 {
		select {
		case e := <-events:
			fmt.Fprintln(out, e)
		case <-time.After(*idle * 3):
			return errors.New("a peer was never timed out")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// The peer with no timeout is still the connection it was: no new
	// OnOpen, no new first datagram.
	if err := say(peers["keep"], "still there?"); err != nil {
		return err
	}
	fmt.Fprintf(out, "client: %q peer still answered, on the connection it opened, after the others timed out\n", "keep")
	select {
	case e := <-events:
		return fmt.Errorf("unexpected: %s", e)
	default:
	}
	return nil
}

// say sends text and waits for its echo.
func say(conn *net.UDPConn, text string) error {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(text)); err != nil {
		return err
	}
	b := make([]byte, 64)
	n, err := conn.Read(b)
	if err == nil && string(b[:n]) != text {
		err = fmt.Errorf("echo %q of %q", b[:n], text)
	}
	return err
}
