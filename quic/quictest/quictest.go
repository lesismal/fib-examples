//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Package quictest is the other end of the QUIC examples in their tests: a
// minimal QUIC echo server for the client's, and a minimal client for the
// server's, on fib/http3/quic like the examples themselves.
package quictest

import (
	"bytes"
	"crypto/tls"
	"errors"
	"sync"
	"testing"
	"time"

	fib "github.com/lesismal/fib"
	"github.com/lesismal/fib/http3/quic"
)

// ALPN is the examples' application protocol.
const ALPN = "fib-echo"

// Serve runs a QUIC server that answers each stream with reply(data) once
// the client ends it, until the test ends, and returns its address.
func Serve(t *testing.T, tlsConfig *tls.Config, reply func([]byte) []byte) string {
	t.Helper()
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	h := &serverPath{config: quic.Config{TLSConfig: tlsConfig}, reply: reply, conns: map[*fib.Connection]*quic.Conn{}}
	engine, err := fib.New(fib.DefaultConfig(), fib.Binding{Network: "udp", Addr: "127.0.0.1:0", Handler: h})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := engine.LocalUDPAddr()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run() }()
	t.Cleanup(func() {
		engine.Stop()
		<-done
		_ = engine.Close()
	})
	return addr.String()
}

type serverPath struct {
	config quic.Config
	reply  func([]byte) []byte
	mu     sync.Mutex
	conns  map[*fib.Connection]*quic.Conn
}

func (s *serverPath) OnOpen(*fib.Connection)                 {}
func (s *serverPath) OnPriorityData(*fib.Connection, []byte) {}
func (s *serverPath) OnClose(*fib.Connection, error)         {}

func (s *serverPath) OnData(c *fib.Connection, d []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if qc := s.conns[c]; qc != nil {
		qc.HandleDatagram(d)
	} else if quic.IsInitial(d) {
		s.conns[c] = quic.Accept(c, c.RemoteAddr(), s.config, &replier{reply: s.reply, bufs: map[uint64]*bytes.Buffer{}}, d)
	}
}

// replier collects each stream and answers it at its end.
type replier struct {
	reply func([]byte) []byte
	bufs  map[uint64]*bytes.Buffer
}

func (r *replier) OnHandshake(*quic.Conn) {}
func (r *replier) OnStreamData(st *quic.Stream, data []byte, fin bool) {
	b := r.bufs[st.ID()]
	if b == nil {
		b = &bytes.Buffer{}
		r.bufs[st.ID()] = b
	}
	b.Write(data)
	if fin {
		_ = st.Write(r.reply(b.Bytes()), true)
		delete(r.bufs, st.ID())
	}
}
func (r *replier) OnStreamReset(*quic.Stream, uint64) {}
func (r *replier) OnStopSending(*quic.Stream, uint64) {}
func (r *replier) OnStreamsAvailable(*quic.Conn)      {}
func (r *replier) OnClose(*quic.Conn, error)          {}

// Echo connects to the QUIC server at addr, sends msg on a stream of its
// own and returns what comes back on it by the server's end of it.
func Echo(addr string, tlsConfig *tls.Config, msg []byte) ([]byte, error) {
	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	engine, err := fib.New(fib.DefaultConfig())
	if err != nil {
		return nil, err
	}
	running := make(chan error, 1)
	go func() { running <- engine.Run() }()
	defer func() { engine.Stop(); <-running; _ = engine.Close() }()

	c := &collector{handshake: make(chan struct{}), done: make(chan struct{})}
	dialed := make(chan error, 1)
	err = engine.DialWithHandler("udp", addr, 5*time.Second, collectorPath{c}, func(fc *fib.Connection, err error) {
		if err == nil {
			c.mu.Lock()
			c.qc, err = quic.Dial(fc, fc.RemoteAddr(), quic.Config{TLSConfig: tlsConfig}, c)
			c.mu.Unlock()
		}
		dialed <- err
	})
	if err == nil {
		err = <-dialed
	}
	if err != nil {
		return nil, err
	}
	defer c.qc.Close(0, "")
	timeout := time.After(10 * time.Second)
	select {
	case <-c.handshake:
	case <-timeout:
		return nil, errors.New("no handshake")
	}
	st, err := c.qc.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := st.Write(msg, true); err != nil {
		return nil, err
	}
	select {
	case <-c.done:
		return c.buf.Bytes(), nil
	case <-timeout:
		return nil, errors.New("no echo")
	}
}

// collector is the client's quic.Handler.
type collector struct {
	mu        sync.Mutex
	qc        *quic.Conn
	handshake chan struct{}
	done      chan struct{}
	buf       bytes.Buffer
}

// collectorPath is the client's fib.Handler, feeding it the datagrams.
type collectorPath struct{ c *collector }

func (p collectorPath) OnOpen(*fib.Connection)                 {}
func (p collectorPath) OnPriorityData(*fib.Connection, []byte) {}
func (p collectorPath) OnClose(*fib.Connection, error)         {}
func (p collectorPath) OnData(_ *fib.Connection, d []byte) {
	p.c.mu.Lock()
	qc := p.c.qc
	p.c.mu.Unlock()
	if qc != nil {
		qc.HandleDatagram(d)
	}
}

func (c *collector) OnHandshake(*quic.Conn) { close(c.handshake) }
func (c *collector) OnStreamData(_ *quic.Stream, data []byte, fin bool) {
	c.buf.Write(data)
	if fin {
		close(c.done)
	}
}
func (c *collector) OnStreamReset(*quic.Stream, uint64) {}
func (c *collector) OnStopSending(*quic.Stream, uint64) {}
func (c *collector) OnStreamsAvailable(*quic.Conn)      {}
func (c *collector) OnClose(*quic.Conn, error)          {}
