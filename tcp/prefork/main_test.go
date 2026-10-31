//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Serving in this process, every connection is answered by it, child 0 of 1.
func TestRunInPlace(t *testing.T) {
	for _, args := range [][]string{{"-children", "0"}, {"-children", "2", "-balance=false"}} {
		if runtime.GOOS == "linux" && args[1] != "0" {
			continue // prefork would start children from the test binary; see TestPreforkLinux
		}
		ctx, cancel := context.WithCancel(context.Background())
		addrs := make(chan string, 1)
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, append(args, "-addr", "127.0.0.1:0"), io.Discard, func(a string) { addrs <- a })
		}()
		var out bytes.Buffer
		if err := run(context.Background(), []string{"-addr", <-addrs, "-dial", "6"}, &out, nil); err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^child 0 of 1, pid \d+: 6 connections\n$`).MatchString(out.String()) {
			t.Errorf("%q: tally %q", args, out.String())
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// On Linux the program, built and run, starts its children, and they share
// the connections.
func TestPreforkLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("prefork serves in one process outside Linux; TestRunInPlace covers that")
	}
	if testing.Short() {
		t.Skip("builds the program")
	}
	bin := filepath.Join(t.TempDir(), "prefork")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cmd := exec.Command(bin, "-addr", addr, "-children", "2")
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	// Both children say they are listening.
	lines := bufio.NewScanner(stdout)
	ready := make(chan struct{})
	go func() {
		seen := 0
		for lines.Scan() {
			if strings.Contains(lines.Text(), "echo listening on") {
				if seen++; seen == 2 {
					close(ready)
				}
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the children never came up")
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-addr", addr, "-dial", "16"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	total, children := 0, 0
	for _, m := range regexp.MustCompile(`child [12] of 2, pid \d+: (\d+) connections`).FindAllStringSubmatch(out.String(), -1) {
		n, _ := strconv.Atoi(m[1])
		total += n
		children++
	}
	if total != 16 || children != 2 {
		t.Errorf("tally:\n%s", out.String())
	}
	t.Logf("tally:\n%s", out.String())
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"-children", "0", "-addr", "not an address"}, {"-dial", "1", "-addr", "127.0.0.1:1"}} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
