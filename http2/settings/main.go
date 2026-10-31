//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command settings tunes HTTP/2 on both ends and shows what each knob puts
// on the wire: the windows, frame size and HPACK table a server announces,
// the PINGs that find a dead peer, the flood limits, and GOAWAY when the
// engine closes.
//
//	go run ./http2/settings
//
// The server is h2c (HTTP/2 without TLS) so that a raw connection can read
// its frames; over TLS the same settings apply to h2.
//
//   - Config.HTTP2 and ClientConfig.HTTP2 (an HTTP2Config) set the stream
//     and connection receive windows, SETTINGS_MAX_FRAME_SIZE, the HPACK
//     dynamic table, and AdaptiveWindow, which grows the windows to the
//     path's bandwidth-delay product as gRPC does. Their zero value is the
//     defaults: 1 MB and 16 MB windows, 16 KB frames, a 4 KB table.
//   - Config.PingInterval has the server PING a client that has sent
//     nothing for that long, and close it if nothing at all comes back
//     within PingTimeout: a machine that lost power, or a NAT that forgot
//     the connection. ClientConfig.ReadIdleTimeout and PingTimeout do the
//     same from the client, which then fails its requests with
//     ErrPingTimeout rather than at their Timeout.
//   - StreamResetBurst/Rate and ControlFrameBurst/Rate bound the resets
//     (Rapid Reset) and the PINGs, SETTINGS and empty frames (CVE-2019-9512
//     and kin) a client may send; past them the connection ends with
//     GOAWAY(ENHANCE_YOUR_CALM).
//   - Engine.Close sends each HTTP/2 connection GOAWAY(NO_ERROR), naming
//     the last stream it took, before cutting it, so a client knows what was
//     served. Engine.Shutdown is the graceful path.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"os"
	"os/signal"
	"sync"
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

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("settings", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address")
	size := flags.Int("size", 16<<20, "bytes the client uploads and downloads")
	ping := flags.Duration("ping", 200*time.Millisecond, "PingInterval and ReadIdleTimeout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	config := fibhttp.DefaultConfig()
	config.HTTP2Only = true
	config.HTTP2 = fibhttp.HTTP2Config{
		StreamWindow:    4 << 20,
		ConnWindow:      32 << 20,
		MaxFrameSize:    64 << 10,
		HeaderTableSize: 16 << 10,
		AdaptiveWindow:  true,
	}
	config.PingInterval = *ping
	config.PingTimeout = time.Second
	config.ControlFrameBurst = 20
	config.ControlFrameRate = 10
	config.StreamResetBurst = 50
	config.StreamResetRate = 10
	config.MaxBodyBytes = int64(*size)
	blob := bytes.Repeat([]byte("h2"), *size/2)
	r := fibhttp.NewRouter()
	r.Get("/blob", func(c *fibhttp.Context) { _ = c.Respond(stdhttp.StatusOK, "application/octet-stream", blob) })
	r.Post("/blob", func(c *fibhttp.Context) {
		n, _ := io.Copy(io.Discard, c.Request.Body)
		_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "%d", n))
	})
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: fibhttp.NewHandlerWithConfig(config, r)})
	if err != nil {
		return err
	}
	// Called early below, to close the engine; once only.
	stop := sync.OnceValue(example.Start(engine))
	defer stop()
	server := example.Addr(engine)

	if err := transfers(ctx, server, *size, *ping, out); err != nil {
		return err
	}
	if err := deadServer(ctx, *ping, out); err != nil {
		return err
	}
	if err := flood(server, out); err != nil {
		return err
	}

	// A raw connection: the server's SETTINGS, its PING once we go quiet,
	// and its GOAWAY when the engine closes.
	conn, err := dialH2(server)
	if err != nil {
		return err
	}
	defer conn.Close()
	f, err := readFrame(conn, frameSettings)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "raw: server SETTINGS %s\n", settingsString(f.payload))
	start := time.Now()
	f, err = readFrame(conn, framePing)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "raw: server PING after %v of silence\n", time.Since(start).Round(*ping/2))
	if err := writeFrame(conn, framePing, flagAck, 0, f.payload); err != nil {
		return err
	}
	_ = stop() // Engine.Close, as stop calls it
	f, err = readFrame(conn, frameGoAway)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "raw: engine closed, server GOAWAY %s\n", goAwayString(f.payload))
	return nil
}

// transfers moves size bytes each way through a fib client whose windows
// and frame size are raised to match.
func transfers(ctx context.Context, server string, size int, ping time.Duration, out io.Writer) error {
	engine, stop := example.ClientEngine()
	defer stop()
	config := fibhttp.DefaultClientConfig()
	config.Timeout = time.Minute
	config.UnencryptedHTTP2 = true
	config.MaxResponseBodyBytes = int64(size)
	config.HTTP2 = fibhttp.HTTP2Config{StreamWindow: 4 << 20, ConnWindow: 32 << 20, MaxFrameSize: 64 << 10, AdaptiveWindow: true}
	config.ReadIdleTimeout = ping
	client := fibhttp.NewClient(engine, config)
	defer client.Close()

	start := time.Now()
	req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, "http://"+server+"/blob", nil)
	resp, err := client.Go(req).Wait()
	if err != nil {
		return err
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	fmt.Fprintf(out, "client: %s GET /blob: %d bytes in %v\n", resp.Proto, n, time.Since(start).Round(time.Millisecond))

	start = time.Now()
	req, _ = stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, "http://"+server+"/blob", bytes.NewReader(make([]byte, size)))
	resp, err = client.Go(req).Wait()
	if err != nil {
		return err
	}
	answer, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "client: %s POST /blob: server read %s bytes in %v\n", resp.Proto, answer, time.Since(start).Round(time.Millisecond))
	return nil
}

