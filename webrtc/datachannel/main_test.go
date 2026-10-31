//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func check(t *testing.T, out, pair string) {
	t.Helper()
	for _, label := range []string{"ordered", "unordered", "partial"} {
		if !strings.Contains(out, "client: "+label+" ") || !strings.Contains(out, "10/10 echoed") {
			t.Fatalf("no echoes on %s:\n%s", label, out)
		}
	}
	if !strings.Contains(out, "ordered   10/10 echoed, in order") {
		t.Fatalf("the ordered channel:\n%s", out)
	}
	if !strings.Contains(out, pair) {
		t.Fatalf("want a %s pair:\n%s", pair, out)
	}
}

func TestDirect(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	check(t, out.String(), "host")
}

func TestRelayed(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-turn"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	check(t, out.String(), "relay ->")
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "127.0.0.1:99999"}, {"-turn", "-addr", ":0"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
