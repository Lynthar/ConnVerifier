# ConnVerifier

[![license](https://img.shields.io/github/license/Lynthar/ConnVerifier)](LICENSE)

用一个 Go 程序维持上万条空闲 TCP 长连接，测出 NAT 在多久后丢弃映射，附 RTT 百分位与掉线归因

[English](README.md) | 简体中文

> **施工中。** 下面写的它都能做，测试也是过的，但**还没有 release**，只能从源码构建。

运营商和家用路由器会悄悄忘掉空闲的 TCP 连接。这东西就是用来测「多久之后忘」的。
`connverifier serve` 把收到的字节原样回显；`connverifier capacity` 按你要求的数量把连接建起来，用很小的心跳维持着，
然后告诉你有多少条活下来、活了多久——每一次掉线都归因到超时、对端关闭，还是出错。
每秒输出 p50、p95、p99 RTT，连同各项连接计数。

纯标准库，零第三方依赖。

## 构建

还没有 release。需要 Go 1.26 以上：

```bash
go install github.com/Lynthar/ConnVerifier/cmd/connverifier@latest
```

或者克隆下来自己编：

```bash
git clone https://github.com/Lynthar/ConnVerifier.git
cd ConnVerifier
go build -o bin/connverifier ./cmd/connverifier
```

跑大规模之前，**两端**都要先把文件描述符上限抬上去——一条连接就是一个描述符。客户端的
上限或临时端口范围低于目标连接数，或者拨号因这两样用尽而失败，结果会标成 `INVALID`：那测到的
是客户机自己的上限，不是网络的。

## 用法

```bash
ulimit -n 20480
./bin/connverifier serve -addr :9000 -max-conns 20000 -idle-timeout 2m
```

另一头。`capacity` 开跑前会先说明要建多少连接，并在终端里请你确认——占满 NAT 表可能让
同一网络里的其他设备断网；`-yes` 跳过确认，标准输入不是终端时必须加：

```bash
./bin/connverifier capacity -addr <服务端>:9000 -clients 10000 -start-rate 500 -heartbeat 30s
```

想跑完自己停的无人值守：

```bash
./bin/connverifier capacity -addr <服务端>:9000 -clients 1000 -duration 1h -yes
```

运行中每秒往 stderr 打一行进度：

```
stats target=50 active=50 dial_attempts=50 connects=50 dial_errors=0 drops=0
  heartbeats=100 ack=100 rtt_p50=159µs rtt_p95=255µs rtt_p99=255µs
```

结束时结果写到 stdout，默认是文本，`-format json` 则输出 JSON。节选：

```
TCP 长连接容量：注意（WARN）
节点：192.0.2.10:9000 · IPv4 · TCP
观测
  成功建连：73
  掉线：31
  掉线（超时无回应）：3
  回显往返时延 p95：309 µs（40 个样本）
  回显往返时延 p99：样本不足（40 个样本）
推断
  - 3 条连接没有收到关闭或重置就不再回应，这是 NAT 或其他中间设备悄悄丢弃连接状态的典型表现。
未能证明
  - 无法区分节点满载后的拒绝与网络造成的中断，两者在客户端看来是一样的。
```

文本语言跟随 `LC_ALL`、`LC_MESSAGES` 或 `LANG`，支持中文和英文，其余一律用中文；
`-lang en` 或 `-lang zh-CN` 可以覆盖。每个状态和数字是什么意思、什么时候不该信，见
[docs/methods](docs/methods/README.md)（英文）。

全部走旗标，没有配置文件，没有环境变量。`capacity`：`-addr`、`-clients`（1000）、
`-start-rate`（100）、`-heartbeat`（30s）、`-dial-timeout`（5s）、`-io-timeout`（5s）、
`-min-backoff`（500ms）、`-max-backoff`（1m）、`-tcp-keepalive`（0）、
`-duration`（0＝直到中断）、`-log-drops`、`-format`（text）、`-lang`、`-yes`。`serve`：`-addr`（:9000）、`-max-conns`（10000）、
`-idle-timeout`（2m）、`-tcp-keepalive`（0）、`-log-connections`。零或负数一律表示
「禁用 / 无限」。

**`-tcp-keepalive` 默认 0 是有意的**——内核 keepalive 会不断刷新 NAT 映射，那样测出来的
结果永远是「NAT 很稳」。

## 能力边界

- **只测 TCP。** 没有 UDP、没有 STUN、没有 DNS、不测带宽。
- **JSON 的 schema 还是 `v0`。** 不同构建之间仍可能变；要解析就固定一个构建，别解析文本。
- **退出码不评判网络质量。** 0 表示跑完了，不管各项状态如何；1 表示有检查没拿到有效
  测量（状态 `ERROR`，比如节点连不上），或工具自己出错；2 表示什么都没跑：参数不对，或
  没有确认。要按网络质量设门禁，读 JSON。
- **掉线是靠心跳发现的，不是即时的。** 两次心跳之间到达的 `FIN` 要等下一拍才被看见，
  所以报出来的存活时长是上界，`active` 会短暂偏高。
- **服务端满载拒绝会伪装成客户端故障。** TCP 握手在拒绝之前就完成了，表现为
  dial 成功、首个心跳失败、而 `dial_errors` 是 0。要对照服务端的 `rejected=` 才分得清。
- **单个源 IP 对单一目标 `ip:port` 只有约 28000 个临时端口**，这限制了一台客户机能
  维持多少连接。

## 与上游的区别

本仓库 fork 自 [codeberg.org/woq/ConnVerifier](https://codeberg.org/woq/ConnVerifier)。
我在它基础上加的是：单个 `connverifier` 程序与测试、RTT 百分位、掉线归因、带抖动的指数退避重连、
有并发上限和空闲回收的服务端，以及给无人值守长跑用的 `-duration`。

## 安全

**服务端是一个不做鉴权的回显端点。** 它没有限速，所以任何能连上的人都可以把
`-max-conns` 占满、把真正的客户端挡在外面；而且它会把收到的任意字节原样回显出去。
请用防火墙只放行你测试用的那个客户端，**别把它挂在公网上**。

## 许可证

MIT —— 见 [LICENSE](LICENSE)。版权行写的是「2025 woq」，即上游作者。
