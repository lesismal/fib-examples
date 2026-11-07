//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lesismal/fib-examples/http/upload/uploadtest"
	"github.com/lesismal/fib-examples/quic/quictest"
)

func TestRunPrintsTheEchoes(t *testing.T) {
	serverTLS, _, certFile := uploadtest.TLS(t)
	addr := quictest.Serve(t, serverTLS, func(b []byte) []byte { return b })
	var out bytes.Buffer
	if err := run([]string{"-addr", addr, "-n", "3", "-ca", certFile}, &out); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if !strings.Contains(out.String(), `connected, ALPN "fib-echo"`) {
		t.Fatalf("output %q", out.String())
	}
	// Client-opened bidirectional streams are numbered 0, 4, 8...
	for i := 1; i <= 3; i++ {
		if !strings.Contains(out.String(), fmt.Sprintf("echo on stream %d: hello from fib #%d\n", 4*(i-1), i)) {
			t.Fatalf("output %q", out.String())
		}
	}
}

// A server whose certificate the client does not trust, one that never
// answers, and bad flags are reported.
func TestRunFailures(t *testing.T) {
	old := timeout
	defer func() { timeout = old }()
	timeout = 500 * time.Millisecond

	serverTLS, _, _ := uploadtest.TLS(t)
	_, _, otherCert := uploadtest.TLS(t)
	addr := quictest.Serve(t, serverTLS, func(b []byte) []byte { return b })
	if err := run([]string{"-addr", addr, "-ca", otherCert}, io.Discard); err == nil {
		t.Error("trusted a certificate it should not have")
	}

	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if err := run([]string{"-addr", silent.LocalAddr().String(), "-insecure"}, io.Discard); err == nil {
		t.Error("no error from a server that never answers")
	}

	for _, args := range [][]string{{"-nope"}, {"-addr", "no port", "-insecure"}, {"-ca", "/nonexistent"}} {
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
