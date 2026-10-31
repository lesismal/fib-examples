//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

package main

import (
	"bytes"
	"context"
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

func startServer(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	var out bytes.Buffer
	args := []string{"-addr", "127.0.0.1:0", "-udp", "127.0.0.1:0"}
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

// peer is a browser, played by a fib PeerConnection on an engine of its own.
func peer(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	engine, stop := example.ClientEngine()
	t.Cleanup(stop)
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{Engine: engine, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// offer POSTs pc's offer to endpoint and sets the answer, or returns the
// status it was refused with.
func offer(t *testing.T, endpoint string, pc *webrtc.PeerConnection) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	o, err := webrtc.Offer(ctx, pc, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(endpoint, "application/sdp", strings.NewReader(o.SDP))
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return resp.StatusCode
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatalf("%v\n%s", err, answer)
	}
	return resp.StatusCode
}

func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// One publisher's video and audio reach every viewer, a viewer's key frame
// request reaches the publisher, and a second publisher is refused.
func TestPublishToViewers(t *testing.T) {
	base := startServer(t)

	pub := peer(t)
	video := webrtc.NewTrackLocal(vp8, "cam", "pub")
	audio := webrtc.NewTrackLocal(opus, "mic", "pub")
	sender, err := pub.AddTrack(video)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.AddTrack(audio); err != nil {
		t.Fatal(err)
	}
	keyFrame := make(chan struct{})
	var once sync.Once
	sender.OnKeyFrameRequest(func() { once.Do(func() { close(keyFrame) }) })
	if status := offer(t, base+"/whip", pub); status != http.StatusCreated {
		t.Fatalf("publish: %d", status)
	}

	type viewer struct {
		pc           *webrtc.PeerConnection
		video, audio chan struct{}
		videoTrack   chan *webrtc.TrackRemote
	}
	viewers := make([]*viewer, 3)
	for i := range viewers {
		v := &viewer{pc: peer(t), video: make(chan struct{}), audio: make(chan struct{}), videoTrack: make(chan *webrtc.TrackRemote, 1)}
		for _, kind := range []webrtc.MediaKind{webrtc.MediaKindVideo, webrtc.MediaKindAudio} {
			if _, err := v.pc.AddTransceiver(kind, webrtc.DirectionRecvOnly); err != nil {
				t.Fatal(err)
			}
		}
		var vOnce, aOnce sync.Once
		v.pc.OnTrack(func(r *webrtc.TrackRemote) {
			if r.Kind() == webrtc.MediaKindVideo {
				v.videoTrack <- r
			}
			r.OnRTP(func(pkt []byte) {
				var p rtp.Packet
				if p.Unmarshal(pkt) != nil {
					return
				}
				switch string(p.Payload) {
				case "video":
					vOnce.Do(func() { close(v.video) })
				case "audio":
					aOnce.Do(func() { close(v.audio) })
				}
			})
		})
		if status := offer(t, base+"/whep", v.pc); status != http.StatusCreated {
			t.Fatalf("watch: %d", status)
		}
		viewers[i] = v
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for seq := uint16(1); ; seq++ {
			_ = video.WritePacket(&rtp.Packet{Header: rtp.Header{PayloadType: vp8.PayloadType, SequenceNumber: seq, Timestamp: uint32(seq) * 3000}, Payload: []byte("video")})
			_ = audio.WritePacket(&rtp.Packet{Header: rtp.Header{PayloadType: opus.PayloadType, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte("audio")})
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	for _, v := range viewers {
		wait(t, v.video, "a viewer's video")
		wait(t, v.audio, "a viewer's audio")
	}
	r := <-viewers[0].videoTrack
	if err := r.RequestKeyFrame(); err != nil {
		t.Fatal(err)
	}
	wait(t, keyFrame, "the viewer's PLI to reach the publisher")

	if status := offer(t, base+"/whip", peer(t)); status != http.StatusServiceUnavailable {
		t.Fatalf("a second publisher: %d", status)
	}
}

func TestRunErrors(t *testing.T) {
	ctx := context.Background()
	for _, args := range [][]string{
		{"-nope"},
		{"-public-ip", "not an ip"},
		{"-addr", "127.0.0.1:99999"},
	} {
		if err := run(ctx, args, io.Discard, nil); err == nil {
			t.Errorf("%q: started", args)
		}
	}
}
