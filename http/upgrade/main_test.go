//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
)

// The proxy switches once the backend has, the lines and the bulk come back
// upper-cased, and the first line, sent with the request, is not lost.
func TestRunRelays(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-size", "2097152"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"client: 101 Switching Protocols, Upgrade: shout",
		`client: sent "hello from fib #1", got "HELLO FROM FIB #1"`,
		`client: sent "hello from fib #3", got "HELLO FROM FIB #3"`,
		"client: 2097152 bytes through the tunnel and back, upper-cased",
		"proxy: held one side's reads ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
}

// A request that does not ask to switch is told to, and one the backend
// will not switch is answered 502.
func TestProxyRefusals(t *testing.T) {
	var holds atomic.Int64
	clientEngine, stopClient := example.ClientEngine()
	defer stopClient()
	client := fibhttp.NewClient(clientEngine, fibhttp.DefaultClientConfig())
	defer client.Close()
	r := fibhttp.NewRouter()
	// The backend is this same engine's /plain, which never switches.
	r.Get("/plain", func(c *fibhttp.Context) { _ = c.Respond(http.StatusOK, "text/plain", []byte("no")) })
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: "127.0.0.1:0", Handler: fibhttp.NewHandler(r)})
	if err != nil {
		t.Fatal(err)
	}
	r.Get("/", proxy(client, "http://"+example.Addr(engine)+"/plain", &holds))
	stop := example.Start(engine)
	defer stop()
	base := "http://" + example.Addr(engine) + "/"

	resp, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("a plain GET: %s", resp.Status)
	}
	req, _ := http.NewRequest(http.MethodGet, base, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("an upgrade the backend refused: %s", resp.Status)
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
