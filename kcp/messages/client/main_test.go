//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/kcp/settings"
	"github.com/lesismal/fib/kcp"
)

// serve runs a KCP server answering each message with reply until the test
// ends, and returns its address.
func serve(t *testing.T, reply func(msg []byte) []byte) string {
	t.Helper()
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	config, err := settings.FlagsOn(flags).Config()
	if err != nil {
		t.Fatal(err)
	}
	config.OnMessage = func(s *kcp.Session, msg []byte) { _ = s.Send(reply(msg)) }
	srv, err := kcp.NewServer(kcp.ServerConfig{Config: config, OnSession: func(*kcp.Session) {}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := fib.New(fib.DefaultConfig(), fib.Binding{Network: "udp", Addr: "127.0.0.1:0", Handler: srv})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := engine.LocalUDPAddr()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run() }()
	t.Cleanup(func() {
		engine.Stop()
		<-done
		_ = engine.Close()
	})
	return addr.String()
}

func TestRunChecksEachMessage(t *testing.T) {
	addr := serve(t, func(msg []byte) []byte { return msg })
	var out bytes.Buffer
	if err := run([]string{"-addr", addr, "-n", "4", "-size", "40000"}, &out); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if n := strings.Count(out.String(), ", whole\n"); n != 4 {
		t.Fatalf("output %q", out.String())
	}
	if !strings.Contains(out.String(), "echo: 40000 bytes in ") {
		t.Fatalf("output %q", out.String())
	}
}

// A message that comes back changed, a server that does not answer, and
// bad flags are reported.
func TestRunFailures(t *testing.T) {
	changed := serve(t, func(msg []byte) []byte { return msg[1:] })
	if err := run([]string{"-addr", changed}, io.Discard); err == nil || !strings.Contains(err.Error(), "different") {
		t.Errorf("changed echo: %v", err)
	}
	old := echoTimeout
	defer func() { echoTimeout = old }()
	echoTimeout = 300 * time.Millisecond
	addr := serve(t, func(msg []byte) []byte { return msg })
	if err := run([]string{"-addr", addr, "-key", "another"}, io.Discard); err == nil || !strings.Contains(err.Error(), "no echo") {
		t.Errorf("mismatched key: %v", err)
	}
	for _, args := range [][]string{{"-nope"}, {"-n", "0"}, {"-n", "5", "-size", "2"}, {"-mode", "turbo"}, {"-addr", "no port"}} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestMainReportsAnError(t *testing.T) {
	var got error
	oldFatal, oldArgs := fatal, os.Args
	defer func() { fatal, os.Args = oldFatal, oldArgs }()
	fatal = func(err error) { got = err }
	os.Args = []string{"client", "-nope"}
	main()
	if got == nil {
		t.Fatal("fatal was not called")
	}
}
