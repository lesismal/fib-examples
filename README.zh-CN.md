# fib 示例

[![CI](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml/badge.svg)](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md)

[fib](https://github.com/lesismal/fib)（Go 的事件驱动网络库）的可运行示例：裸 TCP 与 UDP、HTTP/1.1、
HTTP/2 与 HTTP/3、WebSocket、gRPC、arpc、SOCKS、同一端口上的多种协议、大文件上传，以及一个反向代理 / 网关。
每个示例都是一个小程序，并配有运行它的测试；本模块像普通应用一样依赖 fib。

## 运行

需要 Go 1.27.3 及以上。克隆仓库后在根目录运行各个示例：

```sh
git clone https://github.com/lesismal/fib-examples && cd fib-examples
go run ./tcp/nontls/server     # 一个终端
go run ./tcp/nontls/client     # 另一个终端
```

先启动 server，再在另一个终端运行对应的 client。echo client 默认发送 5 条消息（`-n` 修改），逐条打印回显后
退出；server 按 Ctrl-C 退出。地址用 `-addr`（TCP、UDP、arpc、gRPC）或 `-url`（HTTP、WebSocket）修改，
`-h` 列出全部参数。示例用 `example.EngineConfig()` 创建引擎：它是 `fib.DefaultConfig()`，只关掉了 fib
启动时的设置清单，让每个程序只打印自己的输出；需要时调用 `Engine.LogSettings()` 打印。

## 示例一览

### 各协议的 echo server

每种协议一个目录，分为 `nontls` 与 `tls` 两个子目录（UDP 只有 `nontls`：fib 不内置 DTLS；HTTP/3 只有
`tls`：QUIC 总是加密的），各有一个 `server` 和一个 `client`：

| 目录 | 演示内容 | 默认地址 |
|------|----------|----------|
| [`tcp/nontls`](tcp/nontls) | 裸 TCP echo | `127.0.0.1:9000` |
| [`tcp/tls`](tcp/tls) | 同上，走 TLS：`fibtls.NewServer` 与 `fibtls.Dial` | `127.0.0.1:9443` |
| [`udp/nontls`](udp/nontls) | UDP echo | `127.0.0.1:9001` |
| [`http/nontls`](http/nontls) | HTTP/1.1 echo；`-dir` 另用 `net/http` 的 `FileServer` 在 `/files/` 下提供该目录的文件（Range、条件请求、sendfile） | `127.0.0.1:8080` |
| [`http/tls`](http/tls) | HTTPS，按 ALPN 走 HTTP/2 或 HTTP/1.1；client 的 `-http1` 固定用 HTTP/1.1 | `127.0.0.1:8443` |
| [`http3/tls`](http3/tls) | UDP 上的 HTTP/3，同端口号的 TCP 上提供 HTTPS，响应带 `Alt-Svc` | `127.0.0.1:8445` |
| [`websocket/nontls`](websocket/nontls) | WebSocket echo；`-compress` 开启 permessage-deflate | `127.0.0.1:8081` |
| [`websocket/tls`](websocket/tls) | 同上，走 `wss://` | `127.0.0.1:8444` |

fib 的 CI 用这两个 WebSocket server 跑 [Autobahn](https://github.com/crossbario/autobahn-testsuite) 测试套件。

### RPC

| 目录 | 演示内容 | 默认地址 |
|------|----------|----------|
| [`arpc`](arpc) | fib/arpc：同步调用、异步调用、类型化的服务与 stream | `127.0.0.1:8888` |
| [`grpc`](grpc) | fib/grpc：unary 调用、被服务端拒绝的调用、服务端流式调用；[`grpc/greeter`](grpc/greeter) 按 `protoc-gen-go-grpc` 生成代码的样子手写，消息用 JSON，因此不需要 protoc | `127.0.0.1:50051` |

### HTTP

| 目录 | 演示内容 |
|------|----------|
| [`http/router`](http/router) | `fibhttp.Router`：带参数与正则的路由、带自己中间件的 `Route`、`Mount` 的子 Router，以及 404/405；curl 命令见文件头注释 |
| [`http/upload`](http/upload) | 用 `StreamRequestBody` 接收 HTTP/1 大 body，内存不随 body 增长：`POST /upload`（`SaveBody`）、可断点续传的 `PUT /resume`（`SaveBodyResumable`）、`POST /echo`（`OnBody`）；`mkfile` 生成可复现的测试文件，`client -mode upload\|resume\|echo` 流式发送并校验 SHA-256 |
| [`http2/upload`](http2/upload) | 同一套接口走 HTTP/2（TLS，`HTTP2Only`），由流的流控窗口施加背压 |
| [`http3/upload`](http3/upload) | 同一套接口走 HTTP/3，由 QUIC 流控施加背压；请求 body 从磁盘流式发送，`-mode echo` 会把响应整个留在内存，大文件的 echo 请用 `-mode resume` 分块发送 |
| [`http2only`](http2only) | 只说 HTTP/2 的 echo server（h2c，或加 `-tls` 走 h2），不以 HTTP/2 preface 开头的连接用 GOAWAY 结束；fib 的 CI 用 [h2spec](https://github.com/summerwind/h2spec) 测它 |

上传示例的完整流程：

```sh
go run ./http/upload/mkfile -size 1GiB -o /tmp/big.bin
go run ./http/upload/server -dir /tmp/uploads
go run ./http/upload/client -mode resume -file /tmp/big.bin -name part.bin -max-chunks 3
go run ./http/upload/client -mode resume -file /tmp/big.bin -name part.bin   # 从上次停下的位置继续
```

三种协议共用同一个 handler（[`http/upload/service`](http/upload/service)）和同一套客户端逻辑
（[`http/upload/uploadclient`](http/upload/uploadclient)）：不论哪种协议，body 都经 `Context.OnBody` 到达，
背压分别来自 TCP、HTTP/2 窗口和 QUIC 流控。

### 一个引擎、一个端口上的多种协议

| 目录 | 演示内容 |
|------|----------|
| [`multiprotocol`](multiprotocol) | 一个引擎同时提供明文 HTTP（`:8090`）、带 HTTP/2 与 HTTP/3 的 HTTPS（`:8453`）、WebSocket（`:8091`）和裸 TCP echo（`:9010`、`:9011`），另有一个端口（`:9012`）由 `mux` 包按首字节区分 HTTPS、HTTP、h2c 与 echo |
| [`multiproto`](multiproto) | 用 `multiproto` 包在一个端口（`:9000`）上提供 HTTPS（HTTP/1.1 与 HTTP/2）、HTTP/1.1、h2c、WebSocket、gRPC、SOCKS5/4/4a、HTTP CONNECT 代理、arpc 与 TCP echo；它的 client 会逐一使用这些协议，server 的文件头注释列出了每种协议对应的 curl 或客户端命令 |
| [`socks`](socks) | 同一端口（`:1080`）上的 SOCKS5（含 UDP ASSOCIATE）、SOCKS4、SOCKS4a 与 HTTP CONNECT 代理；`-user`/`-pass` 开启认证，client 经它抓取一个 URL |

### 网关

[`gateway`](gateway) 是一个反向代理：下游接 HTTP/1.1、HTTP/2、HTTP/3 和 WebSocket，按路径前缀转发到
`http://`、`https://`（ALPN 选 HTTP/2）或 `h3://` 上游，全程不阻塞引擎的 worker：请求 body、上游请求、
上游响应 body、WebSocket 拨号与消息都是回调。依次运行 `gateway/upstream`、`gateway/server`、`gateway/client`；
设计、取舍与测试见它的 [README](gateway/README.zh-CN.md)。

## TLS 证书

TLS server 未指定 `-cert` 和 `-key` 时，会为 `localhost` 与 `127.0.0.1` 签发一张自签名证书，并把证书（不含
私钥）写到临时目录的 `fib-example-cert.pem`。对应的 client 默认信任这个文件（`-ca` 指定其他文件），因此会像
正式部署一样校验服务端证书；`-insecure` 跳过校验。所有 TLS 示例都由 [`certs`](certs) 包完成这件事。

不同协议加 TLS 的方式不同：TCP、HTTP、WebSocket server 用 `fibtls.NewServer` 包装原本的 handler，client
分别用 `fibtls.Dial`、`ClientConfig.TLSConfig` 和 `DialerConfig.TLSConfig`。QUIC 自己完成 TLS，所以 HTTP/3
server 把 TLS 配置直接交给 `http3.NewHandler`，client 设置 `http3.ClientConfig.TLSConfig`。

## 目录结构

```text
fib-examples/
├── example/       所有示例共用的启动、运行到 Ctrl-C、停止等代码，让每个示例只展示自己的协议
├── certs/         TLS 示例用的自签名证书
├── tcp/ udp/ http/ http2/ http2only/ http3/ websocket/
├── arpc/ grpc/
├── multiprotocol/ multiproto/ socks/
└── gateway/
```

## 测试

每个示例都有测试：在空闲端口上启动它的 server，再用它的 client 访问：

```sh
go vet ./...
go test -race ./...
```

想用本地的 fib 代码而不是 `go.mod` 锁定的版本运行示例时，在两者旁边建一个 workspace：

```sh
go work init . ../fib
```

## 许可证

MIT，见 [LICENSE](LICENSE)。
