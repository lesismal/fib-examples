//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/kcp/settings"
	"github.com/lesismal/fib/kcp"
)

// syncBuffer is an output the engine's goroutines and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// defaultConfig is the KCP config the server's default flags give.
func defaultConfig(t *testing.T) kcp.Config {
	t.Helper()
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	f := settings.FlagsOn(flags)
	c, err := f.Config()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The server echoes what a session writes and reports its sessions.
func TestRunEchoes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0"}, out, func(addr string) { addrs <- addr })
	}()
	addr := <-addrs

	engine, err := fib.New(fib.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	running := make(chan error, 1)
	go func() { running <- engine.Run() }()
	defer func() { engine.Stop(); <-running; _ = engine.Close() }()
	conn, err := kcp.Dial(engine, addr, defaultConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	msg := bytes.Repeat([]byte("kcp "), 10000) // many segments, FEC sets
	go func() { _, _ = conn.Write(msg) }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	echo := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, echo); err != nil || !bytes.Equal(echo, msg) {
		t.Fatalf("echo of %d bytes: %v", len(msg), err)
	}
	_ = conn.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"KCP echo server listening on " + addr + " (mode fast, crypt aes, FEC 10/3)", ": open", ": closed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q lacks %q", text, want)
		}
	}
}

func TestRunErrors(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}, {"-mode", "turbo"}, {"-ds", "300", "-ps", "1"}} {
		if err := run(ctx, args, io.Discard, nil); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestMainReportsAStartupError(t *testing.T) {
	var got error
	oldFatal, oldArgs := fatal, os.Args
	defer func() { fatal, os.Args = oldFatal, oldArgs }()
	fatal = func(err error) { got = err }
	os.Args = []string{"server", "-nope"}
	main()
	if got == nil {
		t.Fatal("fatal was not called")
	}
}
