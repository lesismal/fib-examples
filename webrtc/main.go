//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command webrtc is a WebRTC server a browser talks to: a data channel that
// echoes what the page sends, and a camera loopback that sends the page's
// video back to it, RTP packet by RTP packet.
//
//	go run ./webrtc
//
// and open http://localhost:8080 in a browser. One fib engine does it all:
// the page and the signaling over HTTP on TCP port 8080, and every
// PeerConnection on one shared UDP port, a webrtc.Mux on UDP port 8080.
//
// The page POSTs its SDP offer to /whip, as WHIP (RFC 9725) does, and the
// server answers with an ICE-lite PeerConnection on the Mux: the reply is
// 201 Created with the SDP answer and a Location, the session, which a
// DELETE closes. The server answers with its host candidates, the
// addresses of the machine's interfaces; -public-ip advertises others, for
// a server behind a one-to-one NAT or in the cloud.
//
// -turn :3478 also runs a TURN server on the engine, and /ice hands the page
// credentials for it that expire in an hour (the TURN REST API scheme), so
// that the page can be told to relay everything through it.
//
// Browsers give the camera only to a page from a secure origin: localhost,
// or HTTPS (-tls, with a self-signed certificate unless -cert and -key name
// one). Without it, or without a camera, the page runs the data channel
// alone.
package main

import (
	"context"
	"crypto/rand"
	stdtls "crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib-examples/webrtc/whip"
	fibhttp "github.com/lesismal/fib/http"
	fibtls "github.com/lesismal/fib/tls"
	"github.com/lesismal/fib/webrtc"
	"github.com/lesismal/fib/webrtc/ice"
	"github.com/lesismal/fib/webrtc/turn"
	"github.com/lesismal/fib/webrtc/turn/turnserver"
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
	flags := flag.NewFlagSet("webrtc", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", ":8080", "HTTP listen address (TCP)")
	udp := flags.String("udp", "", "WebRTC listen address (UDP), shared by every PeerConnection; the -addr one when empty")
	publicIPs := flags.String("public-ip", "", "comma-separated IPs to advertise as host candidates instead of the interfaces' (MuxConfig.PublicAddrs)")
	turnAddr := flags.String("turn", "", "also run a TURN server at this UDP address, \":3478\" say, with ephemeral credentials")
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

	s := &server{}
	router := fibhttp.NewRouter()
	router.Get("/", func(c *fibhttp.Context) { _ = c.Respond(200, "text/html; charset=utf-8", indexHTML) })
	router.Get("/ice", s.iceServers)
	s.whip = &whip.Endpoint{Path: "/whip", MaxSessions: 100, NewPeerConnection: s.newPeerConnection}
	s.whip.Register(router)

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
	if err := s.listen(engine, *udp, *turnAddr, public); err != nil {
		_ = engine.Close()
		return err
	}

	stop := example.Start(engine)
	httpAddr := example.Addr(engine)
	fmt.Fprintf(out, "WebRTC example on %s://%s, PeerConnections on UDP %v\n", scheme, browserAddr(httpAddr), s.mux.Addrs())
	if s.turn != nil {
		fmt.Fprintf(out, "TURN server on UDP %s, relaying from %s\n", s.turnURL, s.relayIP)
	}
	if ready != nil {
		ready(httpAddr)
	}
	<-ctx.Done()
	s.whip.Close()
	if s.turn != nil {
		s.turn.Close()
	}
	return stop()
}

// listen adds the WebRTC Mux, and the TURN server if turnAddr is set, to
// engine.
func (s *server) listen(engine *fib.Engine, udp, turnAddr string, public []netip.Addr) error {
	// The Mux is the one UDP port every PeerConnection shares, a binding it
	// adds to the engine, as the TURN server does.
	var err error
	s.mux, err = webrtc.NewMux(engine, webrtc.MuxConfig{Addr: udp, PublicAddrs: public})
	if err != nil {
		return err
	}
	// One DTLS certificate serves every PeerConnection: browsers check it
	// against the fingerprint in the answer, not against a CA.
	cert, err := fibtls.GenerateWebRTCCertificate()
	if err != nil {
		return err
	}
	s.certs = []stdtls.Certificate{cert}
	if turnAddr != "" {
		return s.startTURN(engine, turnAddr, udp, public)
	}
	return nil
}

type server struct {
	mux   *webrtc.Mux
	certs []stdtls.Certificate
	whip  *whip.Endpoint

	// The TURN server, when -turn runs one, and the secret its ephemeral
	// credentials are made with.
	turn       *turnserver.Server
	turnSecret []byte
	turnURL    string
	relayIP    netip.Addr
}

// vp8 is the one video codec the server negotiates, so that the page's video
// can go back to it as it came: the loopback forwards RTP and never decodes
// it, and every browser sends VP8.
var vp8 = codec("VP8")

// codec is the codec of DefaultCodecs named name.
func codec(name string) webrtc.Codec {
	for _, c := range webrtc.DefaultCodecs() {
		if strings.EqualFold(c.Name(), name) {
			return c
		}
	}
	panic("no codec " + name)
}

// newPeerConnection makes the PeerConnection that answers a page's offer:
// data channels the page opens echo, and its video, if it sends any, comes
// back to it.
func (s *server) newPeerConnection(offer string) (*webrtc.PeerConnection, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		Mux: s.mux,
		// A server with a reachable address is ICE lite: it answers the
		// browser's checks rather than making its own.
		ICELite:      true,
		Certificates: s.certs,
		Codecs:       []webrtc.Codec{vp8},
	})
	if err != nil {
		return nil, err
	}

	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				_ = d.SendText(string(m.Data))
			} else {
				_ = d.Send(m.Data)
			}
		})
	})

	if strings.Contains(offer, "m=video") {
		if err := loopback(pc); err != nil {
			_ = pc.Close()
			return nil, err
		}
	}
	return pc, nil
}

