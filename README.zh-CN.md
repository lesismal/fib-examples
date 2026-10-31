# fib 示例

[![CI](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml/badge.svg)](https://github.com/lesismal/fib-examples/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md)

[fib](https://github.com/lesismal/fib)（Go 的事件驱动网络库）的可运行示例：裸 TCP 与 UDP、DTLS、HTTP/1.1、
HTTP/2 与 HTTP/3、WebSocket、WebRTC、gRPC、arpc、SOCKS、同一端口上的多种协议、大文件上传，以及一个反向代理 / 网关。
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

每种协议一个目录，分为 `nontls` 与 `tls` 两个子目录（UDP 的加密版本是 DTLS，在 `tls/dtls` 中；HTTP/3 只有
`tls`：QUIC 总是加密的），各有一个 `server` 和一个 `client`：

| 目录 | 演示内容 | 默认地址 |
|------|----------|----------|
| [`tcp/nontls`](tcp/nontls) | 裸 TCP echo | `127.0.0.1:9000` |
| [`tcp/tls`](tcp/tls) | 同上，走 TLS：`fibtls.NewServer` 与 `fibtls.Dial` | `127.0.0.1:9443` |
| [`udp/nontls`](udp/nontls) | UDP echo | `127.0.0.1:9001` |
| [`tls/dtls`](tls/dtls) | UDP binding 上的 DTLS 1.2 echo，使用 fib 的 sans-IO `DTLSConn`：自签名证书按 fingerprint 校验，协商 SRTP profile 并打印两端导出的相同密钥材料；client 经 `Engine.OpenUDPPeer` 先开口 | `127.0.0.1:4444` |
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

### HTTP 单项功能

下面每个目录都是一个程序：自己启动 server，用 client 访问它，打印过程后退出；`-h` 列出参数，带 `-serve`
的可以一直运行，供 curl 访问。除非表中另有说明，它们走 HTTP/1.1，所演示的功能在 HTTP/2、HTTP/3 上用法相同。

| 目录 | 演示内容 |
|------|----------|
| [`http/duplex`](http/duplex) | `Context.ServeDuplex`：handler 同时读请求 body、写响应，走 h2c，每收到一行就立即回复，client 收到回复后才发下一行 |
| [`http/bodyfilter`](http/bodyfilter) | `OnBodyFilter`：gzip 中间件的 filter 保持流式输出（`BodyFlusher`），HEAD 或客户端断开时归还压缩器（`BodyAborter`）；`OnHeaderAsync` 与 `OnBodyFilterAsync`：在“插件”协程池里决定响应头和 body |
| [`http/sendfile`](http/sendfile) | `Context.SendFile`：任意大小的文件，不占 worker 等磁盘或客户端（sendfile，Linux 上加 `-tls` 走内核 TLS），Range 请求，以及 `SendFileStall` 放弃一个什么都不读的客户端 |
| [`http/nowait`](http/nowait) | `Context.WriteNoWait` 与 `OnWritable`：没有协程阻塞地流式输出响应，分别由快、慢两个客户端控速；`RespondCopy` 从池化的临时缓冲区应答；`OnFinish` 访问日志读回 `SetValue` 存的值 |
| [`http/requestbody`](http/requestbody) | body 到达之前能做的事：按路由 `LimitBody`，超过 1 MiB `StreamRequestBodyThreshold` 的 8 MiB 上传在请求头就被拒绝，带 `Expect: 100-continue` 时 body 一个字节都不发；`StreamRequestBodyAtHeader`：聊天式请求的每条消息在下一条发出前就到达 handler；上传中途断开，handler 的 `OnBody` 与中间件的 `OnCancel` 都收到；两个 `OnCancel` 回调都收到 `Client.Start` 的 `Call.Cancel` |
| [`http/client`](http/client) | fib 的 client：`ResponseHeaderTimeout`、`MaxPendingPerHost`（`ErrTooManyPending`）、在响应头到达时就开始接收的涓流 body 及 `HoldBody` 暂停、`BodyBytes`、以 `CloseWithTrailer` 结束的 `BodyWriter` 上传，以及解码 `gzip, chunked` 传输编码 |
| [`http/upgrade`](http/upgrade) | 协议切换的代理：后端切换后（`Client.StartUpgrade`）才对保留的请求做 Upgrade，中继由 `Tunnel.NotifySendable` 与 `HoldReads` 控速 |
| [`http/zerortt`](http/zerortt) | HTTP/1.1 的 TLS 1.3 0-RTT：`Config.EarlyData` 与 `ClientConfig.EarlyData`，恢复会话后的 GET 作为 early data 发出并带 `Early-Data: 1`，POST 等握手完成再发；`-suite` 选择 TLS 1.3 密码套件（`TLS13CipherSuites`，含 ChaCha20-Poly1305） |
| [`http2/settings`](http2/settings) | 两端的 `HTTP2Config`（窗口、帧大小、HPACK 表、自适应窗口），从 server 的 SETTINGS 帧读回；server 对静默 client 发 PING，client 对失联 server 发 PING（`ErrPingTimeout`）；PING 洪泛被 `ENHANCE_YOUR_CALM` 结束；`Engine.Close` 时发 GOAWAY |
| [`http3/altsvc`](http3/altsvc) | `http3.AltSvcClient`：先走 TCP 上的 HTTPS，看到 `Alt-Svc` 后改走 HTTP/3；UDP 不通的源站退回 TCP |
| [`http3/transport`](http3/transport) | HTTP/3 下面的 QUIC：`RetryHandshakes` 与 NEW_TOKEN、路径 MTU 探测把数据报从 1200 字节提到 1472 字节，以及 `TransportConfig`，从 server 收到的数据报上观察 |

### 一个引擎、一个端口上的多种协议

| 目录 | 演示内容 |
|------|----------|
| [`multiprotocol`](multiprotocol) | 一个引擎同时提供明文 HTTP（`:8090`）、带 HTTP/2 与 HTTP/3 的 HTTPS（`:8453`）、WebSocket（`:8091`）和裸 TCP echo（`:9010`、`:9011`），另有一个端口（`:9012`）由 `mux` 包按首字节区分 HTTPS、HTTP、h2c 与 echo |
| [`multiproto`](multiproto) | 用 `multiproto` 包在一个端口（`:9000`）上提供 HTTPS（HTTP/1.1 与 HTTP/2）、HTTP/1.1、h2c、WebSocket、gRPC、SOCKS5/4/4a、HTTP CONNECT 代理、arpc 与 TCP echo；它的 client 会逐一使用这些协议，server 的文件头注释列出了每种协议对应的 curl 或客户端命令 |
| [`socks`](socks) | 同一端口（`:1080`）上的 SOCKS5（含 UDP ASSOCIATE）、SOCKS4、SOCKS4a 与 HTTP CONNECT 代理；`-user`/`-pass` 开启认证，`-idle` 关闭长时间无数据的中继，client 经它抓取一个 URL |

[`udp/openpeer`](udp/openpeer) 单独演示 `Engine.OpenUDPPeer`：UDP binding 主动向某个地址发送，并在同一个端口、
同一个连接上收到回复。

### 引擎与 socket

| 目录 | 演示内容 |
|------|----------|
| [`tcp/limits`](tcp/limits) | `Config.MaxConns` 拒绝超出的连接（`Stats.ConnsRefused`），`MaxWriteQueueBytes` 与 `SetWriteQueueLimit` 关闭不读数据的对端（`ErrSendQueueFull`），`PollBatchSize`，以及 `Engine.Settings` / `LogSettings` |
| [`tcp/proxyprotocol`](tcp/proxyprotocol) | 用单独的 handler 读 PROXY protocol v1 头，`Connection.SetRemoteAddrPort`，再用 `SetHandler` 把连接交给 HTTP，后者看到的是负载均衡器后面的真实客户端地址 |
| [`tcp/dialfrom`](tcp/dialfrom) | `Engine.DialWith` 从指定的本地地址和端口范围拨出（`DialOptions.LocalAddr`、`PortMin`/`PortMax`），TCP 或 UDP（`-network`），用于防火墙只放行少数源端口的中继；范围内端口全被占用时以 `ErrNoLocalPort` 失败 |
| [`tcp/prefork`](tcp/prefork) | `prefork` 包多进程服务，`BalanceReusePort` 用 eBPF 轮询分配新连接；`-dial` 统计各进程接到的连接。prefork 与均衡器都需要 Linux（均衡器还需要 CAP_BPF 和 bpffs），其他系统上由单个进程服务 |
| [`udp/peers`](udp/peers) | `Connection.SetUDPIdleTimeout`：每个 UDP 对端各自的超时（短的、引擎默认的、不超时），以及监听 socket 上的 `SetDontFragment` |

### 网关

[`gateway`](gateway) 是一个反向代理：下游接 HTTP/1.1、HTTP/2、HTTP/3 和 WebSocket，按路径前缀转发到
`http://`、`https://`（ALPN 选 HTTP/2）或 `h3://` 上游，全程不阻塞引擎的 worker：请求 body、上游请求、
上游响应 body、WebSocket 拨号与消息都是回调。依次运行 `gateway/upstream`、`gateway/server`、`gateway/client`；
设计、取舍与测试见它的 [README](gateway/README.zh-CN.md)。

### WebRTC

fib 的 WebRTC 实现（见 fib 的 [WebRTC 文档](https://github.com/lesismal/fib/blob/main/docs/webrtc.zh-CN.md)），从浏览器
页面一直到下面的各层协议。浏览器示例监听 `:8080`：TCP 提供页面，UDP 供所有 PeerConnection 共用，在 `localhost` 上
直接可用；`-public-ip` 公布其它地址，`-tls` 用 HTTPS 提供页面（除 `localhost` 外，浏览器只在 HTTPS 下开放摄像头）。

| 目录 | 演示内容 |
|------|----------|
| [`webrtc`](webrtc) | 打开 `http://localhost:8080`：服务端回显的 data channel，以及摄像头画面发到服务端、再逐个 RTP 包送回（没有摄像头时只用 data channel）。页面像 WHIP 那样 POST 自己的 offer，服务端在共用的 `webrtc.Mux` 上用 ICE-lite PeerConnection 应答；`-turn :3478` 还会在同一个 engine 上运行 TURN 服务器，并通过 `/ice` 给页面发放限时凭据 |
| [`webrtc/sfu`](webrtc/sfu) | 一个浏览器发布摄像头和麦克风（WHIP），任意多个观看（WHEP）；服务端用每种媒体一个 `TrackLocal` 把 RTP 转发给所有观看者，并把他们的 PLI 转交发布者 |
| [`webrtc/datachannel`](webrtc/datachannel) | 两个 Go PeerConnection，不用浏览器：Mux 上的服务端经 fib/http 应答，客户端用自己的端口，在可靠、无序、部分可靠三种 channel 上收发消息；`-turn` 让所有流量经 TURN 中转 |
| [`webrtc/media`](webrtc/media) | Go 到 Go 的 RTP：合成的 VP8、Opus 包写入 `TrackLocal`，经 `OnTrack` 接收并按 SSRC 统计，再发一个 PLI，发送端在 `OnKeyFrameRequest` 中收到 |
| [`webrtc/turnserver`](webrtc/turnserver) | 独立的 TURN 服务器（`webrtc/turn/turnserver`）：固定用户或限时凭据的 secret、中继端口范围、公网 IP、允许中继到的对端；启动时打印一个可给页面用的 `RTCIceServer` |
| [`webrtc/stun`](webrtc/stun) | 应答 Binding 的 STUN 服务端，以及向 STUN 服务器（默认 `stun.l.google.com:19302`）查询自己 server-reflexive 地址的客户端，用到 `Engine.OpenUDPPeer` 和 `stun.Transaction` |

`webrtc/whip` 是浏览器示例和 `webrtc/datachannel` 共用的 WHIP/WHEP 信令。

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
├── tcp/ udp/ tls/ http/ http2/ http2only/ http3/ websocket/
├── webrtc/
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
