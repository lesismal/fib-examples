// Package session drives a fib DTLS connection for the DTLS examples.
//
// fib's tls.DTLSConn is sans-IO: it never reads a socket, starts a goroutine
// or reads a clock. Its owner hands it each datagram that arrives
// (HandleDatagram), sends what it emits (DTLSOptions.Output), and calls
// HandleTimeout when NextTimeout says, which is how lost handshake flights
// are sent again. A Session is that owner: a mutex serializes the
// connection, which is not safe for concurrent use, a timer follows
// NextTimeout, and the application's callbacks run after the mutex is
// released, so that they may Write.
package session

import (
	stdtls "crypto/tls"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	fibtls "github.com/lesismal/fib/tls"
)

// SRTP protection profiles the examples negotiate, in order of preference,
// as a WebRTC peer does with the use_srtp extension.
var SRTPProfiles = []uint16{fibtls.SRTP_AEAD_AES_128_GCM, fibtls.SRTP_AES128_CM_HMAC_SHA1_80}

// Callbacks are what a Session tells its application, outside its mutex.
type Callbacks struct {
	// OnHandshake runs once the handshake is complete.
	OnHandshake func(s *Session)
	// OnData receives the application data of one record; data belongs
	// to it.
	OnData func(s *Session, data []byte)
	// OnClose runs once the connection has ended: err is io.EOF when the
	// peer closed it.
	OnClose func(s *Session, err error)
}

// Session is one DTLS connection and the timer that drives it.
type Session struct {
	cb Callbacks

	mu        sync.Mutex
	conn      *fibtls.DTLSConn
	timer     *time.Timer
	done      bool
	handshake bool     // complete, not yet reported
	data      [][]byte // received, not yet delivered
	err       error    // ended, not yet reported
}

// New makes the client or server side of a connection that sends through
// output, whose buffer is valid only during the call.
func New(client bool, config *stdtls.Config, output func([]byte), opts fibtls.DTLSOptions, cb Callbacks) (*Session, error) {
	s := &Session{cb: cb}
	opts.Output = output
	opts.OnData = func(b []byte) { s.data = append(s.data, append([]byte(nil), b...)) }
	opts.OnHandshakeComplete = func() { s.handshake = true }
	var err error
	if client {
		s.conn, err = fibtls.NewDTLSClient(config, opts)
	} else {
		s.conn, err = fibtls.NewDTLSServer(config, opts)
	}
	if err != nil {
		return nil, err
	}
	s.timer = time.AfterFunc(time.Hour, s.onTimer)
	s.timer.Stop()
	return s, nil
}

// Start sends a client's first flight.
func (s *Session) Start() error {
	s.mu.Lock()
	err := s.conn.Start(time.Now())
	s.unlock()
	return err
}

// HandleDatagram takes a datagram from the peer. It decrypts b in place.
func (s *Session) HandleDatagram(b []byte) {
	s.mu.Lock()
	if !s.done {
		if err := s.conn.HandleDatagram(b, time.Now()); err != nil {
			s.end(err)
		}
	}
	s.unlock()
}

// Write sends b in one record.
func (s *Session) Write(b []byte) error {
	s.mu.Lock()
	defer s.unlock()
	if s.done {
		return fibtls.ErrDTLSClosed
	}
	return s.conn.Write(b)
}

// Close sends close_notify and ends the connection.
func (s *Session) Close() {
	s.mu.Lock()
	if !s.done {
		_ = s.conn.Close()
		s.end(fibtls.ErrDTLSClosed)
	}
	s.unlock()
}

// Describe reports what the handshake negotiated: the version and suite,
// the SRTP profile, the peer's certificate fingerprint, and the start of
// the SRTP keying material RFC 5764 exports, which is the same on both ends.
func (s *Session) Describe() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.conn.ConnectionState()
	profile := "none"
	if p, ok := s.conn.SelectedSRTPProfile(); ok {
		profile = ProfileName(p)
	}
	peer := "none"
	if len(st.PeerCertificates) > 0 {
		peer = fibtls.CertificateFingerprint(st.PeerCertificates[0].Raw)
	}
	km, err := s.conn.ExportKeyingMaterial("EXTRACTOR-dtls_srtp", nil, 16)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("%s %s, SRTP %s, peer sha-256 %s, exported %s",
		versionName(st.Version), stdtls.CipherSuiteName(st.CipherSuite), profile, peer, hex.EncodeToString(km))
}

// end records that the connection is over, to report once unlocked.
func (s *Session) end(err error) {
	s.done, s.err = true, err
}

// unlock rearms the timer, releases the mutex, and runs the callbacks the
// call produced.
func (s *Session) unlock() {
	if t, ok := s.conn.NextTimeout(); ok && !s.done {
		s.timer.Reset(time.Until(t))
	} else {
		s.timer.Stop()
	}
	handshake, data, err := s.handshake, s.data, s.err
	s.handshake, s.data, s.err = false, nil, nil
	s.mu.Unlock()
	if handshake && s.cb.OnHandshake != nil {
		s.cb.OnHandshake(s)
	}
	for _, b := range data {
		if s.cb.OnData != nil {
			s.cb.OnData(s, b)
		}
	}
	if err != nil && s.cb.OnClose != nil {
		s.cb.OnClose(s, err)
	}
}

func (s *Session) onTimer() {
	s.mu.Lock()
	if !s.done {
		s.conn.HandleTimeout(time.Now())
	}
	s.unlock()
}

// ProfileName names an SRTP protection profile.
func ProfileName(p uint16) string {
	switch p {
	case fibtls.SRTP_AES128_CM_HMAC_SHA1_80:
		return "SRTP_AES128_CM_HMAC_SHA1_80"
	case fibtls.SRTP_AES128_CM_HMAC_SHA1_32:
		return "SRTP_AES128_CM_HMAC_SHA1_32"
	case fibtls.SRTP_AEAD_AES_128_GCM:
		return "SRTP_AEAD_AES_128_GCM"
	case fibtls.SRTP_AEAD_AES_256_GCM:
		return "SRTP_AEAD_AES_256_GCM"
	}
	return fmt.Sprintf("0x%04x", p)
}

func versionName(v uint16) string {
	if v == fibtls.VersionDTLS12 {
		return "DTLS 1.2"
	}
	return fmt.Sprintf("version 0x%04x", v)
}
