//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command limits bounds what an engine's peers can cost it: how many
// connections it holds, and how much it queues for a peer that stops
// reading; and reads back the settings the engine runs with.
//
//	go run ./tcp/limits
//	go run ./tcp/limits -log-settings      # also prints fib's settings table
//
// Config.MaxConns caps the connections the bindings have accepted and not
// yet closed: one past it is closed as it is accepted, before any handler
// hears of it, and counted in Stats.ConnsRefused. Dialed connections and
// UDP peers do not count.
//
// WriteBufferHighWatermark pauses a connection's reads while its replies
// back up, but nothing in it bounds what the application itself sends, a
// broadcast to a peer that has stopped reading, say. Config.MaxWriteQueueBytes
// caps the bytes queued for each connection's socket, and
// Connection.SetWriteQueueLimit one connection's: the send that would pass it
// closes the connection with ErrSendQueueFull instead.
//
// Engine.Settings is the configuration after fib has filled in its defaults
// and sized itself to the machine; Settings.Parameters lists each value with
// whether the caller set it. Config.LogSettings prints that list as the
// engine starts (example.EngineConfig turns it off), and Engine.LogSettings
// prints it on demand.
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
	flags := flag.NewFlagSet("limits", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address")
	maxConns := flags.Int("max-conns", 3, "Config.MaxConns")
	conns := flags.Int("conns", 5, "connections to open")
	queue := flags.Int64("queue", 4<<20, "Config.MaxWriteQueueBytes")
	logSettings := flags.Bool("log-settings", false, "print fib's own settings table too")
	if err := flags.Parse(args); err != nil {
		return err
	}

	config := example.EngineConfig()
	config.MaxConns = *maxConns
	config.MaxWriteQueueBytes = *queue
	// The most events one wait of an event loop collects, 1024 by default:
	// smaller hands each round's events to the workers sooner.
	config.PollBatchSize = 256
	floods := make(chan string, 1)
	engine, err := fib.New(config, fib.Binding{Addr: *addr, Handler: fib.HandlerFuncs{
		Data: func(c *fib.Connection, data []byte) {
			if string(data) == "flood" {
				go flood(c, floods)
				return
			}
			_ = c.Send(data)
		},
	}})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	server := example.Addr(engine)

	// Connections, one after another: those past MaxConns are closed as
	// they are accepted.
	var open []net.Conn
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	for i := 1; i <= *conns; i++ {
		c, err := net.DialTimeout("tcp", server, 5*time.Second)
		if err != nil {
			return err
		}
		open = append(open, c)
		fmt.Fprintf(out, "connection %d: %s\n", i, echo(c))
	}
	stats := engine.Stats()
	fmt.Fprintf(out, "stats: OpenConns %d, ConnsRefused %d\n", stats.OpenConns, stats.ConnsRefused)

	// A peer that asks for a flood and reads none of it.
	if _, err := open[0].Write([]byte("flood")); err != nil {
		return err
	}
	select {
	case r := <-floods:
		fmt.Fprintln(out, r)
	case <-time.After(30 * time.Second):
		return errors.New("the flood was never stopped")
	case <-ctx.Done():
		return ctx.Err()
	}

	fmt.Fprintln(out, "settings the caller chose:")
	for _, p := range engine.Settings().Parameters() {
		if p.Source == "custom" {
			fmt.Fprintf(out, "  %s = %s\n", p.Name, p.Value)
		}
	}
	if *logSettings {
		engine.LogSettings()
	}
	return nil
}

// echo sends a line and says what came back.
func echo(c net.Conn) string {
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return "refused: " + err.Error()
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil {
		return "refused: closed as it was accepted"
	}
	return "echoed " + string(b)
}

// flood sends to c until its send queue passes the cap, which closes it.
func flood(c *fib.Connection, result chan<- string) {
	// Tighter than the engine's MaxWriteQueueBytes, for this connection.
	c.SetWriteQueueLimit(1 << 20)
	chunk := []byte(strings.Repeat("x", 64<<10))
	var sent int64
	for {
		if err := c.Send(chunk); err != nil {
			result <- fmt.Sprintf("flood: %v after %d MiB sent, the peer reading none (ErrSendQueueFull: %v)",
				err, sent>>20, errors.Is(err, fib.ErrSendQueueFull))
			return
		}
		sent += int64(len(chunk))
	}
}
