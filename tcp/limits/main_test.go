//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// Connections past MaxConns are refused and counted, the flood ends with
// ErrSendQueueFull, and the settings list shows what the example chose.
func TestRunLimits(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-max-conns", "2", "-conns", "4"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"connection 1: echoed ping",
		"connection 2: echoed ping",
		"connection 3: refused: closed as it was accepted",
		"connection 4: refused: closed as it was accepted",
		"stats: OpenConns 2, ConnsRefused 2",
		"flood: fib: connection send queue full after ",
		"(ErrSendQueueFull: true)",
		"  maxConns = 2",
		"  pollBatch = 256",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
	// -log-settings prints through fib's logger, not out.
	if err := run(context.Background(), []string{"-log-settings"}, io.Discard); err != nil {
		t.Error(err)
	}
}
