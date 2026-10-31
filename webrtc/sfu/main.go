//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command sfu is a selective forwarding unit: one browser publishes its
// camera and microphone, any number of others watch, and the server
// forwards the publisher's RTP packets to each of them without decoding
// anything.
//
//	go run ./webrtc/sfu
//
// and open http://localhost:8080 in two or more tabs: Publish in one,
// Watch in the others. The publisher POSTs its offer to /whip (WHIP, RFC
// 9725) and the viewers to /whep (WHEP), each answered by an ICE-lite
// PeerConnection on one shared webrtc.Mux, UDP port 8080; a DELETE on the
// Location of the answer leaves.
//
// Each kind of media is one webrtc.TrackLocal that every viewer's
// PeerConnection sends, under its own SSRC, payload type and header
// extension ids: the publisher's packets are written to it once, and go to
// all of them. A viewer that needs a key frame, to start or after a loss,
// sends a PLI, and the server asks the publisher for one, the only one
// that can make it.
//
// -public-ip and -tls are as in the webrtc example next to this one.
package main

import (
	"context"
	stdtls "crypto/tls"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib-examples/webrtc/whip"
	fibhttp "github.com/lesismal/fib/http"
	fibtls "github.com/lesismal/fib/tls"
	"github.com/lesismal/fib/webrtc"
)

//go:embed index.html
var indexHTML []byte

// fatal reports a startup error and exits; a test replaces it.
var fatal = example.Fatal

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fatal(err)
	}
}

// run serves until ctx ends. ready, if not nil, is told the HTTP address
// once the server is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("sfu", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", ":8080", "HTTP listen address (TCP)")
	udp := flags.String("udp", "", "WebRTC listen address (UDP), shared by every PeerConnection; the -addr one when empty")
	publicIPs := flags.String("public-ip", "", "comma-separated IPs to advertise as host candidates instead of the interfaces' (MuxConfig.PublicAddrs)")
	maxViewers := flags.Int("max-viewers", 100, "viewers served at once")
	useTLS := flags.Bool("tls", false, "serve the page over HTTPS, which browsers require for the camera beyond localhost")
	certFlags := certs.ServerFlagsOn(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *udp == "" {
		*udp = *addr
	}
	public, err := parseIPs(*publicIPs)
	if err != nil {
		return err
	}
	cert, err := fibtls.GenerateWebRTCCertificate()
	if err != nil {
		return err
	}

	s := &sfu{
		certs: []stdtls.Certificate{cert},
		video: webrtc.NewTrackLocal(vp8, "video", "sfu"),
		audio: webrtc.NewTrackLocal(opus, "audio", "sfu"),
	}
	router := fibhttp.NewRouter()
	router.Get("/", func(c *fibhttp.Context) { _ = c.Respond(200, "text/html; charset=utf-8", indexHTML) })
	publish := &whip.Endpoint{Path: "/whip", MaxSessions: 1, NewPeerConnection: s.publisher, OnStateChange: s.publisherState}
	watch := &whip.Endpoint{Path: "/whep", MaxSessions: *maxViewers, NewPeerConnection: s.viewer}
	publish.Register(router)
	watch.Register(router)

	var handler fib.Handler = fibhttp.NewHandler(router)
	scheme := "http"
	if *useTLS {
		tlsConfig, err := certFlags.Config()
		if err != nil {
			return err
		}
		handler = fibtls.NewServer(fibhttp.ConfigureTLS(tlsConfig), handler)
		scheme = "https"
	}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: handler})
	if err != nil {
		return err
	}
	// Publisher and viewers share one UDP port.
	if s.mux, err = webrtc.NewMux(engine, webrtc.MuxConfig{Addr: *udp, PublicAddrs: public}); err != nil {
		_ = engine.Close()
		return err
	}

	stop := example.Start(engine)
	httpAddr := example.Addr(engine)
	fmt.Fprintf(out, "SFU on %s://%s, PeerConnections on UDP %v\n", scheme, browserAddr(httpAddr), s.mux.Addrs())
	if ready != nil {
		ready(httpAddr)
	}
	<-ctx.Done()
	watch.Close()
	publish.Close()
	return stop()
}

