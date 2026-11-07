//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/kcp/settings"
	"github.com/lesismal/fib/kcp"
)

// serveEcho runs a KCP echo server with the settings args give until the
// test ends, and returns its address.
func serveEcho(t *testing.T, args ...string) string {
	t.Helper()
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	f := settings.FlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	config, err := f.Config()
	if err != nil {
		t.Fatal(err)
	}
	config.OnMessage = func(s *kcp.Session, msg []byte) { _ = s.Send(msg) }
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

func TestRunPrintsTheEchoes(t *testing.T) {
	for _, args := range [][]string{nil, {"-mode", "fast3", "-crypt", "none", "-ds", "0"}, {"-crypt", "aes-128-gcm", "-key", "k"}} {
		addr := serveEcho(t, args...)
		var out bytes.Buffer
		if err := run(append([]string{"-addr", addr, "-n", "3"}, args...), &out); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out.String())
		}
		for i := 1; i <= 3; i++ {
			if !strings.Contains(out.String(), fmt.Sprintf("echo: hello from fib #%d\n", i)) {
				t.Fatalf("%v: output %q", args, out.String())
			}
		}
	}
}

// A server with another key never answers, and bad flags are refused.
func TestRunFailures(t *testing.T) {
	old := echoTimeout
	defer func() { echoTimeout = old }()
	echoTimeout = 300 * time.Millisecond
	addr := serveEcho(t, "-key", "another")
	if err := run([]string{"-addr", addr}, io.Discard); err == nil || !strings.Contains(err.Error(), "no echo") {
		t.Errorf("mismatched key: %v", err)
	}
	for _, args := range [][]string{{"-nope"}, {"-mode", "turbo"}, {"-addr", "no port"}} {
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
