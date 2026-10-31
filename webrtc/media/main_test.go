//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// Both streams arrive with the payload types the answer negotiated, and the
// receiver's PLI reaches the sender.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-duration", "1500ms"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"video/VP8 PT 96", "audio/opus PT 111", "asks for a key frame"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %q in\n%s", want, out.String())
		}
	}
}

func TestRunErrors(t *testing.T) {
	if err := run(context.Background(), []string{"-nope"}, io.Discard); err == nil {
		t.Error("accepted an unknown flag")
	}
}
