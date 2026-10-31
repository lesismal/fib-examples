//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// Every line is answered before the next is sent, which only a handler that
// writes while its body is still arriving can do.
func TestRunChats(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-n", "4"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "client: HTTP/2.0 200 OK") {
		t.Fatalf("output %q", text)
	}
	for i := 1; i <= 4; i++ {
		want := `got "HELLO FROM FIB #` + string(rune('0'+i)) + `"`
		if !strings.Contains(text, want) {
			t.Fatalf("no %s in %q", want, text)
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
