# ConnVerifier

[![license](https://img.shields.io/github/license/Lynthar/ConnVerifier)](LICENSE)

用一个 Go 程序维持上万条空闲 TCP 长连接，测出 NAT 在多久后丢弃映射，附 RTT 百分位与掉线归因

[English](README.md) | 简体中文

> **施工中。** 下面写的它都能做，测试也是过的，但**还没有 release**，只能从源码构建。

运营商和家用路由器会悄悄忘掉空闲的 TCP 连接。这东西就是用来测「多久之后忘」的。
你在自己控制的服务器上跑一个**节点**，再给要测的人发**邀请串**；`connverifier capacity`
按你要求的数量向节点建连，用很小的心跳维持着，然后告诉你有多少条活下来、活了多久——
每一次掉线都归因到悄悄超时、被关闭、被重置，还是节点说明原因后关闭。节点满了会直说，
结果也会写明上限是节点定的，不是你的网络。

`connverifier check` 在同一条路径上并排测 UDP 和 TCP 的往返时延及其变化，以及 UDP 丢包——
不用对时，也能把丢包拆成去程和回程。

单个程序，一个依赖（`golang.org/x/term`）。

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

## 用法

在服务器上起节点，给每个要来测的人各建一张邀请串。节点只开一个 TCP 端口（默认 7443），
所有 TCP 流量都走它；UDP 探测在同号端口上应答——防火墙两样都要放行：

```bash
ulimit -n 20480
./bin/connverifier serve -max-conns 20000
./bin/connverifier invite create -label alice -addr <服务器IP>:7443 > alice.invite
```

第一次 `serve` 或 `invite` 会在用户配置目录下生成节点密钥（`-state-dir` 可改位置）。
`invite list` 列出邀请串，`invite revoke -label alice` 吊销一张。

在要测网络的那台机器上，把邀请串交给 `capacity`——从文件读，或放进 `CONNVERIFIER_NODE`
环境变量，就不会留在 shell 历史里。开跑前它会先说明要建多少连接并请你确认——占满 NAT 表
可能让同一网络里的其他设备断网；`-yes` 跳过确认，标准输入不是终端时必须加：

```bash
ulimit -n 20480
./bin/connverifier capacity -node @alice.invite -clients 10000 -start-rate 500 -heartbeat 30s
./bin/connverifier capacity -node @alice.invite -clients 1000 -duration 1h -yes
```

`check` 不需要确认：它在 UDP 和 TCP 上各发每秒 50 个小包，持续 20 秒（`-rate`、`-duration`）。
UDP 探测出现之前建的邀请串里没有 UDP 端口，这时 UDP 那一项报 `UNSUPPORTED`。

```bash
./bin/connverifier check -node @alice.invite
```

```
UDP 往返时延与丢包：注意（WARN）
节点：192.0.2.10:7443 · IPv4 · UDP
观测
  发出的探测：1000
  丢失（未按时收到回包）：21
  丢包率：2.1%（95% 区间 1.38–3.19%，1000 个样本）
  去程丢失：10
  回程丢失：11
  往返时延 p50：34 ms（979 个样本）
推断
  - 没有按时收到回包的探测：21 个，其中迟到的：1 个。
```

`capacity` 运行中每秒往 stderr 打一行进度。结束时结果写到 stdout，默认是文本，`-format json` 则输出
JSON。节选：

```
TCP 长连接容量：注意（WARN）
节点：192.0.2.10:7443 · IPv4 · TCP
节点自报
  名称：tokyo-test
  批给的额度：50 条连接 · 每秒新建 100 条 · 时长 10.3 min · 空闲 2 min
观测
  成功建连：73
  掉线：31
  掉线（超时无回应）：3
  掉线（节点说明原因后关闭）：25
  回显往返时延 p95：309 µs（40 个样本）
  回显往返时延 p99：样本不足（40 个样本）
推断
  - 3 条连接没有收到关闭或重置就不再回应，这是 NAT 或其他中间设备悄悄丢弃连接状态的典型表现。
  - 25 条连接由节点说明原因后关闭（idle_timeout=25），不算网络掉线的证据。
```

文本语言跟随 `LC_ALL`、`LC_MESSAGES` 或 `LANG`，支持中文和英文，其余一律用中文；
`-lang en` 或 `-lang zh-CN` 可以覆盖。每个状态和数字是什么意思、什么时候不该信，见
[docs/methods](docs/methods/README.md)（英文）；节点协议见 [docs/protocol.md](docs/protocol.md)。

全部走旗标。`check`：`-node`、`-rate`（50）、`-duration`（20s）、`-dial-timeout`（5s）、
`-format`、`-lang`。`capacity`：`-node`、`-clients`（1000）、`-start-rate`（100）、`-heartbeat`（30s）、
`-dial-timeout`（5s）、`-io-timeout`（5s）、`-min-backoff`（500ms）、`-max-backoff`（1m）、
`-duration`（0＝直到中断）、`-log-drops`、`-format`（text）、`-lang`、`-yes`。`serve`：
`-listen`（:7443）、`-listen-udp`（同 `-listen`）、`-state-dir`、`-max-conns`（20000）、
`-max-sessions`（64）、`-log-connections`。`invite create`：`-label`、`-addr`（可重复）、
`-udp-port`（第一个 `-addr` 的端口）、`-max-sessions`（2）、`-max-connections`（20000）、
`-max-dial-rate`（1000）、`-max-stamp-rate`（100）、`-max-duration`（24h）、`-max-idle`（1h）。

两端都不开 TCP keepalive：keepalive 会不断刷新 NAT 映射，那样测出来的结果永远是「NAT 很稳」。

## 能力边界

- **只测空闲路径。** 不测带宽和跑满时的时延，没有 STUN、没有 DNS，UDP 只用一种包长；
  时延只有往返，从不报单向。
- **JSON 的 schema 还是 `v0`。** 不同构建之间仍可能变；要解析就固定一个构建，别解析文本。
- **退出码不评判网络质量。** 0 表示跑完了，不管各项状态如何；1 表示有检查没拿到有效
  测量（状态 `ERROR`，比如节点连不上），或工具自己出错；2 表示什么都没跑：参数不对，或
  没有确认。要按网络质量设门禁，读 JSON。
- **悄悄的掉线要靠心跳发现。** 关闭和重置一到就能看见，但被 NAT 悄悄遗忘的连接要等心跳
  没有回应才发现，这类掉线的存活时长是上界。
- **本机上限会框住结果。** 一条连接占一个文件描述符；单个源地址对同一个目标只有约 28000 个
  临时端口（macOS 与 Windows 约 16000）。客户端上限低于目标，或拨号因此失败，结果会标成
  `INVALID`，而不是当成网络的上限报出来。

## 与上游的区别

本仓库 fork 自 [codeberg.org/woq/ConnVerifier](https://codeberg.org/woq/ConnVerifier)。
我在它基础上加的是：单个 `connverifier` 程序与测试、带邀请串和逐张配额的鉴权节点协议、
RTT 百分位、掉线归因、带抖动的指数退避重连，以及给无人值守长跑用的 `-duration`。

## 安全

- **邀请串等同于密码。** 谁拿到它，谁就能在那张邀请串的额度内使用节点。私下发送；
  用 `invite revoke` 吊销。
- **节点只接待持邀请串的人。** 10 秒内完不成 TLS 或有效票据的连接会被断开，每个地址同时
  只能挂几条这样的连接。节点从不替客户端去连别处；UDP 探测只回应用活会话密钥签过名的包，
  回包不比来包大。
- **客户端会核对节点的密钥。** 邀请串钉住了节点公钥；节点出示别的密钥，客户端在发出令牌
  之前就会拒绝。
- **节点能看到你的公网地址**——你连的任何服务器都能。结果会在本地记下它，但结果里永远不含
  邀请串。

完整的威胁模型见 [docs/threat-model.md](docs/threat-model.md)。

## 许可证

MIT —— 见 [LICENSE](LICENSE)。版权行写的是「2025 woq」，即上游作者。
