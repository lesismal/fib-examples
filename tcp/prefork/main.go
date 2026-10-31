//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Command prefork serves a TCP echo from several processes with package
// prefork, and spreads the connections over them round-robin with
// BalanceReusePort.
//
//	go run ./tcp/prefork                      # serves on 127.0.0.1:9020
//	go run ./tcp/prefork -dial 12             # in another terminal: who answered?
//	go run ./tcp/prefork -balance=false       # the kernel's hash instead
//
// prefork.Run makes this process the master: it starts -children copies of
// the program, each with ProcsPerChild Ps and a heap and collector of its
// own, and they all bind the same address with SO_REUSEPORT. A busy server
// that allocates spends far less on garbage collection in many small
// processes than in one large one.
//
// The kernel spreads a reuseport group's connections by a hash of their
// addresses: even over many, uneven over a few. prefork.Config.BalanceReusePort
// and fib.Config.BalanceReusePort attach an eBPF selector to the group that
// hands each new connection to the next socket in turn, across the children
// and across each child's pollers.
//
// Linux only, in two ways:
//
//   - Prefork needs Linux, where SO_REUSEPORT spreads connections between
//     processes. Elsewhere prefork.Run serves in this one process, as a lone
//     child would, and every answer comes from it.
//   - The balancer needs Linux on amd64 or arm64, bpffs mounted at
//     /sys/fs/bpf, and CAP_BPF (root, or CAP_SYS_ADMIN before Linux 5.8).
//     Without them fib logs why and keeps the kernel's hash; the server
//     comes up either way.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib-examples/example"
	"github.com/lesismal/fib/prefork"
)

// fatal reports an error and exits; a test replaces it.
var fatal = example.Fatal

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, nil); err != nil {
		fatal(err)
	}
}

// run serves, as the master or a child, until SIGINT, SIGTERM or ctx ends;
// with -dial it is a client instead. ready, if not nil, is told the address
// once a process serving in-place is listening.
func run(ctx context.Context, args []string, out io.Writer, ready func(addr string)) error {
	flags := flag.NewFlagSet("prefork", flag.ContinueOnError)
	flags.SetOutput(out)
	addr := flags.String("addr", "127.0.0.1:9020", "the address every child listens on")
	children := flags.Int("children", 4, "processes that serve; 0 serves in this one, without prefork")
	balance := flags.Bool("balance", true, "round-robin new connections with BalanceReusePort")
	dial := flags.Int("dial", 0, "be a client: open this many connections and count who answers")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dial > 0 {
		return tally(*addr, *dial, out)
	}

	serve := func(ctx context.Context) error {
		config := example.EngineConfig()
		// Two pollers, each with a listening socket of its own in the
		// reuseport group, which the balancer needs.
		config.IOPollers = true
		config.IOPollerCount = 2
		config.EngineLoop = false
		config.ReusePort = true // set in a prefork child in any case
		config.BalanceReusePort = *balance
		who := fmt.Sprintf("child %d of %d, pid %d", prefork.Child(), prefork.Children(), os.Getpid())
		engine, err := fib.New(config, fib.Binding{Addr: *addr, Handler: fib.HandlerFuncs{
			Data: func(c *fib.Connection, _ []byte) { _ = c.Send([]byte(who)) },
		}})
		if err != nil {
			return err
		}
		return example.Serve(ctx, out, ready, who+": echo listening on %s", engine)
	}
	if *children == 0 {
		return serve(ctx)
	}
	return prefork.RunContext(ctx, prefork.Config{
		Children:         *children,
		ProcsPerChild:    1,
		BalanceReusePort: *balance,
		ShutdownTimeout:  5 * time.Second,
	}, serve)
}

// tally opens n connections, keeping each open until the last, and counts
// which process answered each.
func tally(addr string, n int, out io.Writer) error {
	counts := map[string]int{}
	for range n {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte("who?")); err != nil {
			return err
		}
		b := make([]byte, 128)
		k, err := conn.Read(b)
		if err != nil {
			return err
		}
		counts[string(b[:k])]++
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(out, "%s: %d connections\n", name, counts[name])
	}
	return nil
}
