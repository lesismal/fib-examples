//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command bodyfilter transforms response bodies as they are written, with
// the hooks a middleware registers: OnBodyFilter, OnBodyFilterAsync and
// OnHeaderAsync.
//
//	go run ./http/bodyfilter                # runs the requests below, then exits
//	go run ./http/bodyfilter -serve         # serves until Ctrl-C
//	curl --compressed -N http://127.0.0.1:8080/events
//	curl --compressed http://127.0.0.1:8080/page
//	curl -I --compressed http://127.0.0.1:8080/page
//	curl http://127.0.0.1:8080/shout
//
// The gzip middleware returns a BodyFilter that every part of a body goes
// through on its way out, so a response that streams still streams: its
// Flush flushes the compressor too (BodyFlusher), and each event of /events
// reaches the client as it is written, compressed. A response sent whole is
// filtered whole, and the hook sees its Content-Length, which lets it leave a
// short body alone. When a response ends without its body ending, a HEAD or
// a client that went, the filter's Abort gives its compressor back to the
// pool (BodyAborter).
//
// /shout goes through work that may not run on the goroutine writing the
// response, a plugin's, say, run on a pool of its own: OnHeaderAsync holds
// the header until a policy check resumes it, and OnBodyFilterAsync hands
// each part of the body to the pool and sends what comes back.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
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
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fatal(err)
	}
}

// run runs the requests against the server, or with -serve serves until
// ctx ends. ready, if not nil, is told the address once it is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("bodyfilter", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "listen address (127.0.0.1:8080 with -serve)")
	serve := flags.Bool("serve", false, "serve until Ctrl-C rather than run the requests")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *serve && *addr == "127.0.0.1:0" {
		*addr = "127.0.0.1:8080"
	}

	stats := &filterStats{}
	plugin := newPlugin(2)
	defer plugin.close()
	r := fibhttp.NewRouter()
	r.Use(gzipMiddleware(stats))
	r.Get("/events", events)
	r.Get("/page", page)
	r.Head("/page", page)
	r.With(policyMiddleware(plugin), shoutMiddleware(plugin)).Get("/shout", page)

	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: fibhttp.NewHandler(r)})
	if err != nil {
		return err
	}
	if *serve {
		return example.Serve(ctx, out, ready, "body filter server listening on http://%s", engine)
	}
	stop := example.Start(engine)
	defer stop()
	return requests(ctx, "http://"+example.Addr(engine), out, stats)
}

// events streams five events, flushing each.
func events(c *fibhttp.Context) {
	c.Header().Set("Content-Type", "text/event-stream")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(c, "data: event %d at %s\n\n", i, time.Now().Format("15:04:05.000"))
		c.Flush()
		time.Sleep(50 * time.Millisecond)
	}
}

// page answers whole, with a body long enough to be worth compressing.
func page(c *fibhttp.Context) {
	body := strings.Repeat("fib filters response bodies as they are written. ", 40)
	_ = c.Respond(stdhttp.StatusOK, "text/plain; charset=utf-8", []byte(body))
}

// filterStats counts what the gzip filters did, for the summary.
type filterStats struct{ closed, aborted atomic.Int64 }

var gzipPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed); return w }}

// gzipMiddleware compresses the responses of clients that accept gzip.
func gzipMiddleware(stats *filterStats) func(fibhttp.Handler) fibhttp.Handler {
	return func(next fibhttp.Handler) fibhttp.Handler {
		return fibhttp.HandlerFunc(func(c *fibhttp.Context) {
			if !strings.Contains(c.Request.Header.Get("Accept-Encoding"), "gzip") {
				next.ServeHTTP(c)
				return
			}
			c.OnBodyFilter(func(status int, header stdhttp.Header) fibhttp.BodyFilter {
				header.Add("Vary", "Accept-Encoding")
				// A response sent whole tells its length; a short one is
				// not worth a compressor.
				if n := header.Get("Content-Length"); n != "" && len(n) < 3 {
					return nil
				}
				header.Set("Content-Encoding", "gzip")
				f := &gzipFilter{stats: stats, w: gzipPool.Get().(*gzip.Writer)}
				f.w.Reset(&f.buf)
				return f
			})
			next.ServeHTTP(c)
		})
	}
}

// gzipFilter is a BodyFilter, a BodyFlusher and a BodyAborter. What it
// returns is its own buffer, which fib sends or copies before calling it
// again.
type gzipFilter struct {
	stats *filterStats
	w     *gzip.Writer
	buf   bytes.Buffer
}

func (f *gzipFilter) Filter(p []byte) ([]byte, error) {
	f.buf.Reset()
	_, err := f.w.Write(p)
	return f.buf.Bytes(), err // often empty: gzip emits in blocks
}

