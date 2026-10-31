//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// The first connection makes a full handshake; the later ones resume, and
// their GETs arrive as early data; the POST waits for the handshake. -suite
// narrows the offer to the one suite named.
func TestRunEarlyData(t *testing.T) {
	for _, suite := range []string{"", "chacha20", "aes256"} {
		var out bytes.Buffer
		if err := run(context.Background(), []string{"-suite", suite}, &out); err != nil {
			t.Fatalf("-suite %q: %v\n%s", suite, err, out.String())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		want := []string{
			"server: https://",
			"client: GET: 200 OK HTTP/1.1 GET, early data: no, ",
			"client: GET: 200 OK HTTP/1.1 GET, early data: 1, ",
			"client: GET: 200 OK HTTP/1.1 GET, early data: 1, ",
			"client: POST: 200 OK HTTP/1.1 POST, early data: no, ",
		}
		if len(lines) != len(want) {
			t.Fatalf("-suite %q: output\n%s", suite, out.String())
		}
		for i, w := range want {
			if !strings.HasPrefix(lines[i], w) {
				t.Errorf("-suite %q: line %d %q, want %q", suite, i, lines[i], w)
			}
		}
		if !strings.HasSuffix(lines[1], "resumed false") || !strings.HasSuffix(lines[2], "resumed true") {
			t.Errorf("-suite %q: resumption\n%s", suite, out.String())
		}
		name := map[string]string{"chacha20": "TLS_CHACHA20_POLY1305_SHA256", "aes256": "TLS_AES_256_GCM_SHA384"}[suite]
		if name != "" && strings.Count(out.String(), name) != 4 {
			t.Errorf("-suite %q: not %s throughout\n%s", suite, name, out.String())
		}
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-suite", "rot13"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
