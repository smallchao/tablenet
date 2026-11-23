# tablenet-adapter-xmage

把**别人已经在运营的** [XMage](https://github.com/magefree/mage) 服务器（例如 xmage.de）接进 Tablenet 节点，一个适配器可以同时接好几台。**MIT**（XMage 本身是 MIT）。

**不需要、也不自带 XMage 服务器。** 适配器在每台服务器上挂一个大厅机器人账号，像一个 XMage 客户端那样登录；跑适配器的机器只要 Java 17 和一份与目标服务器同版本的 XMage jar（XMage 客户端发布包里的 `lib` 就行）。

> 状态：0.2。2026-11-22 用本机起的一台 XMage 1.4.63-V2 当替身实测过（还没连过真的公共服务器）：桥接整局打完、排队开桌（一位桥接一位交接）打完，发给独弈的帧全部通过 `arcane.v0` schema。**还没被真的独弈客户端用过**（见文末）。

## 两段

XMage 的客户端和服务器之间是 JBoss Remoting 加 Java 序列化，只能用 Java 接。所以适配器分两段：

| | 语言 | 做什么 |
|---|---|---|
| `driver/` | Java | 用 XMage 自己的 `Mage.Common`（`SessionImpl`）连服务器；一个进程里开多个会话（大厅机器人一个，每位桥接玩家各一个）；把局面和提问压平成普通 JSON |
| 其余 | Go | 讲 Tablenet、管房间和排队、把局面和提问翻成独弈的 `arcane.v0` |

两段之间用驱动的标准输入输出讲 JSON Lines（见 `driver/src/tablenet/xmage/Driver.java` 开头的注释）。驱动的 jar **只带本驱动的类**，运行时从 XMage 服务器的 `lib` 目录加载 XMage 的 jar，所以本仓库不分发 XMage 的任何东西。

## 它做四件事

| | 做什么 |
|---|---|
| 大厅镜像 | 每 10 秒取一次每台服务器主房间的桌子，把等人的、认得出赛制的发布到节点（`venue.room.upsert`，`join: bridge`）；XMage 上没了的撤下。锦标赛、限制赛的桌不发布 |
| 交接 | 玩家要去 XMage 客户端打时，回 `connect {client: "xmage", server, room: 桌名}`（排队开的桌另带 `password`），玩家用自己的 XMage 账号进 |
| 桥接 | 玩家在独弈里打 XMage 上的局：适配器替他开一个 XMage 会话（临时用户名）、坐下、交牌组，在对局通道里冒充独弈主机。**只在允许不注册登录的服务器上做**；要注册的服务器只做交接（适配器不经手玩家的 XMage 密码） |
| 排队开桌 | 排队配到 XMage 时，大厅机器人在设了 `queue` 的那台（没设就第一台）建一张带随机密码的桌（**不坐下**），桥接的玩家由适配器替他坐下，交接的玩家拿地址、桌名、密码自己去坐；坐满后机器人开打（XMage 只认桌主按开始）。10 分钟没坐满就撤桌 |

## 跑起来

```sh
# 1. 编 Java 驱动（要 JDK 17+，对着服务器同一版的 XMage lib 编）
XMAGE_LIB=/opt/xmage/mage-server/lib ./driver/build.sh ../../bin
# 2. 编 Go 主体
go build -o ../../bin/ ./cmd/tablenet-adapter-xmage
```

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `TABLENET_NODE_URL` | `ws://127.0.0.1:8080/tablenet/1` | 节点代管时自动给 |
| `TABLENET_ADAPTER_KEY` | `xmage-adapter.key` | 适配器私钥，没有就生成 |
| `XMAGE_SERVERS` | — | 要接入的服务器清单（JSON 文件，见下）。机器人密码写在里面，文件名用 `servers.json` 会被 `.gitignore` 挡住 |
| `XMAGE_SERVER`、`XMAGE_PUBLIC_SERVER`、`XMAGE_USER`、`XMAGE_PASSWORD` | `127.0.0.1:17171` | 只接一台时的简写（没给 `XMAGE_SERVERS` 时用） |
| `XMAGE_LIB` | （必填） | 和目标服务器同一版本的 XMage `lib` 目录（客户端或服务器发布包里的都行） |
| `XMAGE_DRIVER` | 可执行文件旁边的 `tablenet-xmage-driver.jar` | Java 驱动 |
| `XMAGE_DRIVER_DIR` | `xmage-driver` | 驱动的工作目录。按牌名补系列和收藏编号要一个本地牌库，第一次启动从 XMage 的 jar 建在这里，二三十秒、约 300 MB |
| `JAVA` | PATH 里的 `java` | 要 17 或更新 |

服务器清单（`servers.example.json`）：

```json
[
  { "id": "de", "name": "xmage.de", "address": "xmage.de:17171", "user": "tablenet", "accounts": "optional", "queue": true },
  { "id": "us", "name": "某台北美服", "address": "us.example.org:17171", "user": "tablenet", "password": "…",
    "accounts": "required", "lib": "/opt/xmage-1.4.63/mage-client/lib" }
]
```

| 字段 | 说明 |
|---|---|
| `id` | 短名（1–12 位小写字母数字），进房间编号：`<节点>/xmage-<id>.<XMage 桌子编号>` |
| `address` / `public` | 适配器连它的地址／交接时告诉玩家的地址（默认同 `address`） |
| `user` / `password` | 大厅机器人的账号。要注册的服务器得先在那台上给机器人注册好 |
| `accounts` | `optional`（默认，不注册也能登，能桥接）或 `required`（只做交接）。写了 `optional` 但匿名登录被拒时，适配器启动时会自己发现、改成只做交接 |
| `lib` | 这台服务器版本对应的 XMage `lib`；不同版本的服务器各指各的，同一版本共用一个驱动进程 |
| `queue` | 排队开桌用这台 |

上线前在跑适配器的那台机器上只读检查一遍（连得通吗、版本对不对、要不要注册、主房间有哪些桌，不建桌不入座）：

```sh
XMAGE_SERVERS=servers.json XMAGE_LIB=… tablenet-adapter-xmage probe
```

注意：本机走代理软件的「假 IP」（DNS 解析成 198.18.x.x）时，代理多半不转 17171 端口，表现为握手时被对方断开（`Wrong client-server protocol`）——这不是版本问题，换一台直连的机器跑。

节点配置里放行它，并把各台交接地址的主机名登记进 `servers`：

```toml
[adapters.xmage]
servers = ["xmage.de", "us.example.org"]
run = "./bin/tablenet-adapter-xmage"
trust_results = true   # 想把桥接局的结果当平台担保记下时
```

大厅机器人用户名默认 `tablenet`，桥接玩家的用户名是「展示名里的小写字母数字＋随机后缀」（XMage 只许 `[a-z0-9_]`、3–14 位）。

**接别人的服务器**：先和运营者打个招呼（spec/02）——机器人会一直挂在主房间里，排队开的桌会以机器人名义出现在他们的大厅。适配器尽量少打扰：默认 10 秒取一次桌子表，建的桌都叫「Tablenet …」、带随机密码、10 分钟没坐满就撤，不转发别人服务器上的玩家名。

（自己起 XMage 服务器测试时注意：用 Java 17 跑要加 `--add-opens java.base/java.io=ALL-UNNAMED --add-opens java.base/java.lang=ALL-UNNAMED --add-opens java.base/java.util=ALL-UNNAMED`，不然客户端一连就被断开。驱动这边已经带上了。）

## 版本

XMage 登录时要求客户端版本和服务器**完全一样**。驱动用的是 `lib` 里那份 `mage-common`，所以每台服务器要指向和它同版本的 XMage 发布包。公共服务器升级后，下一份同版本的 XMage 客户端发布包、改 `lib`、重启适配器（驱动的 jar 不用重编，除非 XMage 改了它用到的接口）。版本不对时日志 `xmage.connect` 会写明。

## 提问对照

XMage 和独弈客人前端都是「点一下答一次」：XMage 每收到一个回答就重新问一次，独弈客人每点一下发一条指令。所以绝大多数是一对一转发（`bridge/session.go`），对照表见 spec/08。几处适配器替玩家多走一步：

- **进攻**：点进攻者后 XMage 若追问「打谁」，照玩家在独弈里选定的防御对象（默认第一位对手，点鹏洛客可改）替他答。「全部进攻」回 `special`，「全部撤回」排成逐只再点一次。
- **阻挡**：XMage 是先点阻挡者、再问挡哪个进攻者；适配器照独弈 `assign_block` 里的进攻者替他答。改挡要先撤再挡，排成两下。XMage 没有「全部阻挡」，不替玩家乱挡。
- **自动支付**：XMage 没有自动横置地。独弈的「自动支付」由适配器逐个点可用的法术力来源（战场上的地优先），点完还没付清就停下、报错、把提问交还玩家。
- **调度**：XMage 的 `true` 是调度、`false` 是保留，和按钮位置相反，已对好。
- **可搜索的长名单**（说出牌名一类，超过 40 项）用独弈的文字输入框，按文字对回选项。
- 认不出的提问退成「确定／取消」选项框，记一条 `bridge.degraded` 日志。

## 测试

```sh
go test ./...                 # 翻译层单测，不连 XMage
```

连真服务器的端到端（平时跳过；可以指向自己起的一台替身，不要拿别人的公共服务器跑）：

```sh
XMAGE_LIVE=127.0.0.1:17171 XMAGE_LIB=…/mage-server/lib XMAGE_DRIVER=…/tablenet-xmage-driver.jar \
XMAGE_DRIVER_DIR=…/工作目录 BRIDGE_DUMP=frames.jsonl go test -run Live -v .
```

- `TestLiveBridge`：测试自己开一个 XMage 会话建桌、坐下、自动应答；适配器把桌子发布到真 Tablenet 节点，脚本化的独弈客人在节点上找到它、桥接入座，一路打完。
- `TestLiveQueue`：两位玩家排队配到 XMage，一位能桥接、一位只能交接；交接的那位照交接信息（地址、桌名、密码）坐下，机器人坐满开打，桥接的那位在独弈里打完。

2026-11-22 实测（本机替身 XMage 1.4.63-V2，Windows，Java 17）：两项连着跑都通过，各 33–37 秒打完一局；独弈客人遇到了优先权、下地、施法、调度、进攻、选目标（含选牌手）、替代费用选择、牺牲选择；适配器报错 0 次；发出的帧（518、625 帧）全部通过独弈的 `arcane.v0` schema。

另外把替身临时改成要注册（`authenticationActivated="true"`）核过：匿名登录被拒的原因是登录返回之后才用 `SHOW_USERMESSAGE` 发来的（「Wrong username or password. You must register your account first.」），驱动会等它带回来，适配器据此把这台改成只做交接。

## 还没做的、还没验证的

- **还没连过真的公共服务器**（xmage.de 等）：要先定接哪几台、和运营者打招呼、给机器人注册账号（要注册的那几台）。
- **没被真的独弈客户端用过**：上面的客人是脚本，只证明帧合规、流程走得通，界面上好不好用要等真客户端。
- 宣告阻挡、分配战斗伤害、多人局、指挥官局还没在真服务器上遇到。
- 三局两胜：局间换备先原样交回同一副牌（独弈的换备牌墙没接到桥接上）。
- 交接档拿不到胜负：机器人不在桌上收不到对局回调，只有桥接玩家的局会报 `venue.result`。
- 没做观战、断线重连（XMage 会话断了这一局就算断了）。
- XMage 的提示文字是英文，原样显示。
