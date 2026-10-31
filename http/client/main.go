//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command client shows fib's HTTP client beyond Do: bounding a server's
// answer, capping the requests that wait for a connection, taking a body as
// it trickles in and pausing it, handing a whole body on without a copy,
// pushing an upload with a trailer, and decoding a gzip transfer coding.
//
//	go run ./http/client
//
// It starts a server of its own, and runs against it:
//
//   - MaxPendingPerHost caps the requests waiting for one of the
//     MaxConnsPerHost connections to a host; past it a request fails at once
//     with ErrTooManyPending, which a proxy answers 503, rather than joining
//     a queue that grows for as long as the host is stuck.
//   - ResponseHeaderTimeout bounds the wait for a response's header alone,
//     where Timeout bounds the whole exchange: a server slow to begin fails
//     with ErrResponseHeaderTimeout.
//   - StreamResponseBody with StreamUnknownLengthAtHeader runs the callback of
//     a chunked body at its header, so each event of a stream arrives as it is
//     sent; ClientResponse.HoldBody pauses the delivery, as a consumer that
//     must not wait does, and resumes it.
//   - ClientResponse.BodyBytes hands out a body that arrived whole: the
//     client's own slice, with no read into another.
//   - A BodyWriter is a request body the caller pushes as it is produced, and
//     CloseWithTrailer sends a checksum, known only at the end, after it.
//   - A response whose Transfer-Encoding is "gzip, chunked" (which net/http
//     refuses) is decoded as it is read.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strings"
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
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address; the legacy server takes the next binding's")
	headerTimeout := flags.Duration("header-timeout", 300*time.Millisecond, "ResponseHeaderTimeout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	engine, err := fib.New(example.EngineConfig(),
		fib.Binding{Addr: *addr, Handler: fibhttp.NewHandler(server())},
		// A server that speaks a transfer coding fib's own does not send.
		fib.Binding{Addr: "127.0.0.1:0", Handler: legacy()},
	)
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	addrs, err := engine.LocalAddrs()
	if err != nil {
		return err
	}
	base, legacyBase := "http://"+addrs[0].String(), "http://"+addrs[1].String()

	clientEngine, stopClient := example.ClientEngine()
	defer stopClient()
	config := fibhttp.DefaultClientConfig()
	config.Timeout = 10 * time.Second
	config.ResponseHeaderTimeout = *headerTimeout
	config.MaxConnsPerHost = 1
	config.MaxPendingPerHost = 1
	// Bodies of no declared length go to the callback at their header;
	// those with a length stay whole up to the threshold.
	config.StreamResponseBody = true
	config.StreamResponseBodyThreshold = 1 << 20
	config.StreamUnknownLengthAtHeader = true
	client := fibhttp.NewClient(clientEngine, config)
	defer client.Close()

	steps := []func(context.Context, *fibhttp.Client, string, io.Writer) error{
		pendingStep, headerTimeoutStep, eventsStep, bodyBytesStep, trailerStep,
	}
	for _, step := range steps {
		if err := step(ctx, client, base, out); err != nil {
			return err
		}
	}
	return transferCodingStep(ctx, client, legacyBase, out)
}

// server is the routes the client runs against.
func server() *fibhttp.Router {
	r := fibhttp.NewRouter()
	// /wait?d= answers after d.
	r.Get("/wait", func(c *fibhttp.Context) {
		d, _ := time.ParseDuration(c.Request.URL.Query().Get("d"))
		c.Retain()
		time.AfterFunc(d, func() {
			_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "waited %v", d))
			c.Release()
		})
	})
	// /events writes five events, a little apart, of no declared length.
	r.Get("/events", func(c *fibhttp.Context) {
		c.Header().Set("Content-Type", "text/event-stream")
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(c, "data: event %d\n\n", i)
			c.Flush()
			time.Sleep(30 * time.Millisecond)
		}
	})
	r.Get("/small", func(c *fibhttp.Context) {
		_ = c.Respond(stdhttp.StatusOK, "application/json", []byte(`{"answer":42}`))
	})
	// /upload checks the body against the checksum in its trailer.
	r.Post("/upload", func(c *fibhttp.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		sum := sha256.Sum256(body)
		got := c.Request.Trailer.Get("X-Checksum")
		verdict := "matches"
		if got != hex.EncodeToString(sum[:]) {
			verdict = "does not match"
		}
		_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "%d bytes, trailer X-Checksum %.16s... %s", len(body), got, verdict))
	})
	return r
}

// legacy answers every request with a body whose transfer codings are gzip
// and then chunked, as some old servers and appliances send.
func legacy() fib.Handler {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = io.WriteString(w, strings.Repeat("compressed by the transfer, not the content. ", 20))
	_ = w.Close()
	var resp bytes.Buffer
	resp.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: gzip, chunked\r\n\r\n")
	for chunk := range slices(gz.Bytes(), 64) {
		fmt.Fprintf(&resp, "%x\r\n%s\r\n", len(chunk), chunk)
	}
	resp.WriteString("0\r\n\r\n")
	return fib.HandlerFuncs{Data: func(c *fib.Connection, data []byte) {
		if bytes.Contains(data, []byte("\r\n\r\n")) {
			_ = c.Send(resp.Bytes())
		}
	}}
}

