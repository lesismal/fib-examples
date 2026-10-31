//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command nowait streams a response without a goroutine waiting for the
// client: Context.WriteNoWait takes every byte at once and says when the
// client is behind, and Context.OnWritable says when it has caught up.
//
//	go run ./http/nowait                 # a fast and a slow download, then exits
//	go run ./http/nowait -serve
//	curl -o /dev/null http://127.0.0.1:8080/stream?mb=64
//	curl --limit-rate 1M -o /dev/null http://127.0.0.1:8080/stream?mb=4
//	curl http://127.0.0.1:8080/scratch
//
// Write paces a response by waiting, on the goroutine that called it, until
// the client has taken enough: right for a handler, wrong for a proxy that
// passes on a body from the worker that reads its upstream, or for anything
// that serves many slow clients from a few goroutines. /stream generates its
// body in a function that writes until WriteNoWait reports the response above
// its limit (64 KB queued, or held by HTTP/2's or HTTP/3's flow control),
// hands itself to OnWritable, and returns: nothing waits, and the function
// runs again once the client has taken half.
//
// /scratch answers from a scratch buffer with RespondCopy, which copies the
// body at the call, so the buffer goes back to its pool at once; Respond
// would keep the slice until the response had gone. And a middleware keeps a
// value with each request through SetValue, read back by an OnFinish hook:
// one slot, with no copy of the request as Request.WithContext makes.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strconv"
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
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		fatal(err)
	}
}

// run runs the downloads against the server, or with -serve serves until
// ctx ends. ready, if not nil, is told the address once it is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("nowait", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "listen address (127.0.0.1:8080 with -serve)")
	serve := flags.Bool("serve", false, "serve until Ctrl-C rather than run the downloads")
	mb := flags.Int("mb", 16, "MiB the fast download takes; the slow one takes a quarter of it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *serve && *addr == "127.0.0.1:0" {
		*addr = "127.0.0.1:8080"
	}

	logs := make(chan string, 16)
	r := fibhttp.NewRouter()
	r.Use(accessLog(logs, out, *serve))
	r.Get("/stream", stream)
	r.Get("/scratch", scratch)
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: fibhttp.NewHandler(r)})
	if err != nil {
		return err
	}
	if *serve {
		return example.Serve(ctx, out, ready, "nowait server listening on http://%s", engine)
	}
	stop := example.Start(engine)
	defer stop()
	return downloads(ctx, "http://"+example.Addr(engine), *mb, logs, out)
}

// entry is what the access log keeps with a request.
type entry struct {
	start  time.Time
	pauses int // how often /stream found the client behind
}

// accessLog keeps an entry with each request, and logs it once the response
// has been handed to the connection: to logs, or with -serve to out.
func accessLog(logs chan<- string, out io.Writer, serve bool) func(fibhttp.Handler) fibhttp.Handler {
	return func(next fibhttp.Handler) fibhttp.Handler {
		return fibhttp.HandlerFunc(func(c *fibhttp.Context) {
			e := &entry{start: time.Now()}
			// Before the request reaches another goroutine, as the slot
			// takes no lock.
			c.SetValue(e)
			path := c.Request.URL.RequestURI()
			c.OnFinish(func(status int, _ stdhttp.Header, size int64) {
				line := fmt.Sprintf("server: %s %d, %d bytes in %v, client behind %d times",
					path, status, size, time.Since(e.start).Round(time.Millisecond), e.pauses)
				if serve {
					fmt.Fprintln(out, line)
					return
				}
				logs <- line
			})
			next.ServeHTTP(c)
		})
	}
}

// chunk is what /stream writes, again and again.
var chunk = func() []byte {
	b := make([]byte, 16<<10)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return b
}()

// stream writes ?mb= MiB without ever waiting for the client.
func stream(c *fibhttp.Context) {
	mb, err := strconv.Atoi(c.Request.URL.Query().Get("mb"))
	if err != nil || mb < 1 {
		mb = 1
	}
	left := int64(mb) << 20
	e := c.Value().(*entry)
	c.Header().Set("Content-Type", "text/plain")
	c.Header().Set("Content-Length", strconv.FormatInt(left, 10))
	// The response stays open after the handler returns, until Release.
	c.Retain()
	var pump func(error)
	pump = func(err error) {
		for err == nil && left > 0 {
			var above bool
			above, err = c.WriteNoWait(chunk[:min(left, int64(len(chunk)))])
			left -= int64(min(left, int64(len(chunk))))
			if above && left > 0 {
				// Counted first: once OnWritable has it, pump may run
				// again on another goroutine before this one goes on.
				e.pauses++
				if c.OnWritable(pump) {
					// The client is behind: pump runs again once it has
					// taken half of what is queued.
					return
				}
				// The client caught up already.
				e.pauses--
			}
		}
		// The body is all written, or the client went (err): either way
		// the last hold goes, which ends the response.
		c.Release()
	}
	pump(nil)
}

// scratchPool holds the buffers /scratch builds its answers in.
var scratchPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

// scratch builds its answer in a pooled buffer and gives it back at once.
func scratch(c *fibhttp.Context) {
	buf := scratchPool.Get().(*[]byte)
	b := fmt.Appendf((*buf)[:0], "built in a pooled buffer at %s\n", time.Now().Format(time.RFC3339Nano))
	// The body is copied here, so the buffer is free to be reused the
	// moment RespondCopy returns, by this request or the next.
	_ = c.RespondCopy(stdhttp.StatusOK, "text/plain; charset=utf-8", b)
	*buf = b
	scratchPool.Put(buf)
}

// downloads takes /stream quickly and slowly, and /scratch.
func downloads(ctx context.Context, base string, mb int, logs <-chan string, out io.Writer) error {
	// A connection each, so the slow download does not inherit the socket
	// buffers the fast one grew.
	client := &stdhttp.Client{Timeout: time.Minute, Transport: &stdhttp.Transport{DisableKeepAlives: true}}
	get := func(path string, slow bool) error {
		req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var n int64
		buf := make([]byte, 32<<10)
		for {
			k, err := resp.Body.Read(buf)
			n += int64(k)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if slow {
				time.Sleep(2 * time.Millisecond) // about 16 MB/s at most
			}
		}
		speed := "quickly"
		if slow {
			speed = "slowly"
		}
		fmt.Fprintf(out, "client: GET %s read %s, %d bytes in %v\n", path, speed, n, time.Since(start).Round(time.Millisecond))
		select {
		case line := <-logs:
			fmt.Fprintln(out, line)
		case <-time.After(10 * time.Second):
			return fmt.Errorf("no access log for %s", path)
		}
		return nil
	}
	if err := get(fmt.Sprintf("/stream?mb=%d", mb), false); err != nil {
		return err
	}
	if err := get(fmt.Sprintf("/stream?mb=%d", max(mb/4, 1)), true); err != nil {
		return err
	}
	return get("/scratch", false)
}
