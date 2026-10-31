//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command turnserver is a standalone TURN server (RFC 8656, UDP) on a fib
// engine, for browsers and other WebRTC peers that cannot reach each other
// directly.
//
//	go run ./webrtc/turnserver -users alice=secret
//	go run ./webrtc/turnserver -secret "$(openssl rand -hex 32)" -public-ip 203.0.113.7 -ports 49152-65535
//
// It prints an RTCIceServer a page can use. -users takes fixed users;
// -secret accepts the time-limited credentials of the TURN REST API scheme,
// "<expiry>:<user>" with an HMAC of it as the password, which a web service
// sharing the secret hands out (turn.EphemeralCredentials); both may be set.
//
// Every allocation's relayed address is a UDP binding of its own on the
// engine, bound to -relay-ip, on a port in -ports or one the kernel picks.
// A server behind a one-to-one NAT tells clients -public-ip instead.
//
// A TURN server refuses to relay to loopback, private, link-local and
// other internal addresses, so that nobody with credentials can reach into
// the network it runs in. -allow lets prefixes through, the private network
// of your own WebRTC servers say, and nothing else.
package main

import (
	"context"
	"crypto/subtle"
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
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/webrtc/ice"
	"github.com/lesismal/fib/webrtc/turn"
	"github.com/lesismal/fib/webrtc/turn/turnserver"
)

// fatal reports a startup error and exits; a test replaces it.
var fatal = example.Fatal

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fatal(err)
	}
}

// run serves until ctx ends. ready, if not nil, is told the address once the
// server is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("turnserver", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", ":3478", "listen address (UDP)")
	realm := flags.String("realm", "fib-examples", "realm of the long-term credentials")
	users := flags.String("users", "", "fixed users, user=password,...")
	secret := flags.String("secret", "", "secret of ephemeral credentials (TURN REST API)")
	relayIP := flags.String("relay-ip", "", "address relayed ports bind to; the first IPv4 interface address when empty")
	publicIP := flags.String("public-ip", "", "address clients are told, for a server behind a one-to-one NAT")
	ports := flags.String("ports", "", "relayed port range, min-max; ports the kernel picks when empty")
	allow := flags.String("allow", "", "comma-separated prefixes peers may be relayed to although internal, 10.0.0.0/8 say")
	maxAllocs := flags.Int("max-allocations", 1000, "allocations held at once, 0 for no limit")
	perUser := flags.Int("max-per-user", 10, "allocations of one username, 0 for no limit")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg := turnserver.Config{}
	cfg.Realm = *realm
	cfg.MaxAllocations, cfg.MaxAllocationsPerUser = *maxAllocs, *perUser
	static, err := parseUsers(*users)
	if err != nil {
		return err
	}
	if static == nil && *secret == "" {
		return errors.New("give -users, -secret or both")
	}
	cfg.Auth = auth(*realm, static, []byte(*secret))
	if cfg.RelayIP, err = relayAddr(*relayIP); err != nil {
		return err
	}
	if *publicIP != "" {
		if cfg.PublicIP, err = netip.ParseAddr(*publicIP); err != nil {
			return fmt.Errorf("-public-ip: %w", err)
		}
	}
	if *ports != "" {
		lo, hi, ok := strings.Cut(*ports, "-")
		min, err1 := strconv.ParseUint(lo, 10, 16)
		max, err2 := strconv.ParseUint(hi, 10, 16)
		if !ok || err1 != nil || err2 != nil || min == 0 || min > max {
			return fmt.Errorf("-ports %q: want min-max", *ports)
		}
		cfg.PortMin, cfg.PortMax = uint16(min), uint16(max)
	}
	for _, p := range strings.Split(*allow, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			return fmt.Errorf("-allow: %w", err)
		}
		cfg.AllowedPeers = append(cfg.AllowedPeers, prefix)
	}

	server, err := turnserver.New(cfg)
	if err != nil {
		return err
	}
	// The listening address is a UDP binding whose handler is the Server;
	// Start gives it the engine and the binding's index, 0.
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Network: "udp", Addr: *addr, Handler: server})
	if err != nil {
		return err
	}
	server.Start(engine, 0)
	defer server.Close()

	local, err := engine.LocalUDPAddr()
	if err != nil {
		_ = engine.Close()
		return err
	}
	host := cfg.RelayIP
	if cfg.PublicIP.IsValid() {
		host = cfg.PublicIP
	}
	url := "turn:" + net.JoinHostPort(host.String(), strconv.Itoa(local.Port))
	fmt.Fprintf(out, "relaying from %s, ports %s; an RTCIceServer for a page:\n", cfg.RelayIP, portRange(cfg))
	fmt.Fprintln(out, iceServer(url, static, []byte(*secret)))
	return example.Serve(ctx, out, ready, "TURN server listening on %s (UDP)", engine)
}

// auth looks a user up among the fixed ones, then as ephemeral credentials.
func auth(realm string, static map[string]string, secret []byte) turn.AuthFunc {
	var fixed, ephemeral turn.AuthFunc
	if static != nil {
		fixed = turn.StaticAuth(realm, static)
	}
	if len(secret) > 0 {
		ephemeral = turn.EphemeralAuth(secret, time.Now)
	}
	return func(username, r string) ([]byte, bool) {
		if subtle.ConstantTimeCompare([]byte(r), []byte(realm)) != 1 {
			return nil, false
		}
		if fixed != nil {
			if key, ok := fixed(username, r); ok {
				return key, true
			}
		}
		if ephemeral != nil {
			return ephemeral(username, r)
		}
		return nil, false
	}
}

// iceServer is the RTCIceServer JSON for url: with ephemeral credentials
// valid for an hour when there is a secret, else the first fixed user's.
func iceServer(url string, static map[string]string, secret []byte) string {
	s := struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username"`
		Credential string   `json:"credential"`
	}{URLs: []string{url}}
	if len(secret) > 0 {
		s.Username, s.Credential = turn.EphemeralCredentials(secret, "browser", time.Now().Add(time.Hour))
	} else {
		for u, p := range static {
			if s.Username == "" || u < s.Username {
				s.Username, s.Credential = u, p
			}
		}
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func parseUsers(s string) (map[string]string, error) {
	var users map[string]string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		u, p, ok := strings.Cut(f, "=")
		if !ok || u == "" || strings.Contains(u, ":") {
			return nil, fmt.Errorf("-users: %q is not user=password", f)
		}
		if users == nil {
			users = make(map[string]string)
		}
		users[u] = p
	}
	return users, nil
}

// relayAddr is the -relay-ip address, or the first IPv4 address of the
// machine's interfaces: a relayed port binds to one specific address,
// which is what clients are told.
func relayAddr(s string) (netip.Addr, error) {
	if s != "" {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return ip, fmt.Errorf("-relay-ip: %w", err)
		}
		return ip, nil
	}
	addrs, err := ice.HostAddrs()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if a.Is4() {
			return a, nil
		}
	}
	return netip.Addr{}, errors.New("no IPv4 interface address; give -relay-ip")
}

func portRange(cfg turnserver.Config) string {
	if cfg.PortMax == 0 {
		return "the kernel picks"
	}
	return fmt.Sprintf("%d-%d", cfg.PortMin, cfg.PortMax)
}
