//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command requestbody shows what a server and a client can do about a
// request's body before it has arrived: bound it per route, refuse it before
// it is sent, take it from its first byte, and hear that its client went.
//
//	go run ./http/requestbody
//
// The server streams bodies past 1 MiB to their handlers
// (StreamRequestBody), once the first 1 MiB of them has arrived, and reads
// smaller ones whole, except those StreamRequestBodyAtHeader picks, which
// stream from their headers whatever their size. Then:
//
//   - POST /avatar takes at most 64 KiB, below the server's own limits:
//     it is picked to stream from its header, so its handler runs at once,
//     and Context.LimitBody answers a larger one, the 8 MiB one here, 413
//     from its Content-Length, before 100 Continue would have asked for it.
//     A client that sends "Expect: 100-continue" (fib's waits
//     ClientConfig.ExpectContinueTimeout for the 100) never sends the body
//     at all.
//   - POST /chat streams from its header, because
//     Config.StreamRequestBodyAtHeader picks it: the client sends a small
//     first message and waits for the server to have it before it sends the
//     next, as a gRPC client may, which a server waiting for 1 MiB of body
//     would never answer.
//   - POST /upload streams from its header too. The client drops part way
//     through the body (Call.Cancel closes its connection); the handler's
//     OnBody hears the body fail, and the middleware's OnCancel, which knows
//     nothing of the body, hears that the request ended unanswered.
//   - GET /report retains its request and builds the answer elsewhere. A
//     middleware and the handler both register OnCancel; when the client
//     abandons the request (Client.Start's Call.Cancel) both hear it, the
//     handler's first, and the work stops.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"slices"
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

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("requestbody", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address")
	limit := flags.Int64("limit", 64<<10, "the most POST /avatar takes")
	big := flags.Int64("big", 8<<20, "the size of the avatar too large to take, above the 1 MiB threshold")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// What the server's handlers saw, printed by the client in its order.
	events := make(chan string, 16)
	r := fibhttp.NewRouter()
	r.Use(cancelLog(events))
	r.Post("/avatar", func(c *fibhttp.Context) {
		// Below MaxBodyBytes and MaxStreamedBodyBytes, for this route only.
		// The body has not arrived yet: its Content-Length is enough.
		if err := c.LimitBody(*limit); err != nil {
			events <- fmt.Sprintf("server: /avatar refused: %v", err)
			return // LimitBody has answered 413
		}
		var n int
		c.OnBody(func(data []byte, fin bool, err error) {
			n += len(data)
			if fin && err == nil {
				_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "avatar of %d bytes saved\n", n))
			}
		})
	})
	r.Post("/chat", func(c *fibhttp.Context) {
		events <- fmt.Sprintf("server: /chat handler running, body complete: %v", c.BodyComplete())
		var total int
		c.OnBody(func(data []byte, fin bool, err error) {
			total += len(data)
			if len(data) > 0 {
				events <- fmt.Sprintf("server: /chat got %q", data)
			}
			if fin && err == nil {
				_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "%d bytes of chat\n", total))
			}
		})
	})
	r.Post("/upload", func(c *fibhttp.Context) {
		var n int
		c.OnBody(func(data []byte, fin bool, err error) {
			if n == 0 && len(data) > 0 {
				events <- "server: /upload receiving"
			}
			n += len(data)
			if err != nil {
				events <- fmt.Sprintf("server: /upload's OnBody: %v", err)
			} else if fin {
				_ = c.Respond(stdhttp.StatusOK, "text/plain", fmt.Appendf(nil, "%d bytes uploaded\n", n))
			}
		})
	})
	r.Get("/report", func(c *fibhttp.Context) {
		// A report that takes a while, built elsewhere: the response is
		// retained, and OnCancel stops the work if the client goes first.
		c.Retain()
		work := time.AfterFunc(time.Minute, func() {
			_ = c.Respond(stdhttp.StatusOK, "text/plain", []byte("the report\n"))
			c.Release()
		})
		c.OnCancel(func(err error) {
			if work.Stop() {
				events <- fmt.Sprintf("server: handler's OnCancel stopped the report: %v", err)
				c.Release()
			}
		})
		events <- "server: /report building"
	})

	config := fibhttp.DefaultConfig()
	config.StreamRequestBody = true
	config.StreamRequestBodyThreshold = 1 << 20
	// Called on the connection's worker with each request whose body is
	// still to come; true streams it from the header.
	config.StreamRequestBodyAtHeader = func(r *stdhttp.Request) bool {
		return r.URL.Path == "/chat" || r.URL.Path == "/avatar" || r.URL.Path == "/upload"
	}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: fibhttp.NewHandlerWithConfig(config, r)})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	return requests(ctx, "http://"+example.Addr(engine), *big, events, out)
}

