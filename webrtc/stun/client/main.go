//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command client asks a STUN server for this host's server-reflexive
// address: the address and port its UDP socket has as seen from the
// server, after any NAT on the way.
//
//	go run ./webrtc/stun/client                          # stun.l.google.com:19302
//	go run ./webrtc/stun/client -server 127.0.0.1:3479   # the server next to it
//
// The socket is a UDP binding of a fib engine. An engine opens a UDP
// connection when a peer first sends to it; here this side speaks first, so
// Engine.OpenUDPPeer opens the server's connection on the binding's socket,
// and the response arrives on it. The request is retransmitted on RFC
// 8489's schedule by a webrtc/stun Transaction, which does no I/O: this
// program sends what it returns, when it says to.
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
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/webrtc/stun"
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
	server := flags.String("server", "stun.l.google.com:19302", "STUN server, host:port")
	local := flags.String("addr", "0.0.0.0:0", "local UDP address to ask about")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// IPv4: the default server name resolves to both, and a socket bound
	// to 0.0.0.0 speaks only IPv4.
	ua, err := net.ResolveUDPAddr("udp4", *server)
	if err != nil {
		return err
	}
	to := netip.AddrPortFrom(ua.AddrPort().Addr().Unmap(), ua.AddrPort().Port())

	// Datagrams from the server, copied: the engine reuses its buffer.
	responses := make(chan []byte, 8)
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *local, Handler: fib.HandlerFuncs{
		Data: func(_ *fib.Connection, data []byte) {
			select {
			case responses <- append([]byte(nil), data...):
			default:
			}
		},
	}})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()

	conn, err := engine.OpenUDPPeer(0, to)
	if err != nil {
		return err
	}
	mapped, err := binding(ctx, conn, responses)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "local %s, server-reflexive %s (as %s sees it)\n", conn.LocalAddr(), mapped, to)
	return nil
}

// binding sends a Binding request on conn, retransmitting it until a
// response with its transaction ID arrives, and returns the mapped address
// the response carries.
func binding(ctx context.Context, conn *fib.Connection, responses <-chan []byte) (netip.AddrPort, error) {
	var req stun.Message
	req.Build(stun.BindingRequest, stun.NewTransactionID())
	req.AddFingerprint()

	var tx stun.Transaction
	tx.Start(req.Raw, time.Now(), stun.TransactionConfig{})
	if err := conn.Send(tx.Request); err != nil {
		return netip.AddrPort{}, err
	}
	for {
		next, _ := tx.NextTimeout()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return netip.AddrPort{}, ctx.Err()
		case now := <-timer.C:
			again, err := tx.HandleTimeout(now)
			if err != nil {
				return netip.AddrPort{}, err // stun.ErrTimeout: 39.5s and 7 sends
			}
			if again != nil {
				_ = conn.Send(again)
			}
		case b := <-responses:
			timer.Stop()
			var resp stun.Message
			if resp.Decode(b) != nil || resp.TransactionID != tx.ID {
				continue // not ours: a late answer to nothing, or noise
			}
			tx.Finish()
			if resp.Type != stun.BindingSuccess {
				code, reason, _ := resp.ErrorCode()
				return netip.AddrPort{}, fmt.Errorf("binding refused: %d %s", code, reason)
			}
			if a, err := resp.XORAddress(stun.AttrXORMappedAddress); err == nil {
				return a, nil
			}
			if a, err := resp.Address(stun.AttrMappedAddress); err == nil {
				return a, nil // a server of RFC 3489's day
			}
			return netip.AddrPort{}, errors.New("response carries no mapped address")
		}
	}
}
