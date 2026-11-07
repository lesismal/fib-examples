//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command server is a KCP message echo server with no goroutine per
// session: Config.OnMessage is given each message whole on a fib worker as
// it completes, however many segments it took, and answers it with
// Session.Send, which queues one message and never blocks.
//
//	go run ./kcp/messages/server
//
// ServerConfig.OnSession takes each new session in place of Accept, so the
// server never needs a net.Listener loop; a session ends with the client's
// silence (Config.IdleTimeout, 30 seconds here) since KCP has no close.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib-examples/kcp/settings"
	"github.com/lesismal/fib/kcp"
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
	addr := flags.String("addr", "127.0.0.1:9003", "listen address")
	idle := flags.Duration("idle", 30*time.Second, "close a session after this long without a datagram")
	kcpFlags := settings.FlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := kcpFlags.Config()
	if err != nil {
		return err
	}
	config.IdleTimeout = *idle
	config.OnMessage = func(s *kcp.Session, msg []byte) {
		fmt.Fprintf(out, "session %s: %d-byte message\n", s.RemoteAddr(), len(msg))
		if err := s.Send(msg); err != nil {
			fmt.Fprintf(out, "session %s: echo dropped: %v\n", s.RemoteAddr(), err)
		}
	}
	config.OnClose = func(s *kcp.Session, err error) {
		fmt.Fprintf(out, "session %s: closed: %v\n", s.RemoteAddr(), err)
	}
	srv, err := kcp.NewServer(kcp.ServerConfig{
		Config: config,
		OnSession: func(s *kcp.Session) {
			fmt.Fprintf(out, "session %s conv %08x: open\n", s.RemoteAddr(), s.Conv())
		},
	})
	if err != nil {
		return err
	}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *addr, Handler: srv})
	if err != nil {
		return err
	}
	defer srv.Close()
	return example.Serve(ctx, out, ready, "KCP message echo server listening on %s ("+kcpFlags.Describe()+")", engine)
}
