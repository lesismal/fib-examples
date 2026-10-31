//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// With Retry the client's first Initial carries no token and the ones after
// the Retry carry its token; without, none does. Path MTU discovery grows
// the client's datagrams past 1200 bytes, and without it they stay there.
func TestRunRetryAndPMTUD(t *testing.T) {
	for _, c := range []struct {
		args            []string
		retried, probed bool
	}{
		{nil, true, true},
		{[]string{"-retry=false", "-no-pmtud"}, false, false},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), append(c.args, "-size", "262144"), &out); err != nil {
			t.Fatalf("%q: %v\n%s", c.args, err, out.String())
		}
		text := out.String()
		if strings.Count(text, "HTTP/3.0 200 OK, 262144 bytes") != 2 {
			t.Errorf("%q: uploads\n%s", c.args, text)
		}
		var plain, token, largest int
		for _, line := range strings.Split(text, "\n") {
			fmt.Sscanf(line, "server: client Initials without a token: %d, with one: %d", &plain, &token)
			fmt.Sscanf(line, "server: largest datagram from the client: %d bytes", &largest)
		}
		if plain < 1 || (token > 0) != c.retried {
			t.Errorf("%q: %d Initials without a token, %d with, retried %v\n%s", c.args, plain, token, c.retried, text)
		}
		if (largest > 1200) != c.probed {
			t.Errorf("%q: largest datagram %d, probed %v\n%s", c.args, largest, c.probed, text)
		}
	}
}

func TestInitialToken(t *testing.T) {
	initial := []byte{0xc3, 0, 0, 0, 1, 2, 0xaa, 0xbb, 1, 0xcc, 0x40, 0x05}
	if n, ok := initialToken(initial); !ok || n != 5 {
		t.Errorf("token %d %v, want 5", n, ok)
	}
	for _, d := range [][]byte{
		{0x40, 1, 2, 3, 4, 5, 6, 7},          // a short header
		{0xe3, 0, 0, 0, 1, 0, 0, 0},          // a Handshake packet
		{0xc3, 0, 0, 0, 1, 9, 1},             // a truncated connection ID
		{0xc3, 0, 0, 0, 1, 0, 0, 0x80, 1, 2}, // a truncated token length
		{0xc3, 0, 0, 0, 1, 0, 0},             // no token length at all
	} {
		if _, ok := initialToken(d); ok {
			t.Errorf("%x taken for an Initial", d)
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
