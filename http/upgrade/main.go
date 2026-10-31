//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command upgrade is a proxy for a request that switches protocols: it
// switches its client's connection only once the server behind it has
// switched, and relays between the two without a goroutine waiting on
// either.
//
//	go run ./http/upgrade
//
// The pieces, each of which a WebSocket or other Upgrade proxy needs:
//
//   - The proxy's handler retains the request and returns. Context.Upgrade
//     then works later, from any goroutine, on the retained request: on
//     HTTP/1 the connection is held meanwhile, and whatever the client sent
//     after its request is the tunnel's first data.
//   - Client.StartUpgrade sends the request on to the backend; on its 101
//     the connection leaves the client's pool and becomes a tunnel,
//     ClientResponse.Tunnel, whose handler opens once the callback returns.
//   - Each side relays to the other with Tunnel.Send, and when the other
//     side has more than a limit unsent, holds its own reads
//     (Tunnel.HoldReads) until Tunnel.NotifySendable says it has drained:
//     backpressure from end to end, with nothing blocked.
//
// The backend upper-cases what it is sent; the client sends lines and then
// a few megabytes, and checks what comes back.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
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

// protocol is what the client asks to switch to.
const protocol = "shout"

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the proxy's address")
	size := flags.Int("size", 4<<20, "bytes the client sends through the tunnel at the end")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// The backend: switches at once, and upper-cases.
	backendRouter := fibhttp.NewRouter()
	backendRouter.Get("/", func(c *fibhttp.Context) {
		if _, err := c.Upgrade(protocol, nil, shout{}); err != nil {
			_ = c.Respond(stdhttp.StatusBadRequest, "text/plain", []byte(err.Error()))
		}
	})
	backend, err := fib.New(example.EngineConfig(), fib.Binding{Addr: "127.0.0.1:0", Handler: fibhttp.NewHandler(backendRouter)})
	if err != nil {
		return err
	}
	stopBackend := example.Start(backend)
	defer stopBackend()

	// The proxy, with a client of its own for the backend.
	clientEngine, stopClient := example.ClientEngine()
	defer stopClient()
	client := fibhttp.NewClient(clientEngine, fibhttp.DefaultClientConfig())
	defer client.Close()
	var holds atomic.Int64
	proxyRouter := fibhttp.NewRouter()
	proxyRouter.Get("/", proxy(client, "http://"+example.Addr(backend)+"/", &holds))
	proxyEngine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: fibhttp.NewHandler(proxyRouter)})
	if err != nil {
		return err
	}
	stopProxy := example.Start(proxyEngine)
	defer stopProxy()

	if err := talk(ctx, example.Addr(proxyEngine), *size, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "proxy: held one side's reads %d times while the other drained\n", holds.Load())
	return nil
}

// proxy passes an Upgrade request on to backend, and switches its client
// once the backend has.
func proxy(client *fibhttp.Client, backend string, holds *atomic.Int64) fibhttp.HandlerFunc {
	return func(c *fibhttp.Context) {
		if !strings.EqualFold(c.Request.Header.Get("Upgrade"), protocol) {
			_ = c.Respond(stdhttp.StatusUpgradeRequired, "text/plain", []byte("upgrade to "+protocol+"\n"))
			return
		}
		// The answer comes from the backend's, after this returns.
		c.Retain()
		req, _ := stdhttp.NewRequest(stdhttp.MethodGet, backend, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", protocol)
		r := &relay{holds: holds}
		r.init()
		client.StartUpgrade(req, &r.up, func(resp *fibhttp.ClientResponse, err error) {
			defer c.Release()
			if err != nil || resp.Tunnel() == nil {
				_ = c.Respond(stdhttp.StatusBadGateway, "text/plain", []byte("the backend did not switch\n"))
				return
			}
			r.up.peer = resp.Tunnel()
			// The late switch, from this callback's goroutine.
			down, err := c.Upgrade(protocol, nil, &r.down)
			if err != nil {
				_ = r.up.peer.Close()
				return
			}
			r.down.peer = down
		})
	}
}

// relay is the two ends of a proxied tunnel: down is the client's side,
// whose handler sends what arrives to the backend, and up the backend's.
type relay struct {
	up, down side
	holds    *atomic.Int64
}

// side is a TunnelHandler that passes what arrives through one tunnel on
// to its other side's.
type side struct {
	peer  *fibhttp.Tunnel // this side's own tunnel, set once it exists
	other *side
	holds *atomic.Int64
}

func (r *relay) init() {
	r.up.other, r.down.other, r.up.holds, r.down.holds = &r.down, &r.up, r.holds, r.holds
}

func (s *side) OnTunnelOpen(*fibhttp.Tunnel) {}

func (s *side) OnTunnelData(t *fibhttp.Tunnel, data []byte) {
	to := s.other.peer
	if to == nil || to.Send(data) != nil {
		_ = t.Close()
		return
	}
	// The other side is behind: stop reading this one until it drains.
	t.HoldReads(true)
	if to.NotifySendable(256<<10, func(error) { t.HoldReads(false) }) {
		s.holds.Add(1)
		return
	}
	t.HoldReads(false)
}

func (s *side) OnTunnelClose(_ *fibhttp.Tunnel, _ error) {
	if to := s.other.peer; to != nil {
		to.CloseAfterSend()
	}
}

// shout is the backend's protocol: it answers in upper case.
type shout struct{}

func (shout) OnTunnelOpen(*fibhttp.Tunnel) {}
func (shout) OnTunnelData(t *fibhttp.Tunnel, data []byte) {
	_ = t.Send(bytes.ToUpper(data))
}
func (shout) OnTunnelClose(*fibhttp.Tunnel, error) {}

// talk asks the proxy to switch, sends lines through the tunnel, and then
// size bytes at once.
func talk(ctx context.Context, proxy string, size int, out io.Writer) error {
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", proxy)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	// The first line rides with the request, before the switch.
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: proxy\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n%s\n", protocol, example.Message(1))
	r := bufio.NewReader(conn)
	resp, err := stdhttp.ReadResponse(r, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "client: %s, Upgrade: %s\n", resp.Status, resp.Header.Get("Upgrade"))
	if resp.StatusCode != stdhttp.StatusSwitchingProtocols {
		return errors.New("the proxy did not switch")
	}
	for i := 1; i <= 3; i++ {
		if i > 1 {
			fmt.Fprintf(conn, "%s\n", example.Message(i))
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "client: sent %q, got %q\n", example.Message(i), strings.TrimSpace(line))
	}
	// Bulk: more than the relay lets wait unsent, read back as it arrives.
	start := time.Now()
	go func() { _, _ = conn.Write(bytes.Repeat([]byte("abcdefgh"), size/8)) }()
	got := make([]byte, size/8*8)
	if _, err := io.ReadFull(r, got); err != nil {
		return err
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("ABCDEFGH"), size/8)) {
		return errors.New("the bulk came back changed")
	}
	fmt.Fprintf(out, "client: %d bytes through the tunnel and back, upper-cased, in %v\n", len(got), time.Since(start).Round(time.Millisecond))
	return nil
}
