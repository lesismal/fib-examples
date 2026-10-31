//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// The binding speaks first, and every reply comes back to the port the
// datagrams left from, on the connection OpenUDPPeer opened.
func TestRunSpeaksFirst(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-n", "3"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "opened 127.0.0.1:") {
		t.Fatalf("output %q", out.String())
	}
	local := strings.Fields(lines[0])[1]
	for i, l := range lines[1:] {
		if !strings.Contains(l, "hello from fib #") || !strings.HasSuffix(l, "on "+local) {
			t.Fatalf("line %d: %q, want a reply on %s", i, l, local)
		}
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-to", "nowhere"}, {"-addr", "127.0.0.1:99999"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