// loopback sends the video the peer sends back to it. The track is added
// before the offer is set, so that the answer takes the offer's video
// section as sendrecv.
func loopback(pc *webrtc.PeerConnection) error {
	track := webrtc.NewTrackLocal(vp8, "loopback", "fib")
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	var remote atomic.Pointer[webrtc.TrackRemote]
	// The browser asks for a key frame when it starts decoding and after
	// a loss: only the page's encoder can make one, so the request goes
	// back to it.
	sender.OnKeyFrameRequest(func() {
		if r := remote.Load(); r != nil {
			_ = r.RequestKeyFrame()
		}
	})
	pc.OnTrack(func(r *webrtc.TrackRemote) {
		if r.Kind() != webrtc.MediaKindVideo {
			return
		}
		// The packets carry the header extensions under the ids the
		// page negotiated; the track rewrites them to the ids its own
		// PeerConnection did, the same ones here.
		track.SetHeaderExtensions(r.HeaderExtensions())
		remote.Store(r)
		r.OnRTP(func(pkt []byte) { _ = track.WriteRTP(pkt) })
		_ = r.RequestKeyFrame()
	})
	return nil
}

// iceServers is GET /ice: the RTCIceServers the page configures, the TURN
// server with fresh credentials when there is one, and none otherwise,
// since the server needs no STUN to be reached.
func (s *server) iceServers(c *fibhttp.Context) {
	type iceServer struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username,omitempty"`
		Credential string   `json:"credential,omitempty"`
	}
	servers := []iceServer{}
	if s.turn != nil {
		user, pass := turn.EphemeralCredentials(s.turnSecret, "page", time.Now().Add(time.Hour))
		servers = append(servers, iceServer{URLs: []string{s.turnURL}, Username: user, Credential: pass})
	}
	body, _ := json.Marshal(map[string]any{"iceServers": servers})
	_ = c.Respond(200, "application/json", body)
}

// startTURN runs a TURN server at addr on engine. It relays from relayIP:
// the -udp address, if it names one, or else the first address of the
// machine's interfaces, and it is reached at -public-ip when there is one.
func (s *server) startTURN(engine *fib.Engine, addr, udp string, public []netip.Addr) error {
	host, _, err := net.SplitHostPort(udp)
	if err != nil {
		return err
	}
	if ip, err := netip.ParseAddr(host); err == nil && !ip.IsUnspecified() {
		s.relayIP = ip
	} else {
		addrs, err := ice.HostAddrs()
		if err != nil || len(addrs) == 0 {
			return errors.New("-turn: no interface address to relay from")
		}
		// IPv4 when there is one, which every network carries.
		s.relayIP = addrs[0]
		for _, a := range addrs {
			if a.Is4() {
				s.relayIP = a
				break
			}
		}
	}

	s.turnSecret = make([]byte, 32)
	_, _ = rand.Read(s.turnSecret)
	cfg := turnserver.Config{RelayIP: s.relayIP}
	cfg.Realm = "fib-examples"
	cfg.Auth = turn.EphemeralAuth(s.turnSecret, time.Now)
	cfg.MaxAllocations = 100
	// A TURN server refuses to relay to loopback and private addresses,
	// or anyone with credentials could reach into its network through it.
	// The one peer the page has here is this server's Mux, so its
	// addresses, and those alone, are let through.
	for _, a := range s.mux.Addrs() {
		cfg.AllowedPeers = append(cfg.AllowedPeers, netip.PrefixFrom(a.Addr(), a.Addr().BitLen()))
	}
	cfg.AllowedPeers = append(cfg.AllowedPeers, netip.PrefixFrom(s.relayIP, s.relayIP.BitLen()))
	reachedAt := s.relayIP
	if len(public) > 0 {
		cfg.PublicIP = public[0]
		reachedAt = public[0]
	}

	ts, err := turnserver.New(cfg)
	if err != nil {
		return err
	}
	i, err := engine.AddBinding(fib.Binding{Network: "udp", Addr: addr, Handler: ts})
	if err != nil {
		return err
	}
	ts.Start(engine, i)
	addrs, err := engine.BindingAddrs(i)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("-turn: %v", err)
	}
	port := addrs[0].(*net.UDPAddr).Port
	s.turn = ts
	s.turnURL = "turn:" + net.JoinHostPort(reachedAt.String(), strconv.Itoa(port))
	return nil
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