// The codecs the SFU negotiates, one of each kind: every viewer must be able
// to take what the publisher sends as it is, and every browser has these.
var (
	vp8  = codec("VP8")
	opus = codec("opus")
)

// codec is the codec of DefaultCodecs named name.
func codec(name string) webrtc.Codec {
	for _, c := range webrtc.DefaultCodecs() {
		if strings.EqualFold(c.Name(), name) {
			return c
		}
	}
	panic("no codec " + name)
}

type sfu struct {
	mux   *webrtc.Mux
	certs []stdtls.Certificate
	// video and audio are what every viewer is sent; the publisher's
	// packets are written to them.
	video, audio *webrtc.TrackLocal

	mu sync.Mutex
	// publishing is the publisher's PeerConnection and source its video,
	// which key frame requests go to.
	publishing *webrtc.PeerConnection
	source     *webrtc.TrackRemote
}

func (s *sfu) config() webrtc.Configuration {
	return webrtc.Configuration{
		Mux:          s.mux,
		ICELite:      true,
		Certificates: s.certs,
		Codecs:       []webrtc.Codec{vp8, opus},
	}
}

// publisher answers the publisher's offer: what it sends is forwarded to
// the viewers' tracks.
func (s *sfu) publisher(string) (*webrtc.PeerConnection, error) {
	pc, err := webrtc.NewPeerConnection(s.config())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.publishing = pc
	s.mu.Unlock()
	pc.OnTrack(func(r *webrtc.TrackRemote) {
		track := s.audio
		if r.Kind() == webrtc.MediaKindVideo {
			track = s.video
			s.mu.Lock()
			if s.publishing == pc {
				s.source = r
			}
			s.mu.Unlock()
		}
		// The publisher's header extension ids, which each viewer's
		// PeerConnection maps to its own.
		track.SetHeaderExtensions(r.HeaderExtensions())
		r.OnRTP(func(pkt []byte) { _ = track.WriteRTP(pkt) })
		if r.Kind() == webrtc.MediaKindVideo {
			// Viewers already watching need a key frame to start from.
			_ = r.RequestKeyFrame()
		}
	})
	return pc, nil
}

// publisherState forgets the publisher once it has gone, so that another
// may publish.
func (s *sfu) publisherState(pc *webrtc.PeerConnection, state webrtc.PeerConnectionState) {
	if state != webrtc.PeerConnectionStateFailed && state != webrtc.PeerConnectionStateClosed {
		return
	}
	s.mu.Lock()
	if s.publishing == pc {
		s.publishing, s.source = nil, nil
	}
	s.mu.Unlock()
}

// viewer answers a viewer's offer, which receives video and audio, with the
// SFU's tracks.
func (s *sfu) viewer(offer string) (*webrtc.PeerConnection, error) {
	if !strings.Contains(offer, "m=video") && !strings.Contains(offer, "m=audio") {
		return nil, errors.New("a viewer's offer receives video or audio")
	}
	pc, err := webrtc.NewPeerConnection(s.config())
	if err != nil {
		return nil, err
	}
	// Added before the offer is set, the tracks are sent on the media
	// sections the offer has of their kinds.
	for _, track := range []*webrtc.TrackLocal{s.video, s.audio} {
		sender, err := pc.AddTrack(track)
		if err != nil {
			_ = pc.Close()
			return nil, err
		}
		if track == s.video {
			sender.OnKeyFrameRequest(s.requestKeyFrame)
		}
	}
	return pc, nil
}

// requestKeyFrame passes a viewer's PLI or FIR on to the publisher.
func (s *sfu) requestKeyFrame() {
	s.mu.Lock()
	source := s.source
	s.mu.Unlock()
	if source != nil {
		_ = source.RequestKeyFrame()
	}
}

func parseIPs(s string) ([]netip.Addr, error) {
	var ips []netip.Addr
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		ip, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("-public-ip: %w", err)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

// browserAddr is addr as a browser on this machine opens it: localhost for
// a wildcard address, which is a secure origin for the camera.
func browserAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}