// slices yields b in pieces of at most n bytes.
func slices(b []byte, n int) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		for len(b) > 0 {
			k := min(n, len(b))
			if !yield(b[:k]) {
				return
			}
			b = b[k:]
		}
	}
}

func get(ctx context.Context, url string) *stdhttp.Request {
	req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, url, nil)
	return req
}

func headerTimeoutStep(ctx context.Context, client *fibhttp.Client, base string, out io.Writer) error {
	_, err := client.Go(get(ctx, base+"/wait?d=2s")).Wait()
	if !errors.Is(err, fibhttp.ErrResponseHeaderTimeout) {
		return fmt.Errorf("a slow header: %v, want ErrResponseHeaderTimeout", err)
	}
	fmt.Fprintf(out, "header timeout: GET /wait?d=2s: %v\n", err)
	return nil
}

// pendingStep sends three requests at once over the one connection allowed:
// one goes, one waits, and one is turned away. It runs first, with no
// connection yet: the first request has one dialed for it, which does not
// count as waiting, and the dialed connection takes it, so the count is the
// same whether the burst meets the dial in progress or done. A connection the
// client closed, as the header timeout's is, stops counting against
// MaxConnsPerHost before its failed request hears why (fib 9824c68), so the
// step could run later too; first keeps the demo's order easy to follow.
func pendingStep(ctx context.Context, client *fibhttp.Client, base string, out io.Writer) error {
	results := make([]string, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		client.Do(get(ctx, fmt.Sprintf("%s/wait?d=%dms", base, 50+50*i)), func(resp *fibhttp.ClientResponse, err error) {
			defer wg.Done()
			if err != nil {
				results[i] = err.Error()
				if errors.Is(err, fibhttp.ErrTooManyPending) {
					results[i] += " (would be a 503)"
				}
				return
			}
			body, _ := resp.BodyBytes()
			results[i] = string(body)
		})
	}
	wg.Wait()
	for i, r := range results {
		fmt.Fprintf(out, "pending: request %d: %s\n", i+1, r)
	}
	return nil
}

// eventsStep takes a chunked body event by event, holding it for a while
// after the second.
func eventsStep(ctx context.Context, client *fibhttp.Client, base string, out io.Writer) error {
	start := time.Now()
	done := make(chan error, 1)
	client.Do(get(ctx, base+"/events"), func(resp *fibhttp.ClientResponse, err error) {
		if err != nil {
			done <- err
			return
		}
		fmt.Fprintf(out, "events: callback at the header, %v in\n", time.Since(start).Round(10*time.Millisecond))
		events := 0
		resp.OnBody(func(data []byte, fin bool, err error) {
			for line := range strings.SplitSeq(string(data), "\n") {
				if line == "" {
					continue
				}
				events++
				fmt.Fprintf(out, "events: %q, %v in\n", line, time.Since(start).Round(10*time.Millisecond))
				if events == 2 {
					// A consumer with no room: pause, and resume later
					// from elsewhere. Nothing waits in the meantime.
					resp.HoldBody(true)
					fmt.Fprintln(out, "events: holding the body for 300ms")
					time.AfterFunc(300*time.Millisecond, func() { resp.HoldBody(false) })
				}
			}
			if fin || err != nil {
				done <- err
			}
		})
	})
	return <-done
}

func bodyBytesStep(ctx context.Context, client *fibhttp.Client, base string, out io.Writer) error {
	done := make(chan error, 1)
	client.Do(get(ctx, base+"/small"), func(resp *fibhttp.ClientResponse, err error) {
		if err != nil {
			done <- err
			return
		}
		// The client's own copy, never reused: it may be kept, or handed
		// to a server's WriteResponse, past this callback.
		body, whole := resp.BodyBytes()
		fmt.Fprintf(out, "body bytes: %s, arrived whole: %v\n", body, whole)
		done <- nil
	})
	return <-done
}

// trailerStep pushes an upload a piece at a time, and its checksum after it.
func trailerStep(ctx context.Context, client *fibhttp.Client, base string, out io.Writer) error {
	body := fibhttp.NewBodyWriter(0)
	req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, base+"/upload", body)
	req.ContentLength = -1 // chunked, which a trailer needs over HTTP/1
	// Servers keep the trailer fields a request names up front.
	req.Trailer = stdhttp.Header{"X-Checksum": nil}
	answer := client.Go(req)
	sum := sha256.New()
	for i := range 4 {
		piece := bytes.Repeat([]byte{'a' + byte(i)}, 10000)
		sum.Write(piece)
		// Write copies what it cannot send yet and never waits.
		if _, err := body.Write(piece); err != nil {
			return err
		}
	}
	_ = body.CloseWithTrailer(stdhttp.Header{"X-Checksum": {hex.EncodeToString(sum.Sum(nil))}})
	resp, err := answer.Wait()
	if err != nil {
		return err
	}
	text, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "trailer: POST /upload: %s\n", text)
	return nil
}

func transferCodingStep(ctx context.Context, client *fibhttp.Client, base string, out io.Writer) error {
	resp, err := client.Go(get(ctx, base+"/")).Wait()
	if err != nil {
		return err
	}
	text, err := io.ReadAll(bufio.NewReader(resp.Body))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "transfer coding: %v, %d bytes decoded: %.44q...\n", resp.TransferEncoding, len(text), text)
	return nil
}
