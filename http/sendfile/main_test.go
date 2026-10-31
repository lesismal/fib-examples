//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The whole file and a range arrive intact, and a client that reads nothing
// loses /slow after the stall, over HTTP and HTTPS.
func TestRunDownloads(t *testing.T) {
	for _, args := range [][]string{nil, {"-tls"}} {
		var out bytes.Buffer
		if err := run(context.Background(), append(args, "-stall", "300ms"), &out, nil); err != nil {
			t.Fatalf("%q: %v\n%s", args, err, out.String())
		}
		text := out.String()
		for _, want := range []string{
			"client: GET /file 200 OK, 33554432 bytes",
			"SHA-256 matches",
			"server: SendFile 33554432 bytes done: <nil>",
			"206 Partial Content, Content-Range bytes 1000-1999/33554432",
			"server: SendFile 1000 bytes done: <nil>",
			"server: SendFileStall 33554432 bytes, stall 300ms done: ",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%q: no %q in\n%s", args, want, text)
			}
		}
		if strings.Contains(text, "SendFileStall 33554432 bytes, stall 300ms done: <nil>") {
			t.Errorf("%q: /slow went whole to a client that read nothing\n%s", args, text)
		}
	}
}

// -serve serves the file it is given.
func TestRunServesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-serve", "-addr", "127.0.0.1:0", "-file", path}, io.Discard, func(a string) { addrs <- a })
	}()
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", "http://"+<-addrs+"/file", nil)
	req.Header.Set("Range", "bytes=7-")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "789" {
		t.Errorf("%s %q", resp.Status, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestByteRange(t *testing.T) {
	for _, c := range []struct {
		header      string
		first, last int64
		ok          bool
	}{
		{"bytes=0-9", 0, 9, true},
		{"bytes=5-", 5, 99, true},
		{"bytes=90-200", 90, 99, true},
		{"bytes=100-", 0, 0, false},
		{"bytes=5-4", 0, 0, false},
		{"bytes=0-1,4-5", 0, 0, false},
		{"bytes=x-", 0, 0, false},
		{"items=0-1", 0, 0, false},
	} {
		first, last, ok := byteRange(c.header, 100)
		if first != c.first || last != c.last || ok != c.ok {
			t.Errorf("%q: %d %d %v", c.header, first, last, ok)
		}
	}
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-addr", "not an address"}, {"-file", "/no/such/file"}} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