// cancelLog registers an OnCancel before the handler runs, as a middleware
// that holds something per request, a place in a queue, say, would.
func cancelLog(events chan<- string) func(fibhttp.Handler) fibhttp.Handler {
	return func(next fibhttp.Handler) fibhttp.Handler {
		return fibhttp.HandlerFunc(func(c *fibhttp.Context) {
			c.OnCancel(func(err error) { events <- fmt.Sprintf("server: middleware's OnCancel: %v", err) })
			next.ServeHTTP(c)
		})
	}
}

// countingBody is a request body that counts what the client took of it.
type countingBody struct {
	r    io.Reader
	read atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return nil }

func requests(ctx context.Context, base string, big int64, events <-chan string, out io.Writer) error {
	engine, stop := example.ClientEngine()
	defer stop()
	config := fibhttp.DefaultClientConfig()
	config.Timeout = 30 * time.Second
	// DefaultClientConfig waits a second for a 100 (Continue).
	config.ExpectContinueTimeout = 5 * time.Second
	client := fibhttp.NewClient(engine, config)
	defer client.Close()
	event := func() string {
		select {
		case e := <-events:
			return e
		case <-time.After(10 * time.Second):
			return "server: (nothing)"
		}
	}

	// Two avatars: a small one, and one LimitBody refuses before a byte of
	// it has been sent.
	for _, size := range []int64{1 << 10, big} {
		body := &countingBody{r: io.LimitReader(zeros{}, size)}
		req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, base+"/avatar", body)
		req.ContentLength = size
		req.Header.Set("Expect", "100-continue")
		resp, err := client.Go(req).Wait()
		if err != nil {
			return err
		}
		answer, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(out, "client: POST /avatar of %d bytes: %s %q, %d bytes of the body sent\n",
			size, resp.Status, strings.TrimSpace(string(answer)), body.read.Load())
		if resp.StatusCode != stdhttp.StatusOK {
			fmt.Fprintln(out, event())
		}
	}

	// A chat: each message goes once the server has the one before it.
	body := fibhttp.NewBodyWriter(0)
	req, _ := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, base+"/chat", body)
	req.ContentLength = -1 // chunked: its length is not known yet
	answer := client.Go(req)
	for i := 1; i <= 3; i++ {
		_, _ = body.Write([]byte(example.Message(i)))
		if i == 1 {
			fmt.Fprintln(out, event()) // the handler, running at the header
		}
		fmt.Fprintln(out, event())
	}
	_ = body.Close()
	resp, err := answer.Wait()
	if err != nil {
		return err
	}
	text, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "client: POST /chat: %s %q\n", resp.Status, strings.TrimSpace(string(text)))

	// An upload the client drops part way: the handler's OnBody and the
	// middleware's OnCancel both hear it, each on its own, so their lines
	// are printed sorted.
	upload := fibhttp.NewBodyWriter(0)
	req, _ = stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, base+"/upload", upload)
	req.ContentLength = -1
	dropped := make(chan error, 1)
	call := client.Start(req, func(_ *fibhttp.ClientResponse, err error) { dropped <- err })
	_, _ = upload.Write(make([]byte, 64<<10))
	fmt.Fprintln(out, event())
	call.Cancel(errors.New("the user cancelled the upload"))
	fmt.Fprintf(out, "client: dropped POST /upload: %v\n", <-dropped)
	heard := []string{event(), event()}
	slices.Sort(heard)
	fmt.Fprintln(out, strings.Join(heard, "\n"))

	// A report the client abandons: both OnCancel callbacks run, the
	// handler's first.
	req, _ = stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, base+"/report", nil)
	failed := make(chan error, 1)
	// Start is Do with a handle to abandon the request by, and no context
	// to pay for.
	call = client.Start(req, func(_ *fibhttp.ClientResponse, err error) { failed <- err })
	fmt.Fprintln(out, event())
	call.Cancel(errors.New("the user closed the tab"))
	fmt.Fprintf(out, "client: abandoned GET /report: %v\n", <-failed)
	fmt.Fprintln(out, event())
	fmt.Fprintln(out, event())
	return nil
}

// zeros reads as endless zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
