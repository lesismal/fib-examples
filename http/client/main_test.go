//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Each step does what its comment says: the third request is turned away,
// the slow header times out, the events arrive one by one with the hold
// between the second and the third, the small body is whole, the trailer's
// checksum matches, and the gzip transfer coding is decoded.
func TestRunSteps(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"header timeout: GET /wait?d=2s: http: timed out awaiting the response header",
		`events: "data: event 5"`,
		`body bytes: {"answer":42}, arrived whole: true`,
		"trailer: POST /upload: 40000 bytes, trailer X-Checksum ",
		"... matches",
		`transfer coding: [gzip chunked], 900 bytes decoded: "compressed by the transfer, not the content."`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	// Of three requests at once, one has the connection, one waits for it,
	// and one is turned away; which is which is up to the scheduler.
	if n := strings.Count(text, "(would be a 503)"); n != 1 {
		t.Errorf("%d requests turned away, want 1:\n%s", n, text)
	}
	if n := len(regexp.MustCompile(`pending: request \d: waited`).FindAllString(text, -1)); n != 2 {
		t.Errorf("%d requests answered, want 2:\n%s", n, text)
	}
	// The third event waited out the hold.
	at := regexp.MustCompile(`events: "data: event (\d)", (\S+) in`).FindAllStringSubmatch(text, -1)
	if len(at) != 5 {
		t.Fatalf("events %q", at)
	}
	second, _ := time.ParseDuration(at[1][2])
	third, _ := time.ParseDuration(at[2][2])
	if third-second < 250*time.Millisecond {
		t.Errorf("event 3 at %v, event 2 at %v: the hold did not hold", third, second)
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
	// A header timeout longer than the slow server's wait is no timeout.
	if err := run(context.Background(), []string{"-header-timeout", "5s"}, io.Discard); err == nil {
		t.Error("succeeded without the header timing out")
	}
}
