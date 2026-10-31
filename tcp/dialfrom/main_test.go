//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// Over TCP and UDP, every dial leaves from a port in the range, and a range
// whose one port is held fails with ErrNoLocalPort.
func TestRunDialFrom(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			var out bytes.Buffer
			if err := run(context.Background(), []string{"-network", network, "-ports", "20100-20139"}, &out); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			text := out.String()
			if n := strings.Count(text, ", in the range: true"); n != 3 {
				t.Errorf("%d dials from the range, want 3:\n%s", n, text)
			}
			if !strings.Contains(text, "(ErrNoLocalPort: true)") {
				t.Errorf("the held port did not fail with ErrNoLocalPort:\n%s", text)
			}
		})
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-nope"},
		{"-network", "unix"},
		{"-local", "not an address"},
		{"-ports", "9-1"},
		{"-ports", "0-10"},
		{"-ports", "100"},
	} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
