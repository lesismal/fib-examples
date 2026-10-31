//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lesismal/fib/webrtc/stun"
)

// A Binding request is answered with the address it came from; anything
// else is ignored.
func TestRunAnswersBinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- run(ctx, []string{"-addr", "127.0.0.1:0"}, &out, func(a string) { addrs <- a }) }()
	addr := <-addrs
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = conn.Write([]byte("not stun, ignored"))
	var req stun.Message
	req.Build(stun.BindingRequest, stun.NewTransactionID())
	if _, err := conn.Write(req.Raw); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1500)
	n, err := conn.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	var resp stun.Message
	if err := resp.Decode(b[:n]); err != nil || resp.Type != stun.BindingSuccess || resp.TransactionID != req.TransactionID {
		t.Fatalf("response %v %v", resp.Type, err)
	}
	if err := resp.CheckFingerprint(); err != nil {
		t.Fatal(err)
	}
	mapped, err := resp.XORAddress(stun.AttrXORMappedAddress)
	if err != nil || mapped.String() != conn.LocalAddr().String() {
		t.Fatalf("mapped %v, want %v (%v)", mapped, conn.LocalAddr(), err)
	}
}

func TestRunErrors(t *testing.T) {
	if err := run(context.Background(), []string{"-nope"}, io.Discard, nil); err == nil {
		t.Error("accepted an unknown flag")
	}
	if err := run(context.Background(), []string{"-addr", "127.0.0.1:99999"}, io.Discard, nil); err == nil {
		t.Error("started on a port that does not exist")
	}
}
