//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// The HTTP/3 origin is reached over TCP once and over HTTP/3 after; the
// origin whose UDP is blocked is answered over TCP every time.
func TestRunFollowsAltSvc(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{
		"h3 origin, request 1: answered over HTTP/2.0",
		"h3 origin, request 2: answered over HTTP/3.0",
		"h3 origin, request 3: answered over HTTP/3.0",
		"UDP blocked, request 1: answered over HTTP/2.0",
		"UDP blocked, request 2: answered over HTTP/2.0",
		"UDP blocked, request 3: answered over HTTP/2.0",
	}
	if len(lines) != len(want) {
		t.Fatalf("output\n%s", out.String())
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d: %q, want %q", i, lines[i], w)
		}
	}
	if !strings.Contains(lines[0], `Alt-Svc "h3=`) {
		t.Errorf("no Alt-Svc on the first response: %q", lines[0])
	}
}

func TestRunErrors(t *testing.T) {
	if err := run(context.Background(), []string{"-nope"}, io.Discard); err == nil {
		t.Error("accepted an unknown flag")
	}
}
