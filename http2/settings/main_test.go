//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// The transfers complete, the silent server is found by the client's PING,
// the flood ends in ENHANCE_YOUR_CALM, the server announces what Config.HTTP2
// set and PINGs a quiet client, and closing the engine sends GOAWAY.
func TestRunShowsEachSetting(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-size", "4194304"}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"client: HTTP/2.0 GET /blob: 4194304 bytes",
		"client: HTTP/2.0 POST /blob: server read 4194304 bytes",
		"client: silent server found dead after ",
		"no answer to PING within PingTimeout",
		"raw: 100 PINGs at once, server GOAWAY ENHANCE_YOUR_CALM",
		"INITIAL_WINDOW_SIZE=4194304",
		"MAX_FRAME_SIZE=65536",
		"HEADER_TABLE_SIZE=16384",
		"raw: server PING after ",
		"raw: engine closed, server GOAWAY NO_ERROR",
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
}

func TestFrameStrings(t *testing.T) {
	if got := settingsString([]byte{0, 9, 0, 0, 0, 1}); got != "0x9=1" {
		t.Errorf("settingsString: %q", got)
	}
	if got := goAwayString([]byte{0, 0, 0, 3, 0, 0, 0, 0x2}); got != "0x2, last stream 3" {
		t.Errorf("goAwayString: %q", got)
	}
	if got := goAwayString([]byte{1}); got != "01" {
		t.Errorf("goAwayString: %q", got)
	}
}
