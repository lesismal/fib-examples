//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command server is a STUN server that answers Binding requests: each
// client is told the address its request came from, which is how a client
// behind a NAT learns the address the Internet sees it at, its
// server-reflexive address (RFC 8489).
//
//	go run ./webrtc/stun/server
//	go run ./webrtc/stun/client -server 127.0.0.1:3479
//
// It parses and builds messages with package webrtc/stun, in place: the
// request is decoded in the buffer it arrived in.
package main

import (
	"context"
	"flag"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/webrtc/stun"
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

// run serves until ctx ends. ready, if not nil, is told the address once the
// server is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:3479", "listen address (UDP)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *addr, Handler: fib.HandlerFuncs{
		Data: func(c *fib.Connection, data []byte) {
			if reply := answer(c.RemoteAddrPort(), data); reply != nil {
				_ = c.SendOwned(reply)
			}
		},
	}})
	if err != nil {
		return err
	}
	return example.Serve(ctx, out, ready, "STUN server listening on %s (UDP)", engine)
}

// answer returns the Binding success response to a Binding request from
// from, or nil for anything else, which a STUN server ignores.
func answer(from netip.AddrPort, data []byte) []byte {
	var req stun.Message
	if req.Decode(data) != nil || req.Type != stun.BindingRequest {
		return nil
	}
	if req.Contains(stun.AttrFingerprint) && req.CheckFingerprint() != nil {
		return nil
	}
	var resp stun.Message
	resp.Build(stun.BindingSuccess, req.TransactionID)
	// XOR-MAPPED-ADDRESS, which NATs that rewrite addresses in payloads
	// leave alone.
	resp.AddXORAddress(stun.AttrXORMappedAddress, netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
	resp.AddString(stun.AttrSoftware, "fib-examples")
	resp.AddFingerprint()
	return resp.Raw
}
