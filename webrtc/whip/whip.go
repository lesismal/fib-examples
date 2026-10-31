//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || windows

// Package whip is the HTTP signaling the WebRTC examples share: WHIP (RFC
// 9725) and WHEP, in which a browser POSTs its SDP offer and is answered,
// in the one response, with the server's SDP answer and the URL of the
// session it made. A DELETE on that URL ends the session.
//
// Neither side trickles candidates: the browser waits for its own to be
// gathered before it POSTs, and webrtc.Answer returns the answer with the
// server's. A server on a shared webrtc.Mux with ICE lite has its host
// candidates at once, so answering does not wait.
package whip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"mime"
	stdhttp "net/http"
	"sync"
	"time"

	fibhttp "github.com/lesismal/fib/http"
	"github.com/lesismal/fib/webrtc"
)

// AnswerTimeout bounds how long an offer waits for its answer.
const AnswerTimeout = 10 * time.Second

// Endpoint answers the offers POSTed to Path, each with a PeerConnection of
// its own, and keeps the sessions until they are DELETEd, fail or close.
type Endpoint struct {
	// Path is where offers are POSTed, "/whip"; each session's resource,
	// its Location, is Path + "/" + its id.
	Path string
	// MaxSessions caps the sessions held at once; an offer beyond it is
	// refused with 503. Zero means no cap.
	MaxSessions int
	// NewPeerConnection makes the PeerConnection that answers offer and
	// gives it its tracks and callbacks. It must not set
	// OnConnectionStateChange, which the Endpoint sets: OnStateChange is
	// told instead. An error refuses the offer with 400.
	NewPeerConnection func(offer string) (*webrtc.PeerConnection, error)
	// OnStateChange, if set, is told of each session's state.
	OnStateChange func(pc *webrtc.PeerConnection, state webrtc.PeerConnectionState)

	mu sync.Mutex
	// sessions holds each session's PeerConnection by its id, nil while
	// it is being made.
	sessions map[string]*webrtc.PeerConnection
	closed   bool
}

// Register routes POST Path and DELETE Path/{id} on r to the Endpoint.
func (e *Endpoint) Register(r *fibhttp.Router) {
	r.Post(e.Path, e.offer)
	r.Delete(e.Path+"/{id}", e.delete)
}

// Len reports how many sessions the Endpoint holds.
func (e *Endpoint) Len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sessions)
}

// Close closes every session.
func (e *Endpoint) Close() {
	e.mu.Lock()
	sessions := e.sessions
	e.sessions, e.closed = nil, true
	e.mu.Unlock()
	for _, pc := range sessions {
		if pc != nil {
			_ = pc.Close()
		}
	}
}

func (e *Endpoint) offer(c *fibhttp.Context) {
	if mt, _, _ := mime.ParseMediaType(c.Request.Header.Get("Content-Type")); mt != "application/sdp" {
		_ = c.Respond(stdhttp.StatusUnsupportedMediaType, "text/plain", []byte("the offer must be application/sdp\n"))
		return
	}
	offer, err := io.ReadAll(c.Request.Body)
	if err != nil {
		_ = c.Respond(stdhttp.StatusBadRequest, "text/plain", []byte(err.Error()+"\n"))
		return
	}
	// The session's place is taken first, so that an offer over the cap
	// makes no PeerConnection at all.
	id, ok := e.reserve()
	if !ok {
		_ = c.Respond(stdhttp.StatusServiceUnavailable, "text/plain", []byte("too many sessions\n"))
		return
	}
	pc, err := e.NewPeerConnection(string(offer))
	if err != nil {
		e.remove(id)
		_ = c.Respond(stdhttp.StatusBadRequest, "text/plain", []byte(err.Error()+"\n"))
		return
	}
	e.mu.Lock()
	closed := e.closed
	if !closed {
		e.sessions[id] = pc
	}
	e.mu.Unlock()
	if closed {
		_ = pc.Close()
		_ = c.Respond(stdhttp.StatusServiceUnavailable, "text/plain", []byte("shutting down\n"))
		return
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if e.OnStateChange != nil {
			e.OnStateChange(pc, s)
		}
		// A peer that went away without a DELETE fails once ICE consent
		// has gone unanswered for 30 seconds.
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			e.remove(id)
			_ = pc.Close()
		}
	})

	// The answer is ready as soon as it is set: a PeerConnection on a
	// shared Mux gathers its host candidates without asking anyone.
	ctx, cancel := context.WithTimeout(context.Background(), AnswerTimeout)
	defer cancel()
	answer, err := webrtc.Answer(ctx, pc, webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)})
	if err != nil {
		e.remove(id)
		_ = pc.Close()
		_ = c.Respond(stdhttp.StatusBadRequest, "text/plain", []byte(err.Error()+"\n"))
		return
	}
	_ = c.WriteResponse(fibhttp.Response{
		StatusCode: stdhttp.StatusCreated,
		Header: stdhttp.Header{
			"Content-Type": {"application/sdp"},
			"Location":     {e.Path + "/" + id},
		},
		Body: []byte(answer.SDP),
	})
}

func (e *Endpoint) delete(c *fibhttp.Context) {
	id := c.Param("id")
	e.mu.Lock()
	pc, ok := e.sessions[id]
	if ok && pc != nil {
		delete(e.sessions, id)
	}
	e.mu.Unlock()
	if pc == nil {
		_ = c.Respond(stdhttp.StatusNotFound, "text/plain", []byte("no such session\n"))
		return
	}
	_ = pc.Close()
	_ = c.Respond(stdhttp.StatusOK, "text/plain", nil)
}

// reserve takes a place for a new session, under a new id, if there is
// one; its PeerConnection is nil until it is made.
func (e *Endpoint) reserve() (string, bool) {
	var b [12]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.MaxSessions > 0 && len(e.sessions) >= e.MaxSessions {
		return "", false
	}
	if e.sessions == nil {
		e.sessions = make(map[string]*webrtc.PeerConnection)
	}
	e.sessions[id] = nil
	return id, true
}

func (e *Endpoint) remove(id string) {
	e.mu.Lock()
	delete(e.sessions, id)
	e.mu.Unlock()
}
