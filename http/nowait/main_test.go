//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Both downloads arrive whole, the slow one finds the client behind at least
// once, and the access log reads back the value its middleware kept.
func TestRunDownloads(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), nil, &out, nil); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"client: GET /stream?mb=16 read quickly, 16777216 bytes",
		"server: /stream?mb=16 200, 16777216 bytes",
		"client: GET /stream?mb=4 read slowly, 4194304 bytes",
		"server: /scratch 200, ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if !regexp.MustCompile(`server: /stream\?mb=4 200, 4194304 bytes in \S+, client behind [1-9]`).MatchString(text) {
		t.Errorf("the slow client was never behind:\n%s", text)
	}
}

// -serve serves until the context ends, and /scratch answers each request
// from a buffer it has already reused.
func TestRunServes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		done <- run(ctx, []string{"-serve", "-addr", "127.0.0.1:0"}, &syncWriter{w: &out}, func(a string) { addrs <- a })
	}()
	base := "http://" + <-addrs
	client := &http.Client{Timeout: 10 * time.Second}
	seen := map[string]bool{}
	for range 3 {
		resp, err := client.Get(base + "/scratch")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.HasPrefix(string(body), "built in a pooled buffer at ") || seen[string(body)] {
			t.Errorf("body %q", body)
		}
		seen[string(body)] = true
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}

// syncWriter is an output the engine's goroutines and the test share.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
