# fib examples

[![CI](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml/badge.svg)](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[中文](README.zh-CN.md)

Runnable examples for [fib](https://github.com/lesismal/fib), the event-driven network library for Go:
raw TCP and UDP, HTTP/1.1, HTTP/2 and HTTP/3, WebSocket, gRPC, arpc, SOCKS, several protocols on one
port, large uploads, and a reverse proxy / gateway. Every example is a small program with a test that
runs it, and the module depends on fib the way any application would.

## Running them

Go 1.27 or later. Clone the repository and run each example from its root:

```sh
git clone https://github.com/lesismal/fib-examples && cd fib-examples
go run ./tcp/nontls/server     # in one terminal
go run ./tcp/nontls/client     # in another
```

Start a server first, then its client in another terminal. An echo client sends 5 messages by default
(`-n` changes it), prints each echo and exits; a server runs until Ctrl-C. `-addr` (TCP, UDP, arpc,
gRPC) or `-url` (HTTP, WebSocket) points them elsewhere, and `-h` lists every flag.

## The examples

### Echo servers, one per protocol

Each protocol has a directory, split into `nontls` and `tls` (UDP has only `nontls`, since fib has no
DTLS; HTTP/3 has only `tls`, since QUIC is always encrypted), each with a `server` and a `client`:

| Directory | What it shows | Default address |
|-----------|---------------|-----------------|
| [`tcp/nontls`](tcp/nontls) | a raw TCP echo | `127.0.0.1:9000` |
| [`tcp/tls`](tcp/tls) | the same over TLS, with `fibtls.NewServer` and `fibtls.Dial` | `127.0.0.1:9443` |
| [`udp/nontls`](udp/nontls) | a UDP echo | `127.0.0.1:9001` |
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
| [`http3/upload`](http3/upload) | the same endpoints over HTTP/3, paced by QUIC flow control; use `-mode resume` for large files, as fib's HTTP/3 client holds a request body in memory |
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

### Many protocols, one engine or one port

| Directory | What it shows |
|-----------|---------------|
| [`multiprotocol`](multiprotocol) | one engine serving plain HTTP (`:8090`), HTTPS with HTTP/2 and HTTP/3 (`:8453`), WebSocket (`:8091`) and a raw TCP echo (`:9010`, `:9011`), plus one port (`:9012`) where package `mux` tells HTTPS, HTTP, h2c and the echo apart by their first bytes |
| [`multiproto`](multiproto) | package `multiproto` on one port (`:9000`): HTTPS (HTTP/1.1 and HTTP/2), HTTP/1.1, h2c, WebSocket, gRPC, SOCKS5/4/4a, an HTTP CONNECT proxy, arpc and a TCP echo; its client speaks every one of them, and the server's doc comment lists a curl or client command for each |
| [`socks`](socks) | a SOCKS5 (with UDP ASSOCIATE), SOCKS4 and SOCKS4a proxy and an HTTP CONNECT proxy on one port (`:1080`); `-user`/`-pass` require authentication, and the client fetches a URL through it |

### Gateway

[`gateway`](gateway) is a reverse proxy that accepts HTTP/1.1, HTTP/2, HTTP/3 and WebSocket and forwards
by path prefix to `http://`, `https://` (HTTP/2 by ALPN) or `h3://` upstreams, with nothing blocking an
engine worker: request bodies, upstream requests, upstream response bodies, WebSocket dials and messages
are all callbacks. Run `gateway/upstream`, `gateway/server` and `gateway/client` in that order; see its
[README](gateway/README.md) for the design, the trade-offs and the tests.

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
├── tcp/ udp/ http/ http2/ http2only/ http3/ websocket/
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