// Flush runs when the handler flushes, so the event goes out now rather
// than when the compressor's block fills.
func (f *gzipFilter) Flush() ([]byte, error) {
	f.buf.Reset()
	err := f.w.Flush()
	return f.buf.Bytes(), err
}

func (f *gzipFilter) Close() ([]byte, error) {
	f.buf.Reset()
	err := f.w.Close()
	f.release()
	f.stats.closed.Add(1)
	return f.buf.Bytes(), err
}

// Abort runs in place of Close when the body never ends: HEAD, a client
// that went, a write that failed.
func (f *gzipFilter) Abort(error) {
	f.release()
	f.stats.aborted.Add(1)
}

func (f *gzipFilter) release() {
	f.w.Reset(io.Discard)
	gzipPool.Put(f.w)
	f.w = nil
}

// plugin stands for work that has to run on goroutines of its own, as a
// sandboxed plugin's does: jobs go in, and each calls back when done.
type plugin struct {
	jobs chan func()
	wg   sync.WaitGroup
}

func newPlugin(workers int) *plugin {
	p := &plugin{jobs: make(chan func(), 64)}
	for range workers {
		p.wg.Go(func() {
			for job := range p.jobs {
				job()
			}
		})
	}
	return p
}

func (p *plugin) run(job func()) { p.jobs <- job }
func (p *plugin) close()         { close(p.jobs); p.wg.Wait() }

// policyMiddleware decides a header on the plugin's goroutines: the response
// waits, retained, until resume.
func policyMiddleware(p *plugin) func(fibhttp.Handler) fibhttp.Handler {
	return func(next fibhttp.Handler) fibhttp.Handler {
		return fibhttp.HandlerFunc(func(c *fibhttp.Context) {
			c.OnHeaderAsync(func(status int, header stdhttp.Header, resume func(error)) {
				p.run(func() {
					time.Sleep(10 * time.Millisecond) // a lookup elsewhere
					header.Set("X-Policy", fmt.Sprintf("checked %d", status))
					resume(nil)
				})
			})
			next.ServeHTTP(c)
		})
	}
}

// shoutMiddleware upper-cases the body on the plugin's goroutines.
func shoutMiddleware(p *plugin) func(fibhttp.Handler) fibhttp.Handler {
	return func(next fibhttp.Handler) fibhttp.Handler {
		return fibhttp.HandlerFunc(func(c *fibhttp.Context) {
			c.OnBodyFilterAsync(func(int, stdhttp.Header) fibhttp.AsyncBodyFilter { return shout{p} })
			next.ServeHTTP(c)
		})
	}
}

type shout struct{ p *plugin }

// FilterAsync owns p until it calls done; the next call waits for that.
func (s shout) FilterAsync(p []byte, end bool, trailer stdhttp.Header, done func([]byte, error)) {
	s.p.run(func() { done(bytes.ToUpper(p), nil) })
}

// requests runs a request against each route and says what came back.
func requests(ctx context.Context, base string, out io.Writer, stats *filterStats) error {
	client := &stdhttp.Client{Timeout: 10 * time.Second}
	get := func(method, path string, gzipped bool) (*stdhttp.Response, error) {
		req, err := stdhttp.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			return nil, err
		}
		// Set by hand, so that net/http neither asks for gzip itself nor
		// undoes it: what is printed is what was on the wire.
		req.Header.Set("Accept-Encoding", "identity")
		if gzipped {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		return client.Do(req)
	}

	// /events: each event arrives, decompressed, before the next is written.
	resp, err := get("GET", "/events", true)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	lines := bufio.NewScanner(zr)
	for lines.Scan() {
		if line := lines.Text(); line != "" {
			fmt.Fprintf(out, "/events %s, received at %s\n", line, time.Now().Format("15:04:05.000"))
		}
	}
	resp.Body.Close()

	// /page, plain and compressed, and HEAD, whose filter is aborted.
	for _, gz := range []bool{false, true} {
		resp, err := get("GET", "/page", gz)
		if err != nil {
			return err
		}
		wire, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Fprintf(out, "/page Accept-Encoding gzip=%v: %d bytes on the wire, Content-Encoding %q, Content-Length %d\n",
			gz, len(wire), resp.Header.Get("Content-Encoding"), resp.ContentLength)
	}
	resp, err = get("HEAD", "/page", true)
	if err != nil {
		return err
	}
	resp.Body.Close()
	fmt.Fprintf(out, "HEAD /page: Content-Encoding %q\n", resp.Header.Get("Content-Encoding"))

	// /shout: header and body from the plugin's goroutines.
	resp, err = get("GET", "/shout", false)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Fprintf(out, "/shout: X-Policy %q, body %q...\n", resp.Header.Get("X-Policy"), body[:min(len(body), 24)])

	fmt.Fprintf(out, "gzip filters: %d closed, %d aborted\n", stats.closed.Load(), stats.aborted.Load())
	return nil
}
