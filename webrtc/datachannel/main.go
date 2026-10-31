//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command datachannel connects two Go PeerConnections, no browser, and
// sends messages over data channels of each kind: reliable and ordered,
// unordered, and partially reliable.
//
//	go run ./webrtc/datachannel
//	go run ./webrtc/datachannel -turn      # everything through a TURN relay
//
// The answering side is a server: an engine serving HTTP signaling with
// fib/http and an ICE-lite PeerConnection per offer on a shared webrtc.Mux,
// the UDP port of the same number. The offering side is a client: a full
// ICE agent on a port of its own, which POSTs its offer as WHIP does,
// created with webrtc.Offer and answered with webrtc.Answer. The server
// echoes every message on the channel it came on.
//
// -turn also runs a TURN server on the server's engine and has the client
// gather relay candidates only, so that every packet between the two goes
// through the relay, as when the client sits behind a firewall that lets
// nothing else out.
package main

import (
	"context"
	stdtls "crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib-examples/webrtc/whip"
	fibhttp "github.com/lesismal/fib/http"
	fibtls "github.com/lesismal/fib/tls"
	"github.com/lesismal/fib/webrtc"
	"github.com/lesismal/fib/webrtc/turn"
	"github.com/lesismal/fib/webrtc/turn/turnserver"
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

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("datachannel", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address: HTTP on TCP, the Mux on UDP")
	n := flags.Int("n", 10, "messages sent on each channel")
	useTURN := flags.Bool("turn", false, "relay everything through a TURN server on the server's engine")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	srv, err := startServer(*addr, *useTURN)
	if err != nil {
		return err
	}
	defer srv.stop()
	fmt.Fprintf(out, "server: signaling on http://%s, PeerConnections on UDP %v\n", srv.httpAddr, srv.mux.Addrs())

	engine, stopClient := example.ClientEngine()
	defer stopClient()
	config := webrtc.Configuration{Engine: engine, ListenAddr: "127.0.0.1:0"}
	if *useTURN {
		// The credentials a web service would hand out, valid for a minute.
		user, pass := turn.EphemeralCredentials(srv.turnSecret, "client", time.Now().Add(time.Minute))
		config.ICEServers = []webrtc.ICEServer{{URLs: []string{srv.turnURL}, Username: user, Credential: pass}}
		config.ICETransportPolicy = webrtc.ICETransportPolicyRelay
		fmt.Fprintf(out, "client: relay candidates only, through %s\n", srv.turnURL)
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return err
	}
	defer pc.Close()

	zero := uint16(0)
	channels := []struct {
		label string
		init  *webrtc.DataChannelInit
	}{
		{"ordered", nil}, // reliable and ordered, as a TCP stream is
		{"unordered", &webrtc.DataChannelInit{Unordered: true}},
		// Sent once and never retransmitted: lost messages stay lost, as
		// UDP datagrams do. On loopback none are.
		{"partial", &webrtc.DataChannelInit{Unordered: true, MaxRetransmits: &zero}},
	}
	results := make(chan string, len(channels))
	for _, c := range channels {
		d, err := pc.CreateDataChannel(c.label, c.init)
		if err != nil {
			return err
		}
		exchange(d, *n, results)
	}

	answer, err := postOffer(ctx, pc, "http://"+srv.httpAddr+"/whip")
	if err != nil {
		return err
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		return err
	}
	for range channels {
		select {
		case r := <-results:
			fmt.Fprintln(out, r)
		case <-ctx.Done():
			return fmt.Errorf("waiting for the echoes: %w", ctx.Err())
		}
	}
	local, remote, _ := pc.SelectedCandidatePair()
	fmt.Fprintf(out, "client: selected pair %s -> %s\n", candidateAddr(local), candidateAddr(remote))
	return nil
}

// exchange sends n messages on d once it opens, and reports on results how
// many came back and whether in order.
func exchange(d *webrtc.DataChannel, n int, results chan<- string) {
	var got []int
	d.OnOpen(func() {
		for i := range n {
			_ = d.SendText(strconv.Itoa(i))
		}
	})
	d.OnMessage(func(m webrtc.DataChannelMessage) {
		i, _ := strconv.Atoi(string(m.Data))
		got = append(got, i)
		if len(got) < n {
			return
		}
		order := "in order"
		for k := range got {
			if got[k] != k {
				order = "out of order"
			}
		}
		results <- fmt.Sprintf("client: %-9s %d/%d echoed, %s", d.Label(), len(got), n, order)
	})
}

// postOffer POSTs pc's offer, with every candidate gathered, to url, and
// returns the answer.
func postOffer(ctx context.Context, pc *webrtc.PeerConnection, url string) (webrtc.SessionDescription, error) {
	offer, err := webrtc.Offer(ctx, pc, nil)
	if err != nil {
		return offer, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(offer.SDP))
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return offer, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return offer, err
	}
	if resp.StatusCode != http.StatusCreated {
		return offer, fmt.Errorf("POST %s: %s: %s", url, resp.Status, body)
	}
	return webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}, nil
}

