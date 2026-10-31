//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"context"
	"io"
	"testing"
)

// The handshake and the echo are tested from the client's side, in
// ../client, which runs this server's code through it.
func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "127.0.0.1:99999"}} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%q: started", args)
		}
	}
}
