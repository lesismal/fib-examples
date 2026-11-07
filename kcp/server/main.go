//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command server is a KCP echo server: each session is a net.Conn that a
// goroutine copies back to itself, as one would serve a kcp-go
// UDPSession, while fib reads the UDP socket and drives every session's
// KCP.
//
//	go run ./kcp/server
//	go run ./kcp/server -mode fast3 -crypt none -ds 0
//
// The flags are kcptun's for the KCP layer, so its defaults (aes with the
// passphrase "it's a secrect", FEC 10/3, mode fast) also take a kcp-go
// client set up the same way. kcp.Server is a fib.Handler for the UDP
// binding and a net.Listener of the sessions it starts, so anything that
// serves a net.Listener, http.Serve included, serves KCP.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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
	addr := flags.String("addr", "127.0.0.1:9002", "listen address")
	kcpFlags := settings.FlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := kcpFlags.Config()
	if err != nil {
		return err
	}
	config.OnClose = func(s *kcp.Session, err error) {
		fmt.Fprintf(out, "session %s conv %08x: closed: %v\n", s.RemoteAddr(), s.Conv(), err)
	}
	srv, err := kcp.NewServer(kcp.ServerConfig{Config: config})
	if err != nil {
		return err
	}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *addr, Handler: srv})
	if err != nil {
		return err
	}
	go func() {
		for {
			c, err := srv.Accept()
			if err != nil {
				return // the server closed
			}
			s := c.(*kcp.Session)
			fmt.Fprintf(out, "session %s conv %08x: open\n", s.RemoteAddr(), s.Conv())
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	defer srv.Close()
	return example.Serve(ctx, out, ready, "KCP echo server listening on %s ("+kcpFlags.Describe()+")", engine)
}
