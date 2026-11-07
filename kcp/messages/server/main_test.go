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

// The server echoes each message whole, and closes a session gone quiet
// for -idle.
func TestRunEchoesMessagesAndClosesIdleSessions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-idle", "300ms"}, out, func(addr string) { addrs <- addr })
	}()
	addr := <-addrs

	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	config, err := settings.FlagsOn(flags).Config()
	if err != nil {
		t.Fatal(err)
	}
	echoes := make(chan []byte, 1)
	config.OnMessage = func(_ *kcp.Session, msg []byte) { echoes <- bytes.Clone(msg) }
	config.IdleTimeout = -1
	engine, err := fib.New(fib.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	running := make(chan error, 1)
	go func() { running <- engine.Run() }()
	defer func() { engine.Stop(); <-running; _ = engine.Close() }()
	s, err := kcp.Dial(engine, addr, config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	msg := bytes.Repeat([]byte{7}, 50*s.MSS())
	if err := s.Send(msg); err != nil {
		t.Fatal(err)
	}
	select {
	case echo := <-echoes:
		if !bytes.Equal(echo, msg) {
			t.Fatalf("echo of %d bytes came back as %d", len(msg), len(echo))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no echo")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "closed: kcp: peer idle") {
		if time.Now().After(deadline) {
			t.Fatalf("the idle session was not closed: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"KCP message echo server listening on " + addr, ": open", "-byte message"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q lacks %q", text, want)
		}
	}
}

func TestRunErrors(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}, {"-crypt", "salsa20"}, {"-ds", "300", "-ps", "1"}} {
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
