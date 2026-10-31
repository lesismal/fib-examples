//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command media sends RTP from one Go PeerConnection to another, no browser
// and no codec: synthetic VP8 and Opus packets written to TrackLocals,
// received through OnTrack, counted per stream, and a key frame asked for
// with a PLI that the sender sees in OnKeyFrameRequest.
//
//	go run ./webrtc/media
//	go run ./webrtc/media -duration 10s
//
// A PeerConnection carries RTP and never looks inside a payload, which is
// what a forwarding server needs: each one rewrites the SSRC, payload type
// and header extension ids to those it negotiated, protects the packet with
// SRTP, and sends sender and receiver reports. Here the payloads are just a
// counter, where an application would put encoded frames.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/webrtc"
	"github.com/lesismal/fib/webrtc/rtp"
)

// fatal reports an error and exits; a test replaces it.
var fatal = example.Fatal

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fatal(err)
	}
}

// codec is the codec of DefaultCodecs named name.
func codec(name string) webrtc.Codec {
	for _, c := range webrtc.DefaultCodecs() {
		if c.Name() == name {
			return c
		}
	}
	panic("no codec " + name)
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// stream counts what one TrackRemote received.
type stream struct {
	kind        webrtc.MediaKind
	ssrc        uint32
	pt          uint8
	codec       string
	packets     int
	first, last uint16
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("media", flag.ContinueOnError)
	flags.SetOutput(out)
	duration := flags.Duration("duration", 3*time.Second, "how long to send")
	if err := flags.Parse(args); err != nil {
		return err
	}
	// The callbacks of the two PeerConnections print from goroutines of
	// their own.
	out = &lockedWriter{w: out}

	engine, stop := example.ClientEngine()
	defer stop()
	newPC := func() (*webrtc.PeerConnection, error) {
		return webrtc.NewPeerConnection(webrtc.Configuration{Engine: engine, ListenAddr: "127.0.0.1:0"})
	}
	sender, err := newPC()
	if err != nil {
		return err
	}
	defer sender.Close()
	receiver, err := newPC()
	if err != nil {
		return err
	}
	defer receiver.Close()

	// The sender: one track of each kind. The payload types written here
	// are the codec's own; each PeerConnection rewrites them to what it
	// negotiated.
	vp8, opus := codec("VP8"), codec("opus")
	video := webrtc.NewTrackLocal(vp8, "camera", "fib")
	audio := webrtc.NewTrackLocal(opus, "microphone", "fib")
	videoSender, err := sender.AddTrack(video)
	if err != nil {
		return err
	}
	if _, err := sender.AddTrack(audio); err != nil {
		return err
	}
	var keyFrames atomic.Int32
	videoSender.OnKeyFrameRequest(func() {
		keyFrames.Add(1)
		fmt.Fprintln(out, "sender:   the receiver asks for a key frame (PLI)")
	})
	var reports atomic.Int32
	sender.OnRTCP(func(pkt []byte) {
		for len(pkt) > 0 {
			h, _, rest, err := rtp.NextRTCP(pkt)
			if err != nil {
				return
			}
			if h.Type == rtp.TypeRR {
				reports.Add(1)
			}
			pkt = rest
		}
	})

	// The receiver: each stream is reported with its first packet.
	var mu sync.Mutex
	streams := map[uint32]*stream{}
	pli := make(chan *webrtc.TrackRemote, 1)
	receiver.OnTrack(func(r *webrtc.TrackRemote) {
		fmt.Fprintf(out, "receiver: %s track %q, SSRC %d, %s at payload type %d\n", r.Kind(), r.ID(), r.SSRC(), r.Codec().MimeType, r.Codec().PayloadType)
		s := &stream{kind: r.Kind(), ssrc: r.SSRC(), codec: r.Codec().MimeType}
		mu.Lock()
		streams[r.SSRC()] = s
		mu.Unlock()
		var p rtp.Packet
		r.OnRTP(func(pkt []byte) {
			if p.Unmarshal(pkt) != nil {
				return
			}
			mu.Lock()
			if s.packets == 0 {
				s.first = p.SequenceNumber
			}
			s.packets++
			s.pt, s.last = p.PayloadType, p.SequenceNumber
			mu.Unlock()
		})
		if r.Kind() == webrtc.MediaKindVideo {
			pli <- r
		}
	})
	connected := make(chan struct{})
	var once sync.Once
	sender.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})

	// Offer and answer, passed straight across: both are in this process.
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	offer, err := webrtc.Offer(wait, sender, nil)
	if err != nil {
		return err
	}
	answer, err := webrtc.Answer(wait, receiver, offer)
	if err != nil {
		return err
	}
	if err := sender.SetRemoteDescription(answer); err != nil {
		return err
	}
	select {
	case <-connected:
	case <-wait.Done():
		return fmt.Errorf("not connected: %w", wait.Err())
	}
	fmt.Fprintln(out, "sender:   connected, sending 30 video and 50 audio packets a second")

	sent := send(ctx, video, audio, vp8, opus, *duration, pli)
	time.Sleep(200 * time.Millisecond) // the last packets in flight

	mu.Lock()
	defer mu.Unlock()
	ssrcs := make([]uint32, 0, len(streams))
	for ssrc := range streams {
		ssrcs = append(ssrcs, ssrc)
	}
	sort.Slice(ssrcs, func(i, j int) bool { return streams[ssrcs[i]].kind > streams[ssrcs[j]].kind })
	for _, ssrc := range ssrcs {
		s := streams[ssrc]
		fmt.Fprintf(out, "receiver: %s SSRC %d, %s PT %d: %d of %d packets, seq %d..%d\n", s.kind, s.ssrc, s.codec, s.pt, s.packets, sent[s.kind], s.first, s.last)
	}
	fmt.Fprintf(out, "sender:   %d key frame requests, %d receiver reports\n", keyFrames.Load(), reports.Load())
	if keyFrames.Load() == 0 || len(streams) != 2 {
		return fmt.Errorf("%d streams received, %d key frame requests", len(streams), keyFrames.Load())
	}
	return nil
}