// candidateAddr is "address port type" out of an a=candidate value.
func candidateAddr(c string) string {
	f := strings.Fields(c)
	if len(f) < 8 {
		return c
	}
	return net.JoinHostPort(f[4], f[5]) + " " + f[7]
}

// server is the answering side.
type server struct {
	engine   *fib.Engine
	stopFn   func() error
	mux      *webrtc.Mux
	whip     *whip.Endpoint
	httpAddr string

	turn       *turnserver.Server
	turnURL    string
	turnSecret []byte
}

func startServer(addr string, useTURN bool) (*server, error) {
	s := &server{}
	cert, err := fibtls.GenerateWebRTCCertificate()
	if err != nil {
		return nil, err
	}
	s.whip = &whip.Endpoint{Path: "/whip", MaxSessions: 10, NewPeerConnection: func(string) (*webrtc.PeerConnection, error) {
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{Mux: s.mux, ICELite: true, Certificates: []stdtls.Certificate{cert}})
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
		return pc, nil
	}}
	router := fibhttp.NewRouter()
	s.whip.Register(router)

	config := example.EngineConfig()
	s.engine, err = fib.New(config, fib.Binding{Addr: addr, Handler: fibhttp.NewHandler(router)})
	if err != nil {
		return nil, err
	}
	tcp, err := s.engine.LocalAddr()
	if err != nil {
		_ = s.engine.Close()
		return nil, err
	}
	s.httpAddr = tcp.String()
	// The Mux takes the UDP port of the HTTP port's number.
	if s.mux, err = webrtc.NewMux(s.engine, webrtc.MuxConfig{Addr: s.httpAddr}); err != nil {
		_ = s.engine.Close()
		return nil, err
	}
	if useTURN {
		if err := s.startTURN(); err != nil {
			_ = s.engine.Close()
			return nil, err
		}
	}
	s.stopFn = example.Start(s.engine)
	return s, nil
}

// startTURN runs a TURN server on the server's engine, on a port the
// kernel picks, relaying from the Mux's address and to it alone: a TURN
// server relays to no loopback or private address unless told to.
func (s *server) startTURN() error {
	host, _, _ := net.SplitHostPort(s.httpAddr)
	relayIP, err := netip.ParseAddr(host)
	if err != nil || relayIP.IsUnspecified() {
		return errors.New("-turn needs a specific -addr to relay from")
	}
	s.turnSecret = []byte(strconv.FormatInt(time.Now().UnixNano(), 36))
	cfg := turnserver.Config{RelayIP: relayIP}
	cfg.Realm = "fib-examples"
	cfg.Auth = turn.EphemeralAuth(s.turnSecret, time.Now)
	for _, a := range s.mux.Addrs() {
		cfg.AllowedPeers = append(cfg.AllowedPeers, netip.PrefixFrom(a.Addr(), a.Addr().BitLen()))
	}
	if s.turn, err = turnserver.New(cfg); err != nil {
		return err
	}
	i, err := s.engine.AddBinding(fib.Binding{Network: "udp", Addr: net.JoinHostPort(host, "0"), Handler: s.turn})
	if err != nil {
		return err
	}
	s.turn.Start(s.engine, i)
	addrs, err := s.engine.BindingAddrs(i)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("TURN binding: %v", err)
	}
	s.turnURL = "turn:" + addrs[0].String()
	return nil
}

func (s *server) stop() {
	s.whip.Close()
	if s.turn != nil {
		s.turn.Close()
	}
	_ = s.stopFn()
}
