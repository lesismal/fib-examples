//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command sendfile serves a file with Context.SendFile, which waits neither
// for the disk nor for the client, and with SendFileStall, which gives up on
// a client that stops reading.
//
//	go run ./http/sendfile                  # downloads, a range and a stalled client, then exits
//	go run ./http/sendfile -tls             # the same over HTTPS
//	go run ./http/sendfile -serve -file big.iso
//	curl -o /dev/null http://127.0.0.1:8080/file
//	curl -r 0-99 http://127.0.0.1:8080/file | xxd | head
//
// SendFile queues the range and returns: the handler is done, no worker
// waits, and done runs once all of it has been handed to the connection.
// On a plain HTTP/1 connection the file goes by sendfile(2) (TransmitFile on
// Windows). Over TLS on Linux, with an AES-GCM suite, the kernel seals it
// (kernel TLS) and it still goes by sendfile; elsewhere it is read and
// sealed 64 KB at a time as the socket drains, so memory stays flat however
// large the file. HTTP/2 and HTTP/3 read it a piece at a time on a pool of
// their own, paced by the stream's flow control.
//
// SendFile waits for the client as long as it takes. SendFileStall bounds
// every such wait: a client that takes nothing for the stall loses the
// response, and one that reads slowly keeps it, which a whole-response
// WriteTimeout cannot tell apart.
package main

import (
	"context"
	"crypto/sha256"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	stdhttp "net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/certs"
	"github.com/lesismal/fib-examples/example"
	fibhttp "github.com/lesismal/fib/http"
	fibtls "github.com/lesismal/fib/tls"
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
	flags := flag.NewFlagSet("sendfile", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:0", "listen address (127.0.0.1:8080 with -serve)")
	serve := flags.Bool("serve", false, "serve until Ctrl-C rather than run the downloads")
	file := flags.String("file", "", "the file to serve; empty writes a random one to the temporary directory")
	size := flags.Int64("size", 32<<20, "the size of the random file, large enough that a client which reads nothing fills the socket buffers")
	stall := flags.Duration("stall", time.Second, "how long /slow waits for a client that takes nothing")
	useTLS := flags.Bool("tls", false, "serve HTTPS with a self-signed certificate")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *serve && *addr == "127.0.0.1:0" {
		*addr = "127.0.0.1:8080"
	}
	path := *file
	if path == "" {
		var err error
		if path, err = randomFile(*size); err != nil {
			return err
		}
		defer os.Remove(path)
	}

	// What SendFile's and SendFileStall's done reported, for the demo.
	results := make(chan string, 16)
	handler := fileHandler(path, *stall, results)
	var tlsConfig *stdtls.Config
	if *useTLS {
		var err error
		// No -cert-out: the client here takes the certificate from memory.
		certFlags := certs.ServerFlagsWithOut(flag.NewFlagSet("", flag.ContinueOnError), "")
		if tlsConfig, err = certFlags.Config(); err != nil {
			return err
		}
		handler = fibtls.NewServer(fibhttp.ConfigureTLS(tlsConfig), handler)
	}
	engine, err := fib.New(example.EngineConfig(), fib.Binding{Addr: *addr, Handler: handler})
	if err != nil {
		return err
	}
	if *serve {
		return example.Serve(ctx, out, ready, "serving "+path+" on %s at /file and /slow", engine)
	}
	stop := example.Start(engine)
	defer stop()
	return downloads(ctx, example.Addr(engine), path, tlsConfig, results, out)
}

// fileHandler serves path at /file, whole or by one byte range, and at /slow
// under a stall bound.
func fileHandler(path string, stall time.Duration, results chan<- string) fib.Handler {
	report := func(what string) func(error) {
		return func(err error) {
			select {
			case results <- fmt.Sprintf("server: %s done: %v", what, err):
			default:
			}
		}
	}
	r := fibhttp.NewRouter()
	r.Get("/file", func(c *fibhttp.Context) {
		f, size, err := open(path)
		if err != nil {
			_ = c.Respond(stdhttp.StatusInternalServerError, "text/plain", []byte(err.Error()))
			return
		}
		offset, n := int64(0), size
		c.Header().Set("Content-Type", "application/octet-stream")
		c.Header().Set("Accept-Ranges", "bytes")
		if first, last, ok := byteRange(c.Request.Header.Get("Range"), size); ok {
			// SendFile leaves a partial response's status and
			// Content-Range to the handler.
			offset, n = first, last-first+1
			c.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, size))
			c.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			c.WriteHeader(stdhttp.StatusPartialContent)
		}
		// Returns at once; the response stays open until done runs, and f
		// must stay open until then.
		done := report(fmt.Sprintf("SendFile %d bytes", n))
		c.SendFile(f, offset, n, func(err error) { f.Close(); done(err) })
	})
	r.Get("/slow", func(c *fibhttp.Context) {
		f, size, err := open(path)
		if err != nil {
			_ = c.Respond(stdhttp.StatusInternalServerError, "text/plain", []byte(err.Error()))
			return
		}
		done := report(fmt.Sprintf("SendFileStall %d bytes, stall %v", size, stall))
		c.SendFileStall(f, 0, size, stall, func(err error) { f.Close(); done(err) })
	})
	return fibhttp.NewHandler(r)
}

