# Tablenet（代号）

一个**不懂规则**的开放打牌联机网络：负责认人、开房、匹配和转发，一局牌由谁裁决是各家引擎自己的事。
任何人都能自建节点（私服）；别的对战平台写一个适配器，就能和网络里的玩家互相匹配，甚至在别家的客户端里直接打（桥接）。

> 状态：**0.1 开发中**。规范是草案 0；节点本体、参考适配器、Go 客户端、Rust SDK、Hexproof 适配器、XMage 适配器（都是大厅镜像＋交接＋桥接＋排队开桌，都连真服务器打完过整局）已能跑。项目名是代号，正式名待定。

## 目录

| 路径 | 内容 | 语言 |
|---|---|---|
| [spec/](spec/README.md) | 协议规范：客户端协议、适配器协议、联邦、部署、首批适配器、桥接模式 | — |
| `cmd/tablenet-node` | 节点可执行文件（纯后端，没有界面） | Go |
| `cmd/tablenet-dir` | 名录服务＋根密钥工具（自组网，只由项目方运营） | Go |
| `cmd/tablenet-probe` | 独立探测点 | Go |
| `internal/directory` | 名录：登记、探测、同步、下架、认证 | Go |
| `pkg/discovery` | 客户端发现节点：取名录清单、查节点、按证书指纹连 | Go |
| `internal/node` | 节点本体：握手、房间、对局通道、排队匹配、适配器接入、战绩、管理接口 | Go |
| `pkg/proto` | 线上格式：信封、Ed25519 身份与签名、通道帧、房号、错误码 | Go |
| `pkg/client` | Go 客户端，写 Go 适配器可以直接用 | Go |
| `adapters/example` | 参考适配器：一个假平台，演示发布房间、交接、桥接 | Go |
| [adapters/hexproof](adapters/hexproof/README.md) | Hexproof 适配器（独立模块，GPL-3.0） | Go |
| `pkg/nodetest` | 在测试进程里起一台真节点，给适配器写端到端测试用 | Go |
| `sdk/rust` | Rust SDK（`tablenet-proto`、`tablenet-client`），给 Rust 写的客户端用（独弈桌面端） | Rust |

Go 和 Rust 两边的身份编码和签名用同一组固定向量测过，逐字节一致（`pkg/proto` 的 `TestFixedVector`、`sdk/rust/tablenet-proto` 的 `fixed_vector`）。

## 起一台节点

要 Go 1.24 以上。

```sh
go build -o bin/ ./cmd/tablenet-node ./adapters/example/cmd/tablenet-adapter-example
./bin/tablenet-node init --config node.toml     # 写一份示例配置
./bin/tablenet-node run --config node.toml
```

玩家和适配器连 `ws://<主机>:8080/tablenet/1`。上公网时在配置里填证书（`listen.tls_cert`／`tls_key`）走 `wss://`，或者放在反向代理后面并打开 `trust_forwarded_for`。

管理（节点在跑时，在同一台机器上）：

```sh
./bin/tablenet-node admin status --config node.toml
./bin/tablenet-node admin rooms --config node.toml
./bin/tablenet-node admin ban <playerId> 原因 --config node.toml
```

公开的 HTTP：`/healthz`、`/node.json`（节点信息，客户端添加私服时先读它）、`/stats.json`。

## 测试

```sh
go test ./...                      # 节点端到端测试：真起 WebSocket，玩家、适配器、排队、桥接全走一遍
(cd sdk/rust && cargo test)        # Rust SDK
```

## 0.1 还没做的

规范里写了、还没实现的：自组网第二、三期（全网房间索引、跨节点匹配、主机接管、端到端加密，见 `spec/09`）、0.3 中继兼容网关（`legacy-tcp`）、账号绑定（`link.*`）、主机接管（`host-takeover`）、端到端加密和 iroh 直连（可选特性）、多人局排队（现在只排一对一）。自组网第一期（名录、名片、探测、节点身份证明、证书指纹、状态落盘、排空、`/metrics`）已实现。

## 许可证

节点本体：[MIT](LICENSE)。
`adapters/` 下的每个适配器是独立程序，许可证看各自目录里的 `LICENSE`：示例和 XMage 适配器是 MIT，Hexproof 适配器是 GPL-3.0、放在单独的 Go 模块里，节点本体不链接它们。XMage 适配器分 Go 主体和一小段 Java 驱动（XMage 只能用 Java 接），驱动运行时加载 XMage 服务器自己的 jar，本仓库不带 XMage 的东西。

本仓库不包含任何卡牌规则引擎、卡牌数据或卡图。与 Wizards of the Coast 无关联，“Magic: The Gathering” 是其商标。
