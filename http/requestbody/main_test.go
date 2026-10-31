//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// The large avatar is refused with none of it sent, the chat's messages each
// reach the handler before the next is sent, the dropped upload reaches both
// the handler's OnBody and the middleware's OnCancel, and both OnCancel
// callbacks hear the abandoned report, the handler's first.
func TestRunRequests(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	want := []string{
		`client: POST /avatar of 1024 bytes: 200 OK "avatar of 1024 bytes saved", 1024 bytes of the body sent`,
		`client: POST /avatar of 8388608 bytes: 413 Request Entity Too Large "Request Entity Too Large", 0 bytes of the body sent`,
		"server: /avatar refused: http: request body too large",
		"server: /chat handler running, body complete: false",
		`server: /chat got "hello from fib #1"`,
		`server: /chat got "hello from fib #2"`,
		`server: /chat got "hello from fib #3"`,
		`client: POST /chat: 200 OK "51 bytes of chat"`,
		"server: /upload receiving",
		"client: dropped POST /upload: the user cancelled the upload",
		"server: /upload's OnBody: ",
		"server: middleware's OnCancel: ",
		"server: /report building",
		"client: abandoned GET /report: the user closed the tab",
		"server: handler's OnCancel stopped the report: ",
		"server: middleware's OnCancel: ",
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(want), out.String())
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d: %q, want %q", i, lines[i], w)
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
