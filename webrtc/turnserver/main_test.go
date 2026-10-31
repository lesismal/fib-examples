//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/webrtc"
)

const waitTimeout = 20 * time.Second

// syncBuffer is the server's output, written on its goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type rtcIceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// start runs the server on loopback with args added, relaying to loopback,
// and returns the RTCIceServer it printed.
func start(t *testing.T, args ...string) rtcIceServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	out := &syncBuffer{}
	args = append([]string{"-addr", "127.0.0.1:0", "-relay-ip", "127.0.0.1", "-allow", "127.0.0.0/8"}, args...)
	go func() { done <- run(ctx, args, out, func(a string) { addrs <- a }) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-addrs:
	case err := <-done:
		t.Fatalf("did not start: %v", err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		var s rtcIceServer
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &s) == nil {
			return s
		}
	}
	t.Fatalf("no RTCIceServer in %q", out.String())
	return rtcIceServer{}
}

// relayed connects two PeerConnections that gather relay candidates only,
// through s, and echoes a message between them.
func relayed(t *testing.T, s rtcIceServer) {
	t.Helper()
	engine, stop := example.ClientEngine()
	defer stop()
	cfg := webrtc.Configuration{
		Engine: engine, ListenAddr: "127.0.0.1:0",
		ICEServers:         []webrtc.ICEServer{{URLs: s.URLs, Username: s.Username, Credential: s.Credential}},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}
	a, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnMessage(func(m webrtc.DataChannelMessage) { _ = d.Send(m.Data) })
	})
	d, _ := a.CreateDataChannel("x", nil)
	got := make(chan string, 1)
	d.OnOpen(func() { _ = d.SendText("through the relay") })
	d.OnMessage(func(m webrtc.DataChannelMessage) { got <- string(m.Data) })

	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	offer, err := webrtc.Offer(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := webrtc.Answer(ctx, b, offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != "through the relay" {
			t.Fatalf("echo %q", m)
		}
	case <-time.After(waitTimeout):
		t.Fatal("no echo through the relay")
	}
	if local, remote, ok := a.SelectedCandidatePair(); !ok || !strings.Contains(local, "typ relay") || !strings.Contains(remote, "typ relay") {
		t.Fatalf("selected %q -> %q", local, remote)
	}
}

func TestEphemeralCredentials(t *testing.T) {
	s := start(t, "-secret", "test secret")
	if !strings.HasPrefix(s.URLs[0], "turn:127.0.0.1:") || !strings.HasSuffix(s.Username, ":browser") {
		t.Fatalf("%+v", s)
	}
	relayed(t, s)
}

func TestStaticUsers(t *testing.T) {
	s := start(t, "-users", "bob=pw2,alice=pw1", "-ports", "40000-40100")
	if s.Username != "alice" || s.Credential != "pw1" {
		t.Fatalf("%+v", s)
	}
	relayed(t, s)
}

func TestRunErrors(t *testing.T) {
	for _, args := range [][]string{
		{"-nope"},
		{"-addr", "127.0.0.1:0"}, // no credentials
		{"-users", "nopassword"},
		{"-users", "a=b", "-relay-ip", "x"},
		{"-users", "a=b", "-public-ip", "x"},
		{"-users", "a=b", "-ports", "9-1"},
		{"-users", "a=b", "-allow", "10.0.0.0"},
		{"-users", "a=b", "-relay-ip", "0.0.0.0"},
		{"-users", "a=b", "-relay-ip", "127.0.0.1", "-addr", "127.0.0.1:99999"},
	} {
		if err := run(context.Background(), args, io.Discard, nil); err == nil {
			t.Errorf("%q: started", args)
		}
	}
}