// send writes synthetic packets to the tracks for d: video at 30 and audio
// at 50 packets a second, with the timestamps of their clock rates. A
// second in, the receiver's video track is asked for a key frame. It
// returns how many packets of each kind it sent.
func send(ctx context.Context, video, audio *webrtc.TrackLocal, vp8, opus webrtc.Codec, d time.Duration, pli <-chan *webrtc.TrackRemote) map[webrtc.MediaKind]int {
	sent := map[webrtc.MediaKind]int{}
	vt, at := time.NewTicker(time.Second/30), time.NewTicker(20*time.Millisecond)
	defer vt.Stop()
	defer at.Stop()
	askAt := time.After(time.Second)
	end := time.After(d)
	v := rtp.Packet{Header: rtp.Header{PayloadType: vp8.PayloadType, SSRC: 1, Marker: true}}
	a := rtp.Packet{Header: rtp.Header{PayloadType: opus.PayloadType, SSRC: 2}}
	var payload [8]byte
	for {
		select {
		case <-ctx.Done():
			return sent
		case <-end:
			return sent
		case <-askAt:
			select {
			case r := <-pli:
				_ = r.RequestKeyFrame() // a PLI, as a decoder that lost its reference sends
			default:
			}
		case <-vt.C:
			v.SequenceNumber++
			v.Timestamp += 90000 / 30
			binary.BigEndian.PutUint64(payload[:], uint64(v.SequenceNumber))
			v.Payload = payload[:]
			if video.WritePacket(&v) == nil {
				sent[webrtc.MediaKindVideo]++
			}
		case <-at.C:
			a.SequenceNumber++
			a.Timestamp += 48000 / 50
			binary.BigEndian.PutUint64(payload[:], uint64(a.SequenceNumber))
			a.Payload = payload[:]
			if audio.WritePacket(&a) == nil {
				sent[webrtc.MediaKindAudio]++
			}
		}
	}
}
