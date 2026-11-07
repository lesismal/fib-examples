//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command client sends messages of growing size to the KCP message echo
// server and checks that each comes back whole.
//
//	go run ./kcp/messages/client
//	go run ./kcp/messages/client -size 100000
//
// Session.Send sends one message, split into as many segments as it needs
// (up to 255 of Session.MSS bytes), and the peer's Config.OnMessage gets
// it back together: unlike Read and Write, the boundaries are kept. The
// n-th message here is -size*n/-n bytes, so the last ones span many
// segments.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib-examples/kcp/settings"
	"github.com/lesismal/fib/kcp"
)

// fatal reports an error and exits; a test replaces it.
var fatal = example.Fatal

// echoTimeout bounds the wait for each echo; a test shortens it.
var echoTimeout = 3 * time.Second

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fatal(err)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:9003", "server address")
	count := flags.Int("n", 5, "messages to send")
	size := flags.Int("size", 20000, "size of the largest message, the last")
	kcpFlags := settings.FlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *count < 1 || *size < *count {
		return errors.New("-n must be at least 1 and -size at least -n")
	}
	config, err := kcpFlags.Config()
	if err != nil {
		return err
	}
	// OnMessage runs on a fib worker: hand each echo to this goroutine,
	// copied, since msg is only valid during the call.
	echoes := make(chan []byte, *count)
	config.OnMessage = func(_ *kcp.Session, msg []byte) { echoes <- bytes.Clone(msg) }

	engine, stop := example.ClientEngine()
	defer stop()
	s, err := kcp.Dial(engine, *addr, config)
	if err != nil {
		return err
	}
	defer s.Close()

	for i := 1; i <= *count; i++ {
		n := *size * i / *count
		msg := bytes.Repeat([]byte(example.Message(i)+" "), n/len(example.Message(i))+1)[:n]
		if err := s.Send(msg); err != nil {
			return err
		}
		select {
		case echo := <-echoes:
			if !bytes.Equal(echo, msg) {
				return fmt.Errorf("message %d came back as %d different bytes", i, len(echo))
			}
			segments := (len(msg) + s.MSS() - 1) / s.MSS()
			fmt.Fprintf(out, "echo: %d bytes in %d segments, whole\n", len(echo), segments)
		case <-time.After(echoTimeout):
			return errors.New("no echo: is the server running, with the same -crypt, -key, -ds and -ps?")
		}
	}
	return nil
}
