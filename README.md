# fib examples

[![CI](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml/badge.svg)](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[中文](README.zh-CN.md)

Runnable examples for [fib](https://github.com/lesismal/fib), the event-driven network library for Go:
raw TCP and UDP, DTLS, HTTP/1.1, HTTP/2 and HTTP/3, WebSocket, WebRTC, gRPC, arpc, SOCKS, several protocols on
one port, large uploads, and a reverse proxy / gateway. Every example is a small program with a test that
runs it, and the module depends on fib the way any application would.

## Running them

Go 1.27.3 or later. Clone the repository and run each example from its root:

```sh
git clone https://github.com/lesismal/fib-examples && cd fib-examples
go run ./tcp/nontls/server     # in one terminal
go run ./tcp/nontls/client     # in another
```

Start a server first, then its client in another terminal. An echo client sends 5 messages by default
(`-n` changes it), prints each echo and exits; a server runs until Ctrl-C. `-addr` (TCP, UDP, arpc,
gRPC) or `-url` (HTTP, WebSocket) points them elsewhere, and `-h` lists every flag. The examples
build their engines with `example.EngineConfig()`, which is `fib.DefaultConfig()` with fib's startup
settings list off, so each program prints only its own lines; call `Engine.LogSettings()` to print
the list.

## The examples

### Echo servers, one per protocol

Each protocol has a directory, split into `nontls` and `tls` (UDP's encrypted twin is DTLS, in `tls/dtls`; HTTP/3 has only `tls`, since QUIC is always encrypted), each with a `server` and a `client`:

| Directory | What it shows | Default address |
|-----------|---------------|-----------------|
| [`tcp/nontls`](tcp/nontls) | a raw TCP echo | `127.0.0.1:9000` |
| [`tcp/tls`](tcp/tls) | the same over TLS, with `fibtls.NewServer` and `fibtls.Dial` | `127.0.0.1:9443` |
| [`udp/nontls`](udp/nontls) | a UDP echo | `127.0.0.1:9001` |
| [`tls/dtls`](tls/dtls) | a DTLS 1.2 echo on UDP bindings with fib's sans-IO `DTLSConn`: self-signed certificates checked by fingerprint, an SRTP profile and the keying material both ends export; the client speaks first through `Engine.OpenUDPPeer` | `127.0.0.1:4444` |
| [`http/nontls`](http/nontls) | an HTTP/1.1 echo; `-dir` also serves a directory under `/files/` through `net/http`'s `FileServer` (Range, conditional requests, sendfile) | `127.0.0.1:8080` |
| [`http/tls`](http/tls) | HTTPS, with HTTP/2 or HTTP/1.1 by ALPN; the client's `-http1` stays on HTTP/1.1 | `127.0.0.1:8443` |
| [`http3/tls`](http3/tls) | HTTP/3 over UDP, and HTTPS on the TCP port of the same number, whose responses carry `Alt-Svc` | `127.0.0.1:8445` |
| [`websocket/nontls`](websocket/nontls) | a WebSocket echo; `-compress` turns on permessage-deflate | `127.0.0.1:8081` |
| [`websocket/tls`](websocket/tls) | the same over `wss://` | `127.0.0.1:8444` |

fib's CI runs the [Autobahn](https://github.com/crossbario/autobahn-testsuite) WebSocket test suite
against both WebSocket servers.

### RPC

| Directory | What it shows | Default address |
|-----------|---------------|-----------------|
| [`arpc`](arpc) | fib/arpc: a call, an asynchronous call, a typed service and a stream | `127.0.0.1:8888` |
| [`grpc`](grpc) | fib/grpc: a unary call, a call the server refuses, and a server-streaming call; [`grpc/greeter`](grpc/greeter) is the service written the way `protoc-gen-go-grpc` generates one, with JSON messages so that nothing needs protoc | `127.0.0.1:50051` |

### HTTP

| Directory | What it shows |
|-----------|---------------|
| [`http/router`](http/router) | `fibhttp.Router`: routes with parameters and regular expressions, a `Route` with middleware of its own, a mounted sub-router, 404 and 405; the curl commands are in its doc comment |
| [`http/upload`](http/upload) | large HTTP/1 request bodies with `StreamRequestBody`, in flat memory: `POST /upload` (`SaveBody`), resumable `PUT /resume` (`SaveBodyResumable`) and `POST /echo` (`OnBody`); `mkfile` writes a reproducible test file, and `client -mode upload\|resume\|echo` streams it and checks the SHA-256 |
| [`http2/upload`](http2/upload) | the same endpoints over HTTP/2 (TLS, `HTTP2Only`), paced by the stream's flow-control window |
| [`http3/upload`](http3/upload) | the same endpoints over HTTP/3, paced by QUIC flow control; request bodies stream from disk, and `-mode echo` holds the response whole, so use `-mode resume` for a large echo |
| [`http2only`](http2only) | an HTTP/2-only echo server (h2c, or h2 with `-tls`) that ends a connection without the HTTP/2 preface with GOAWAY; fib's CI runs [h2spec](https://github.com/summerwind/h2spec) against it |

An upload session, for example:

```sh
go run ./http/upload/mkfile -size 1GiB -o /tmp/big.bin
go run ./http/upload/server -dir /tmp/uploads
go run ./http/upload/client -mode resume -file /tmp/big.bin -name part.bin -max-chunks 3
go run ./http/upload/client -mode resume -file /tmp/big.bin -name part.bin   # picks up where it stopped
```

The handler is shared by all three protocols ([`http/upload/service`](http/upload/service)), and so is
the client logic ([`http/upload/uploadclient`](http/upload/uploadclient)): the body arrives through
`Context.OnBody` whatever carried it, and the back-pressure comes from TCP, the HTTP/2 window or QUIC.

### HTTP, one feature at a time

Each of these is one program that starts its own server, runs a client against it, prints what happened and
exits; `-h` lists its flags, and those with `-serve` keep serving for curl. Unless a row says otherwise they
speak HTTP/1.1, and what they show works the same over HTTP/2 and HTTP/3.

| Directory | What it shows |
|-----------|---------------|
| [`http/duplex`](http/duplex) | `Context.ServeDuplex`: a handler that reads its request body and writes its response at once, over h2c, answering each line before the client sends the next |
| [`http/bodyfilter`](http/bodyfilter) | `OnBodyFilter`: a gzip middleware whose filter streams (`BodyFlusher`) and gives its compressor back on HEAD or a lost client (`BodyAborter`); `OnHeaderAsync` and `OnBodyFilterAsync`, a header and a body decided on goroutines of a "plugin" pool |
| [`http/sendfile`](http/sendfile) | `Context.SendFile`, a file of any size with no worker waiting for the disk or the client (sendfile, kernel TLS on Linux with `-tls`), a byte range, and `SendFileStall` giving up on a client that reads nothing |
| [`http/nowait`](http/nowait) | `Context.WriteNoWait` and `OnWritable`: a response streamed without a goroutine waiting, paced by a fast and a slow client; `RespondCopy` from a pooled scratch buffer; `SetValue` read back by an `OnFinish` access log |
| [`http/requestbody`](http/requestbody) | a request's body before it arrives: `LimitBody` per route, an 8 MiB upload past the 1 MiB `StreamRequestBodyThreshold` refused from its header with `Expect: 100-continue` before any of it is sent; `StreamRequestBodyAtHeader`, a chat whose messages each reach the handler before the next is sent; an upload dropped part way, heard by the handler's `OnBody` and a middleware's `OnCancel`; two `OnCancel` callbacks hearing `Client.Start`'s `Call.Cancel` |
| [`http/client`](http/client) | fib's client: `ResponseHeaderTimeout`, `MaxPendingPerHost` (`ErrTooManyPending`), a trickling body taken at its header and paused with `HoldBody`, `BodyBytes`, a `BodyWriter` upload ended by `CloseWithTrailer`, and a `gzip, chunked` transfer coding decoded |
| [`http/upgrade`](http/upgrade) | a proxy for a protocol switch: the retained request upgraded only once the backend has switched (`Client.StartUpgrade`), and a relay paced by `Tunnel.NotifySendable` and `HoldReads` |
| [`http/zerortt`](http/zerortt) | TLS 1.3 0-RTT for HTTP/1.1: `Config.EarlyData` and `ClientConfig.EarlyData`, resumed GETs sent as early data with `Early-Data: 1`, a POST that waits for the handshake; `-suite` picks the TLS 1.3 cipher suite (`TLS13CipherSuites`, ChaCha20-Poly1305 among them) |
| [`http2/settings`](http2/settings) | `HTTP2Config` on both ends (windows, frame size, HPACK table, adaptive windows) read back from the server's SETTINGS; the server's PING of a quiet client and the client's of a dead server (`ErrPingTimeout`); a PING flood ended by `ENHANCE_YOUR_CALM`; GOAWAY on `Engine.Close` |
| [`http3/altsvc`](http3/altsvc) | `http3.AltSvcClient`: HTTPS over TCP until `Alt-Svc` advertises HTTP/3, and back to TCP for an origin whose UDP is blocked |
| [`http3/transport`](http3/transport) | the QUIC under HTTP/3: `RetryHandshakes` and NEW_TOKEN, path MTU discovery growing datagrams from 1200 to 1472 bytes, and `TransportConfig`, watched from the datagrams the server receives |

### Many protocols, one engine or one port

| Directory | What it shows |
|-----------|---------------|
| [`multiprotocol`](multiprotocol) | one engine serving plain HTTP (`:8090`), HTTPS with HTTP/2 and HTTP/3 (`:8453`), WebSocket (`:8091`) and a raw TCP echo (`:9010`, `:9011`), plus one port (`:9012`) where package `mux` tells HTTPS, HTTP, h2c and the echo apart by their first bytes |
| [`multiproto`](multiproto) | package `multiproto` on one port (`:9000`): HTTPS (HTTP/1.1 and HTTP/2), HTTP/1.1, h2c, WebSocket, gRPC, SOCKS5/4/4a, an HTTP CONNECT proxy, arpc and a TCP echo; its client speaks every one of them, and the server's doc comment lists a curl or client command for each |
| [`socks`](socks) | a SOCKS5 (with UDP ASSOCIATE), SOCKS4 and SOCKS4a proxy and an HTTP CONNECT proxy on one port (`:1080`); `-user`/`-pass` require authentication, `-idle` closes relays that carry nothing, and the client fetches a URL through it |

[`udp/openpeer`](udp/openpeer) shows `Engine.OpenUDPPeer` on its own: a UDP binding that speaks first to an
address and hears the reply on the same port and connection.

### Engine and sockets

| Directory | What it shows |
|-----------|---------------|
| [`tcp/limits`](tcp/limits) | `Config.MaxConns` refusing connections past it (`Stats.ConnsRefused`), `MaxWriteQueueBytes` and `SetWriteQueueLimit` closing a peer that reads none of a flood (`ErrSendQueueFull`), `PollBatchSize`, and `Engine.Settings` / `LogSettings` |
| [`tcp/proxyprotocol`](tcp/proxyprotocol) | a PROXY protocol v1 header read by a handler of its own, `Connection.SetRemoteAddrPort`, and `SetHandler` handing the connection to HTTP, which then sees the client behind a load balancer |
| [`tcp/dialfrom`](tcp/dialfrom) | `Engine.DialWith` from a chosen local address and port range (`DialOptions.LocalAddr`, `PortMin`/`PortMax`), over TCP or UDP (`-network`), as a relay behind a firewall that admits a few source ports must; a range whose ports are all in use fails with `ErrNoLocalPort` |
| [`tcp/prefork`](tcp/prefork) | package `prefork` serving from several processes, and `BalanceReusePort` spreading connections round-robin with eBPF; `-dial` counts who answered. Prefork and the balancer need Linux (the balancer also CAP_BPF and bpffs); elsewhere one process serves |
| [`udp/peers`](udp/peers) | `Connection.SetUDPIdleTimeout`, a timeout per UDP peer (short, the engine's, or none), and `SetDontFragment` on the listener's socket |

### Gateway

[`gateway`](gateway) is a reverse proxy that accepts HTTP/1.1, HTTP/2, HTTP/3 and WebSocket and forwards
by path prefix to `http://`, `https://` (HTTP/2 by ALPN) or `h3://` upstreams, with nothing blocking an
engine worker: request bodies, upstream requests, upstream response bodies, WebSocket dials and messages
are all callbacks. Run `gateway/upstream`, `gateway/server` and `gateway/client` in that order; see its
[README](gateway/README.md) for the design, the trade-offs and the tests.

### WebRTC

fib's WebRTC stack (see fib's [WebRTC document](https://github.com/lesismal/fib/blob/main/docs/webrtc.md)) from a browser
page down to the protocols under it. The browser examples listen on `:8080`, TCP for the page and UDP for every
PeerConnection, and work on `localhost` as they are; `-public-ip` advertises another address, and `-tls` serves the
page over HTTPS, which browsers require for the camera anywhere but `localhost`.

| Directory | What it shows |
|-----------|---------------|
| [`webrtc`](webrtc) | open `http://localhost:8080`: a data channel the server echoes, and the camera sent to the server and back RTP packet by RTP packet (or the data channel alone, with no camera). The page POSTs its offer as WHIP does, and the server answers with an ICE-lite PeerConnection on a shared `webrtc.Mux`; `-turn :3478` also runs a TURN server on the engine and hands the page ephemeral credentials from `/ice` |
| [`webrtc/sfu`](webrtc/sfu) | one browser publishes its camera and microphone (WHIP), any number watch (WHEP); the server forwards RTP to every viewer through one `TrackLocal` per kind and passes their PLIs to the publisher |
| [`webrtc/datachannel`](webrtc/datachannel) | two Go PeerConnections, no browser: a server on a Mux answering over fib/http, a client on a port of its own, and messages on reliable, unordered and partially reliable channels; `-turn` relays everything through a TURN server |
| [`webrtc/media`](webrtc/media) | Go to Go RTP: synthetic VP8 and Opus written to `TrackLocal`s, received through `OnTrack` and counted per SSRC, and a PLI that reaches the sender's `OnKeyFrameRequest` |
| [`webrtc/turnserver`](webrtc/turnserver) | a standalone TURN server (`webrtc/turn/turnserver`) over UDP, and over TCP and TLS with `-tcp` / `-tls` (`ListenTCP`, `ListenTLS`): fixed users or an ephemeral-credential secret, relay port range, public IP and the peers it may relay to; it prints an `RTCIceServer` for a page |
| [`webrtc/stun`](webrtc/stun) | a STUN server answering Binding, and a client that asks one (`stun.l.google.com:19302` by default) for its server-reflexive address, with `Engine.OpenUDPPeer` and a `stun.Transaction` |

`webrtc/whip` is the WHIP/WHEP signaling the browser examples and `webrtc/datachannel` share.

## TLS certificates

A TLS server run without `-cert` and `-key` issues itself a self-signed certificate for `localhost` and
`127.0.0.1`, and writes the certificate (never the key) to `fib-example-cert.pem` in the temporary
directory. Its client trusts that file by default (`-ca` names another), so it verifies the server
exactly as a real deployment would; `-insecure` skips the check. Package [`certs`](certs) does this for
every TLS example.

How TLS is added differs by protocol: TCP, HTTP and WebSocket servers wrap their handler with
`fibtls.NewServer`, and their clients use `fibtls.Dial`, `ClientConfig.TLSConfig` and
`DialerConfig.TLSConfig`. QUIC does its own TLS, so an HTTP/3 server hands its TLS config to
`http3.NewHandler`, and its client sets `http3.ClientConfig.TLSConfig`.

## Layout

```text
fib-examples/
├── example/       the plumbing every example shares (start, serve until Ctrl-C, stop), so each shows only its protocol
├── certs/         self-signed certificates for the TLS examples
├── tcp/ udp/ tls/ http/ http2/ http2only/ http3/ websocket/
├── webrtc/
├── arpc/ grpc/
├── multiprotocol/ multiproto/ socks/
└── gateway/
```

## Tests

Every example has a test that starts its server on a free port and drives it with its client:

```sh
go vet ./...
go test -race ./...
```

To try the examples against a local fib checkout instead of the version `go.mod` pins, add a workspace
next to both:

```sh
go work init . ../fib
```

## License

MIT, see [LICENSE](LICENSE).
