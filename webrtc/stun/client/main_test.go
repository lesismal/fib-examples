//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lesismal/fib/webrtc/stun"
)

// stunServer answers Binding requests on loopback, dropping the first one
// so that the client has to retransmit.
func stunServer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		b := make([]byte, 1500)
		for seen := 0; ; seen++ {
			n, from, err := conn.ReadFromUDPAddrPort(b)
			if err != nil {
				return
			}
			var req stun.Message
			if seen == 0 || req.Decode(b[:n]) != nil || req.Type != stun.BindingRequest {
				continue
			}
			var resp stun.Message
			resp.Build(stun.BindingSuccess, req.TransactionID)
			resp.AddXORAddress(stun.AttrXORMappedAddress, from)
			_, _ = conn.WriteToUDPAddrPort(resp.Raw, from)
		}
	}()
	return conn.LocalAddr().String()
}

func TestRunPrintsTheMappedAddress(t *testing.T) {
	server := stunServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, []string{"-server", server, "-addr", "127.0.0.1:0"}, &out); err != nil {
		t.Fatal(err)
	}
	// On loopback the server sees the socket's own address.
	if !strings.Contains(out.String(), "server-reflexive 127.0.0.1:") || !strings.Contains(out.String(), "as "+server) {
		t.Fatalf("output %q", out.String())
	}
}

func TestRunErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"-nope"},
		{"-server", "no port"},
		{"-addr", "127.0.0.1:99999"},
		// Nobody answers: the context ends the wait.
		{"-server", "127.0.0.1:9", "-addr", "127.0.0.1:0"},
	} {
		if err := run(ctx, args, io.Discard); err == nil {
			t.Errorf("%q: succeeded", args)
		}
	}
}
