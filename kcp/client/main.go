//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command client sends messages to the KCP echo server and prints the
// echoes.
//
//	go run ./kcp/client
//	go run ./kcp/client -mode fast3 -crypt none -ds 0   # as the server was started
//
// kcp.Dial starts a conversation from a UDP binding of its own on a client
// engine and returns a net.Conn at once: KCP has no handshake, and the
// server hears of the conversation with its first segment. Read and Write
// carry a stream of bytes, so the client reads back as many bytes as it
// wrote. The settings must match the server's: a different crypt or key
// fails every datagram's checksum, and the server never answers.
package main

import (
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
	addr := flags.String("addr", "127.0.0.1:9002", "server address")
	count := flags.Int("n", 5, "messages to send")
	kcpFlags := settings.FlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	config, err := kcpFlags.Config()
	if err != nil {
		return err
	}

	engine, stop := example.ClientEngine()
	defer stop()
	conn, err := kcp.Dial(engine, *addr, config)
	if err != nil {
		return err
	}
	defer conn.Close()

	for i := 1; i <= *count; i++ {
		msg := example.Message(i)
		if _, err := conn.Write([]byte(msg)); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(echoTimeout))
		echo := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, echo); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return errors.New("no echo: is the server running, with the same -crypt, -key, -ds and -ps?")
			}
			return err
		}
		fmt.Fprintf(out, "echo: %s\n", echo)
	}
	fmt.Fprintf(out, "rtt %v, %d segments sent, %d retransmitted\n", conn.RTT(), conn.Stats().Sent, conn.Stats().Retransmitted)
	return nil
}
