//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command duplex shows Context.ServeDuplex: a handler that reads its request
// body and writes its response at the same time.
//
//	go run ./http/duplex
//	go run ./http/duplex -n 10
//
// The server answers POST /upper with every line of the body in upper case,
// each as soon as it has read it. The client, net/http over h2c, sends one
// line, waits for its answer, and only then sends the next, so the exchange
// deadlocks unless the server writes before the body has ended: a chat over
// one request, as a gRPC bidirectional stream is.
//
// A handler that takes its body through OnBody, or reads Request.Body until
// it would block, answers once it has it; ServeDuplex hands the request to a
// goroutine of its own instead, whose Read waits for the client without
// holding an engine worker, and whose Write and Flush send the response as
// it is produced. HTTP/2 and HTTP/3 streams are full duplex; HTTP/1.1 is too
// on the wire, but most clients stop sending once the response begins.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	stdhttp "net/http"
	"os"
	"os/signal"
	"strings"
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
	flags := flag.NewFlagSet("duplex", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "the server's address")
	n := flags.Int("n", 5, "lines to send, each after the answer to the last")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	router := fibhttp.NewRouter()
	router.Post("/upper", func(c *fibhttp.Context) {
		if err := c.ServeDuplex(upper); err != nil {
			_ = c.Respond(stdhttp.StatusInternalServerError, "text/plain", []byte(err.Error()))
		}
	})
	config := fibhttp.DefaultConfig()
	// The handler runs as soon as the header has arrived, and the body
	// streams to it; without this it would run only once the body had all
	// arrived, which here is never.
	config.StreamRequestBody = true
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: fibhttp.NewHandlerWithConfig(config, router)})
	if err != nil {
		return err
	}
	stop := example.Start(engine)
	defer stop()
	url := "http://" + example.Addr(engine) + "/upper"
	fmt.Fprintf(out, "server: POST %s\n", url)
	return chat(ctx, url, *n, out)
}

// upper answers each line of the body as soon as it has read it. It runs on
// a goroutine of its own, so its Read may wait for the client.
func upper(d *fibhttp.Duplex) {
	d.Context().Header().Set("Content-Type", "text/plain; charset=utf-8")
	lines := bufio.NewScanner(d)
	for lines.Scan() {
		if _, err := fmt.Fprintf(d, "%s\n", strings.ToUpper(lines.Text())); err != nil {
			return
		}
		// Without Flush the answer could wait for more to send with it.
		if d.Flush() != nil {
			return
		}
	}
	// Returning closes the duplex, which ends the response.
}

// chat sends n lines over one request, each once the answer to the one
// before it has come back.
func chat(ctx context.Context, url string, n int, out io.Writer) error {
	transport := &stdhttp.Transport{Protocols: new(stdhttp.Protocols)}
	// h2c: HTTP/2 without TLS, whose streams carry both directions at once.
	transport.Protocols.SetUnencryptedHTTP2(true)
	defer transport.CloseIdleConnections()
	client := &stdhttp.Client{Transport: transport}

	body, send := io.Pipe()
	req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodPost, url, body)
	if err != nil {
		return err
	}
	// The response's header comes back once the handler first flushes,
	// which is after the first line, so that is sent before Do waits.
	go func() { _, _ = io.WriteString(send, example.Message(1)+"\n") }()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fmt.Fprintf(out, "client: %s %s\n", resp.Proto, resp.Status)
	answers := bufio.NewScanner(resp.Body)
	for i := 1; i <= n; i++ {
		if !answers.Scan() {
			return fmt.Errorf("no answer to line %d: %w", i, errors.Join(answers.Err(), io.ErrUnexpectedEOF))
		}
		fmt.Fprintf(out, "client: sent %q, got %q\n", example.Message(i), answers.Text())
		if i < n {
			// Only now, with the answer in, does the next line go.
			if _, err := io.WriteString(send, example.Message(i+1)+"\n"); err != nil {
				return err
			}
		}
	}
	_ = send.Close()
	if answers.Scan() {
		return fmt.Errorf("an answer to nothing: %q", answers.Text())
	}
	return answers.Err()
}
