//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// The brief peer times out first, the default one at the engine's timeout,
// and the one with none stays.
func TestRunTimesPeersOut(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-idle", "800ms", "-brief", "100ms"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{
		`server: "brief" peer opened, SetDontFragment: <nil>`,
		`server: "default" peer opened, SetDontFragment: <nil>`,
		`server: "keep" peer opened, SetDontFragment: <nil>`,
		`server: "brief" peer closed after about `,
		`server: "default" peer closed after about `,
		`client: "keep" peer still answered`,
	}
	if len(lines) != len(want) {
		t.Fatalf("output\n%s", out.String())
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d: %q, want %q", i, lines[i], w)
		}
	}
	if !strings.HasSuffix(lines[3], "fib: udp peer idle timeout") {
		t.Errorf("close reason %q", lines[3])
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