func open(path string) (*os.File, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// byteRange parses a Range header of one range, "bytes=first-last" or
// "bytes=first-", which is all this example serves.
func byteRange(header string, size int64) (first, last int64, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	first, err := strconv.ParseInt(a, 10, 64)
	if err != nil || first >= size {
		return 0, 0, false
	}
	last = size - 1
	if b != "" {
		if last, err = strconv.ParseInt(b, 10, 64); err != nil || last < first {
			return 0, 0, false
		}
		last = min(last, size-1)
	}
	return first, last, true
}

// downloads fetches the file whole and by a range, checking both, and then
// asks for /slow and reads nothing.
func downloads(ctx context.Context, addr, path string, tlsConfig *stdtls.Config, results <-chan string, out io.Writer) error {
	want, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	scheme, clientTLS := "http", (*stdtls.Config)(nil)
	if tlsConfig != nil {
		scheme = "https"
		roots := x509.NewCertPool()
		cert, _ := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
		roots.AddCert(cert)
		// HTTP/1.1, whose connection the file goes on by sendfile.
		clientTLS = &stdtls.Config{RootCAs: roots, ServerName: "localhost", NextProtos: []string{"http/1.1"}}
	}
	client := &stdhttp.Client{Timeout: time.Minute, Transport: &stdhttp.Transport{TLSClientConfig: clientTLS}}
	defer client.CloseIdleConnections()
	base := scheme + "://" + addr

	get := func(rangeHeader string) ([]byte, *stdhttp.Response, error) {
		req, err := stdhttp.NewRequestWithContext(ctx, stdhttp.MethodGet, base+"/file", nil)
		if err != nil {
			return nil, nil, err
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return body, resp, err
	}
	start := time.Now()
	body, resp, err := get("")
	if err != nil {
		return err
	}
	if sha256.Sum256(body) != sha256.Sum256(want) {
		return errors.New("the download differs from the file")
	}
	fmt.Fprintf(out, "client: GET /file %s, %d bytes in %v, SHA-256 matches\n", resp.Status, len(body), time.Since(start).Round(time.Millisecond))
	fmt.Fprintln(out, <-results)

	body, resp, err = get("bytes=1000-1999")
	if err != nil {
		return err
	}
	if string(body) != string(want[1000:2000]) {
		return errors.New("the range differs from the file")
	}
	fmt.Fprintf(out, "client: GET /file Range bytes=1000-1999: %s, Content-Range %s\n", resp.Status, resp.Header.Get("Content-Range"))
	fmt.Fprintln(out, <-results)

	// A client that asks and then reads nothing: the socket buffers fill,
	// and the server gives up after the stall.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if clientTLS != nil {
		tc := stdtls.Client(conn, clientTLS)
		if err := tc.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = tc
	}
	if _, err := io.WriteString(conn, "GET /slow HTTP/1.1\r\nHost: example\r\n\r\n"); err != nil {
		return err
	}
	fmt.Fprintln(out, "client: GET /slow, and then read nothing")
	asked := time.Now()
	select {
	case r := <-results:
		fmt.Fprintf(out, "%s, %v after the request\n", r, time.Since(asked).Round(100*time.Millisecond))
	case <-time.After(time.Minute):
		return errors.New("the server kept waiting for a client that reads nothing")
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// randomFile writes size reproducible random bytes to a temporary file.
func randomFile(size int64) (string, error) {
	f, err := os.CreateTemp("", "fib-sendfile-*.bin")
	if err != nil {
		return "", err
	}
	rng := rand.NewChaCha8([32]byte{'f', 'i', 'b'})
	_, err = io.CopyN(f, rng, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return filepath.Clean(f.Name()), nil
}
