//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/webrtc"
	"github.com/lesismal/fib/webrtc/rtp"
)

const waitTimeout = 20 * time.Second

// startServer runs the example on loopback with args added and returns its
// base URL.
func startServer(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	var out bytes.Buffer
	args = append([]string{"-addr", "127.0.0.1:0", "-udp", "127.0.0.1:0"}, args...)
	go func() { done <- run(ctx, args, &out, func(addr string) { addrs <- addr }) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case addr := <-addrs:
		return "http://" + addr
	case err := <-done:
		t.Fatalf("did not start: %v", err)
		return ""
	}
}

// browser is the page's side, played by a fib PeerConnection on an engine
// of its own.
func browser(t *testing.T, config webrtc.Configuration) *webrtc.PeerConnection {
	t.Helper()
	engine, stop := example.ClientEngine()
	t.Cleanup(stop)
	config.Engine = engine
	config.ListenAddr = "127.0.0.1:0"
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// postOffer POSTs pc's offer to the server and sets its answer, returning the
// session's URL.
func postOffer(t *testing.T, base string, pc *webrtc.PeerConnection) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	offer, err := webrtc.Offer(ctx, pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(base+"/whip", "application/sdp", strings.NewReader(offer.SDP))
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Content-Type") != "application/sdp" {
		t.Fatalf("POST /whip: %d %v %s", resp.StatusCode, resp.Header, answer)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "/whip/") {
		t.Fatalf("Location %q", location)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatalf("%v\n%s", err, answer)
	}
	return base + location
}

func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// echo opens a data channel on pc and returns what each message sent on it
// comes back as.
func echo(t *testing.T, pc *webrtc.PeerConnection) (open <-chan struct{}, send func(string), got <-chan string) {
	t.Helper()
	d, err := pc.CreateDataChannel("echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	opened, received := make(chan struct{}), make(chan string, 4)
	d.OnOpen(func() { close(opened) })
	d.OnMessage(func(m webrtc.DataChannelMessage) { received <- string(m.Data) })
	return opened, func(s string) {
		if err := d.SendText(s); err != nil {
			t.Error(err)
		}
	}, received
}

func closedCh(pc *webrtc.PeerConnection) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateFailed {
			once.Do(func() { close(ch) })
		}
	})
	return ch
}

// The server serves the page, echoes on a data channel, sends video back
// as it came, and closes the session on DELETE.
func TestPageEchoAndLoopback(t *testing.T) {
	base := startServer(t)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(page, []byte("createDataChannel")) {
		t.Fatalf("GET /: %d, %d bytes", resp.StatusCode, len(page))
	}

	pc := browser(t, webrtc.Configuration{})
	open, send, got := echo(t, pc)
	camera := webrtc.NewTrackLocal(vp8, "camera", "page")
	if _, err := pc.AddTrack(camera); err != nil {
		t.Fatal(err)
	}
	back := make(chan []byte, 64)
	pc.OnTrack(func(r *webrtc.TrackRemote) {
		r.OnRTP(func(pkt []byte) {
			var p rtp.Packet
			if p.Unmarshal(pkt) == nil {
				select {
				case back <- append([]byte(nil), p.Payload...):
				default:
				}
			}
		})
	})
	closed := closedCh(pc)
	session := postOffer(t, base, pc)

	wait(t, open, "the data channel to open")
	send("hello fib")
	if s := wait(t, got, "the echo"); s != "hello fib" {
		t.Fatalf("echo %q", s)
	}

	// Fake VP8 packets, sent until one comes back.
	stop := make(chan struct{})
	go func() {
		for seq := uint16(1); ; seq++ {
			p := rtp.Packet{Header: rtp.Header{PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, SSRC: 1}, Payload: []byte(fmt.Sprintf("frame %d", seq))}
			_ = camera.WritePacket(&p)
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	payload := wait(t, back, "video to come back")
	close(stop)
	if !strings.HasPrefix(string(payload), "frame ") {
		t.Fatalf("looped back %q", payload)
	}

	req, _ := http.NewRequest(http.MethodDelete, session, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	wait(t, closed, "the server to close the PeerConnection")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE: %d", resp.StatusCode)
	}
}

// With -turn the page is handed credentials for the TURN server on the same
// engine, and connects through it with relay candidates alone.
func TestTURNRelay(t *testing.T) {
	base := startServer(t, "-turn", "127.0.0.1:0")
	resp, err := http.Get(base + "/ice")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		ICEServers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
	}
	err = json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if err != nil || len(cfg.ICEServers) != 1 || cfg.ICEServers[0].Username == "" {
		t.Fatalf("GET /ice: %v %+v", err, cfg)
	}
	s := cfg.ICEServers[0]
	pc := browser(t, webrtc.Configuration{
		ICEServers:         []webrtc.ICEServer{{URLs: s.URLs, Username: s.Username, Credential: s.Credential}},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	})
	open, send, got := echo(t, pc)
	postOffer(t, base, pc)
	wait(t, open, "the data channel to open through the relay")
	send("relayed")
	if s := wait(t, got, "the echo"); s != "relayed" {
		t.Fatalf("echo %q", s)
	}
	if local, _, ok := pc.SelectedCandidatePair(); !ok || !strings.Contains(local, "typ relay") {
		t.Fatalf("selected local candidate %q", local)
	}
}

// Without TURN the page gets no ICE servers, and offers that are not SDP or
// not offers are refused.
func TestSignalingErrors(t *testing.T) {
	base := startServer(t)
	resp, err := http.Get(base + "/ice")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(body)) != `{"iceServers":[]}` {
		t.Fatalf("GET /ice: %s", body)
	}
	for _, c := range []struct {
		contentType, body string
		status            int
	}{
		{"text/plain", "v=0", http.StatusUnsupportedMediaType},
		{"application/sdp", "not sdp", http.StatusBadRequest},
	} {
		resp, err := http.Post(base+"/whip", c.contentType, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s %q: %d, want %d", c.contentType, c.body, resp.StatusCode, c.status)
		}
	}
}

func TestRunErrors(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{
		{"-nope"},
		{"-public-ip", "not an ip"},
		{"-addr", "127.0.0.1:99999"},
		{"-addr", "127.0.0.1:0", "-udp", "not an address"},
	} {
		if err := run(ctx, args, io.Discard, nil); err == nil {
			t.Errorf("%q: started", args)
		}
	}
}
