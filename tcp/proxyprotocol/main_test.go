//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"sync"
	"testing"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
)

// syncBuffer is an output the proxy's goroutines and the test share.
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

// The server reports the client's address, the one the proxy relayed, and
// not the proxy's own; a connection without the header is closed.
func TestRunReportsTheClient(t *testing.T) {
	out := &syncBuffer{}
	if err := run(context.Background(), nil, out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	m := regexp.MustCompile(`proxy: client (\S+), upstream connection from (\S+)\n`).FindStringSubmatch(text)
	if m == nil || m[1] == m[2] {
		t.Fatalf("output\n%s", text)
	}
	if !bytes.Contains([]byte(text), []byte("server, through the proxy: you are "+m[1]+"\n")) {
		t.Errorf("the server did not see the client %s\n%s", m[1], text)
	}
	if !bytes.Contains([]byte(text), []byte("server, straight: closed, no PROXY header")) {
		t.Errorf("output\n%s", text)
	}
}

func TestParseProxyV1(t *testing.T) {
	ap, err := parseProxyV1("PROXY TCP6 2001:db8::1 2001:db8::2 4000 80")
	if err != nil || ap.String() != "[2001:db8::1]:4000" {
		t.Errorf("%v %v", ap, err)
	}
	for _, line := range []string{
		"PROXY UNKNOWN",
		"GET / HTTP/1.1",
		"PROXY TCP4 nowhere 1.2.3.4 1 2",
		"PROXY TCP4 1.2.3.4 1.2.3.4 99999 2",
	} {
		if _, err := parseProxyV1(line); err == nil {
			t.Errorf("%q: parsed", line)
		}
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}

// A header that arrives in pieces, with the request right behind it in the
// same read, is put together and the request served.
func TestHeaderInPieces(t *testing.T) {
	r := fibhttp.NewRouter()
	r.Get("/", func(c *fibhttp.Context) {
		_ = c.Respond(http.StatusOK, "text/plain", []byte(c.Request.RemoteAddr))
	})
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: "127.0.0.1:0", Handler: &proxyHeader{next: fibhttp.NewHandler(r)}})
	if err != nil {
		t.Fatal(err)
	}
	stop := example.Start(engine)
	defer stop()
	conn, err := net.Dial("tcp", example.Addr(engine))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(conn, "PROXY TCP4 192.0.2.10 ")
	time.Sleep(50 * time.Millisecond)
	_, _ = io.WriteString(conn, "198.51.100.1 51234 8080\r\nGET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	reply, _ := io.ReadAll(conn)
	if !bytes.HasSuffix(reply, []byte("192.0.2.10:51234")) {
		t.Fatalf("reply %q", reply)
	}
}
