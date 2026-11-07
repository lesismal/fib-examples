//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesismal/fib-examples/quic/quictest"
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

// The server echoes a stream, larger than a datagram, and reports the
// connection it came on, open and then closed.
func TestRunEchoesStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	certFile := filepath.Join(t.TempDir(), "cert.pem")
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-cert-out", certFile}, out, func(addr string) { addrs <- addr })
	}()
	addr := <-addrs

	pem, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	msg := bytes.Repeat([]byte("quic "), 20000)
	echo, err := quictest.Echo(addr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}, msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(echo, msg) {
		t.Fatalf("echo of %d bytes came back as %d", len(msg), len(echo))
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), ": closed: ") {
		if time.Now().After(deadline) {
			t.Fatalf("the connection was not reported closed: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"QUIC echo server listening on " + addr, `open, ALPN "fib-echo"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q lacks %q", text, want)
		}
	}
}

func TestRunErrors(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address", "-cert-out", ""}, {"-cert", "/nonexistent"}} {
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