// deadServer sends a request to a server that takes the connection and
// then goes silent, as one whose machine lost power does: the client's PING
// finds it.
func deadServer(ctx context.Context, ping time.Duration, out io.Writer) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// SETTINGS and an ack of the client's, and then nothing at all.
		_ = writeFrame(conn, frameSettings, 0, 0, nil)
		_ = writeFrame(conn, frameSettings, flagAck, 0, nil)
		_, _ = io.Copy(io.Discard, conn)
	}()

	engine, stop := example.ClientEngine()
	defer stop()
	config := fibhttp.DefaultClientConfig()
	config.Timeout = 30 * time.Second
	config.UnencryptedHTTP2 = true
	config.ReadIdleTimeout = ping
	config.PingTimeout = 2 * ping
	client := fibhttp.NewClient(engine, config)
	defer client.Close()
	start := time.Now()
	req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, "http://"+ln.Addr().String()+"/", bytes.NewReader([]byte("anyone?")))
	_, err = client.Go(req).Wait()
	if !errors.Is(err, fibhttp.ErrPingTimeout) {
		return fmt.Errorf("a silent server: %v, want ErrPingTimeout", err)
	}
	fmt.Fprintf(out, "client: silent server found dead after %v, not the 30s Timeout: %v\n", time.Since(start).Round(ping/2), err)
	return nil
}

// flood sends PINGs past the server's ControlFrameBurst.
func flood(server string, out io.Writer) error {
	conn, err := dialH2(server)
	if err != nil {
		return err
	}
	defer conn.Close()
	for i := range 100 {
		if writeFrame(conn, framePing, 0, 0, binary.BigEndian.AppendUint64(nil, uint64(i))) != nil {
			break // the server has closed
		}
	}
	f, err := readFrame(conn, frameGoAway)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "raw: 100 PINGs at once, server GOAWAY %s\n", goAwayString(f.payload))
	return nil
}

// The few HTTP/2 frames this example reads and writes (RFC 9113 section 6).
const (
	frameSettings = 0x4
	framePing     = 0x6
	frameGoAway   = 0x7
	flagAck       = 0x1
)

type frame struct {
	typ, flags byte
	payload    []byte
}

// dialH2 opens an h2c connection with prior knowledge: the preface and an
// empty SETTINGS.
func dialH2(addr string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(conn, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, writeFrame(conn, frameSettings, 0, 0, nil)
}

func writeFrame(w io.Writer, typ, flags byte, stream uint32, payload []byte) error {
	head := []byte{byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload)), typ, flags}
	head = binary.BigEndian.AppendUint32(head, stream)
	_, err := w.Write(append(head, payload...))
	return err
}

// readFrame reads frames until one of typ, acking the server's SETTINGS on
// the way, as a client must within the server's SettingsTimeout.
func readFrame(conn net.Conn, typ byte) (frame, error) {
	head := make([]byte, 9)
	for {
		if _, err := io.ReadFull(conn, head); err != nil {
			return frame{}, fmt.Errorf("waiting for frame type %d: %w", typ, err)
		}
		f := frame{typ: head[3], flags: head[4], payload: make([]byte, int(head[0])<<16|int(head[1])<<8|int(head[2]))}
		if _, err := io.ReadFull(conn, f.payload); err != nil {
			return frame{}, err
		}
		if f.typ == frameSettings && f.flags&flagAck == 0 {
			if err := writeFrame(conn, frameSettings, flagAck, 0, nil); err != nil {
				return frame{}, err
			}
		}
		if f.typ == typ && f.flags&flagAck == 0 {
			return f, nil
		}
	}
}

func settingsString(p []byte) string {
	names := map[uint16]string{1: "HEADER_TABLE_SIZE", 2: "ENABLE_PUSH", 3: "MAX_CONCURRENT_STREAMS",
		4: "INITIAL_WINDOW_SIZE", 5: "MAX_FRAME_SIZE", 6: "MAX_HEADER_LIST_SIZE", 8: "ENABLE_CONNECT_PROTOCOL"}
	var b bytes.Buffer
	for ; len(p) >= 6; p = p[6:] {
		id, v := binary.BigEndian.Uint16(p), binary.BigEndian.Uint32(p[2:])
		name := names[id]
		if name == "" {
			name = fmt.Sprintf("0x%x", id)
		}
		fmt.Fprintf(&b, "%s=%d ", name, v)
	}
	return string(bytes.TrimSpace(b.Bytes()))
}

func goAwayString(p []byte) string {
	if len(p) < 8 {
		return fmt.Sprintf("%x", p)
	}
	codes := map[uint32]string{0x0: "NO_ERROR", 0xb: "ENHANCE_YOUR_CALM", 0x1: "PROTOCOL_ERROR", 0x4: "SETTINGS_TIMEOUT"}
	code := binary.BigEndian.Uint32(p[4:])
	name := codes[code]
	if name == "" {
		name = fmt.Sprintf("0x%x", code)
	}
	return fmt.Sprintf("%s, last stream %d", name, binary.BigEndian.Uint32(p)&0x7fffffff)
}
