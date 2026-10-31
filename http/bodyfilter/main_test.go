//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The requests show each filter at work: streamed events compressed one by
// one, a whole page compressed, HEAD aborting its filter, and the plugin's
// header and body.
func TestRunRequests(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"/events data: event 5",
		`/page Accept-Encoding gzip=false: 1960 bytes on the wire, Content-Encoding "", Content-Length 1960`,
		`Content-Encoding "gzip", Content-Length `,
		`HEAD /page: Content-Encoding "gzip"`,
		`/shout: X-Policy "checked 200", body "FIB FILTERS RESPONSE BOD"`,
		"gzip filters: 2 closed, 1 aborted",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
}

// -serve serves until the context ends.
func TestRunServes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-serve", "-addr", "127.0.0.1:0"}, io.Discard, func(a string) { addrs <- a })
	}()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + <-addrs + "/page")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// net/http asked for gzip itself, and undid it.
	if !resp.Uncompressed {
		t.Errorf("response not compressed: %v", resp.Header)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
