# tablenet-adapter-hexproof

把一个 [Hexproof](https://github.com/ClayStan404/hexproof) hub 接进 Tablenet 节点。**GPL-3.0-or-later**（照 Hexproof 的 `protocol/v3` schema 实现），是独立的 Go 模块，Tablenet 节点本体（MIT）不链接它。

> 状态：0.1。用模拟 hub 测过；**还没连过真的 Hexproof hub，也没被真的独弈客户端用过**（见文末）。

## 它做三件事

| | 做什么 |
|---|---|
| 大厅镜像 | 每 5 秒向 hub 要一次 `room.list`，把等人的、服务器主机的房间发布到节点（`venue.room.upsert`）；hub 上没了的撤下 |
| 交接 | 玩家要去 Hexproof 客户端打时，回 `connect {client: "hexproof", server, room, protocol}` |
| 桥接 | 玩家在独弈里打 Hexproof **规则房**：适配器替他连 hub、进房、交牌组、准备，在对局通道里冒充独弈主机，把 `rules.snapshot`／`rules.prompt` 翻成 `arcane.v0`，把独弈的指令翻回 `rules.respond` |

规则房（`rulesMode: forge`）发布成桥接房（解不了 `arcane.v0` 的玩家由节点自动退成交接）；手动房（`manual`）只做交接，房间带 `x-hexproof-rules`。
玩家主机的房（`hostingMode: player`）和已开打的房不发布。

排队配到 Hexproof 时：Hexproof 建房的人自己就占一个座位，所以由配对里**第一位能桥接的玩家**的会话来建房（规则房、服务器主机、随机密码），其他人凭房号和密码进。双方都不能桥接时适配器回错，节点把两人放回排队。

## 跑起来

```sh
go build -o ../../bin/ ./cmd/tablenet-adapter-hexproof
```

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `TABLENET_NODE_URL` | `ws://127.0.0.1:8080/tablenet/1` | 节点代管时自动给 |
| `TABLENET_ADAPTER_KEY` | `hexproof-adapter.key` | 适配器私钥，没有就生成 |
| `HEXPROOF_HUB_URL` | `ws://127.0.0.1:57320/ws` | 适配器连 hub 的地址 |
| `HEXPROOF_PUBLIC_URL` | 同上 | 交接时告诉玩家的 hub 地址 |

节点配置里放行它，并把 hub 的主机名登记进 `servers`（交接信息里的服务器地址要过这道检查）：

```toml
[adapters.hexproof]
servers = ["hub.example.org"]
run = "./bin/tablenet-adapter-hexproof"
```

只连**你自己部署的** hub，或者征得 hub 运营者同意（spec/02）。

## 协议版本

只讲 `hexproof.v3`，hub 讲别的版本时握手直接失败。Hexproof 要求版本完全相等；它的官方 hub 2026-10-25 起是 `hexproof.v2`，源码 master 是 `v3`。所以现在只能连用 master 源码自建的 hub。

## 提问对照

20 种 `kind` 全部有对照（`bridge/session.go`），表见 spec/08。几处说明：

- `acknowledge`、`diceRolled` 不是提问：发一条 `message`，替玩家回 `$ack`。
- `revealCards` 用 `reveal_request`，玩家关掉时回 `$ack`。
- `scry` **退化**：独弈的牌库面板只认顶／底，Hexproof 的去处可能更多，所以退成「每张牌选一个去处」的选项框，记一条 `bridge.degraded` 日志。
- `supported: false`（Hexproof 自己也答不了）：锁住提问，提示只能等超时或认输。
- 没有对照的 `kind`：退成通用选项框，把 Hexproof 给的选项原样列出。

## 测试

```sh
go test ./...
```

- `bridge/`：翻译层单测，覆盖局面、优先权（单动作直接交、多动作先选）、进攻（换防御对象、全部进攻、撤回）、阻挡（改挡、全部阻挡、挡不了要拒）、选牌（选满即交、只读不能选）、选目标、是否、数字、颜色、牌名、调度、排序、战斗伤害分配、占卜、通知、展示、不支持、认输、过期提问。
- 根目录：模拟 hub（照 schema）＋真 Tablenet 节点（`tablenet/pkg/nodetest`）的端到端测试：大厅镜像与撤房、交接、桥接全程（握手 → 进房带密码 → 交牌组 → 加载 → 局面 → 提问 → 回答 → 认输）、坏握手（版本、没牌组、不是 join、密码错）、排队建房、双方都不能桥接时拒绝。

`BRIDGE_DUMP=<文件> go test ./...` 会把发给独弈客人的每一帧写出来，可以用独弈仓库的 `arcane.v0` schema 校验。最近一次：123 帧、25 种，全部通过。

## 还没做的、还没验证的

**真 hub 实测（2026-11-22，Hexproof 3.4.0，源码 `f2b56e2`，没装 Forge 运行时）：**

`HEXPROOF_LIVE_URL=ws://127.0.0.1:57320/ws go test -run Live -v .` 跑两组：`TestLiveHub` 逐项核对 hub 行为，`TestLiveAdapter` 是真 hub＋真节点＋适配器。

| 项 | 结果 |
|---|---|
| 握手 | `hexproof.v3` 通过 |
| 房间表字段、赛制映射 | 对得上（`waiting`、`playerJoinable`、`hostingMode: server`） |
| 进房回的 `seat` | 是本人座位号（房主 0、客人 1） |
| 密码错 | `wrong_password`，适配器映射成独弈的 `bad_password` |
| 牌组缺系列或收藏编号 | **hub 拒收（`invalid_deck`），基本地也一样**。适配器改成先拦下、说清是哪几张。独弈 0.4 起在 `join` 里另附 `printedDeck`（每行 `张数 牌名\|系列\|收藏编号`，客户端按自己的卡牌库补齐，没挑版本的用默认那一版），适配器优先用它；`deck` 是 .dck 原文，`\|` 后面那个数是 Forge 的画稿序号，不能当收藏编号 |
| `client.load_complete` | **必须是带 `id` 的请求**，不带回 `invalid_message`、对局开不了。原来发的是通知，已修；模拟 hub 也照此要求 |
| 自己认输 | `game.conceded` 作为请求的回话回来（带 `id`），适配器已按此处理 |
| 房主离开 | 房间解散（`room.disbanded`），房间表随即消失。排队建房由桥接玩家自己建，避开了这一点 |
| 大厅镜像＋交接（真 hub＋真节点） | 通过：hub 上开的指挥官手动房出现在节点上（`commander`／`ffa`／交接），入座给 `connect`；房解散后节点上撤下 |

**规则房桥接实测（同日，装上 Forge 运行时 adapter25＋Temurin JRE 21）：**

`HEXPROOF_LIVE_URL=… HEXPROOF_LIVE_RULES=1 go test -run 'TestLiveRules$' -v -timeout 8m .`：hub 上一个自动回默认答案的「Hexproof 玩家」，对经适配器桥接、在对局通道里讲 `arcane.v0` 的「独弈玩家」（能下地就下地、能施放就施放）。

结果：**一整局打完**（68 秒，独弈一侧获胜，`game_over` 送到）。桥接一侧答了 353 次，跑到了让过／施放、付费、调度、宣攻 28 次、选目标与选牌、选项框、展示牌，13 个阶段全部映射正确；发给独弈的 1671 帧全部通过 `arcane.v0` schema。宣告阻挡没跑到（对面机器人不进攻）。

实测查出、已修的：

| 问题 | 修法 |
|---|---|
| 房间一开局，大厅镜像就把它撤了，节点撤房时连桥接通道一起关，hub 等不到这位玩家加载完 | 适配器：发布过的房开局后改成 `playing` 挂着；节点：撤外部平台的房不再关它的通道（通道归适配器管）。两边都加了回归测试 |
| 座位没坐满之前 hub 不让准备（`seats_not_filled`） | 交了牌组先记着，等房间快照显示坐满了再准备 |
| 开局失败（牌组里某个印次 Forge 不认）时，最后加载完的人是在 `load_complete` 的回话里收到 `rules_unavailable`，独弈一侧收不到 | 转成 `error`（`remote_start_failed`）告诉玩家；等这条回话改在另一条协程里，主循环照常发心跳 |
| 回答被 hub 拒（`rules_action_rejected`）后，适配器已经把这一问清掉，整局卡死 | 被拒时把这一问恢复、清空已选、换个编号重问 |
| 付不起时 Hexproof 不提供 `$auto-pay`，适配器照发 | 只发这一问里真有的选项，没有就本地拒 |

另外记下的事实：Hexproof 的步骤名是小写（`main1`、`declare_attackers`），适配器已经认；动作标签是「牌名 — cast spell／play land」；Forge 会把付不起的牌也列成可施放；`-forge-home` 要指到运行时里的 `forge-gui/` 并带结尾斜杠，不然报找不到 `en-US` 语言包（路径里有中文不影响）；hub 默认同时只跑 1 局 Forge（`-max-forge-games`）。

**还没验证：**
- 宣告阻挡、战斗伤害分配、排序、占卜这几种提问还没在真 hub 上遇到过（只有单测和模拟 hub）。
- 没被真的独弈客户端用过（独弈 0.4 还没接 Tablenet）。帧格式过了 schema，行为照独弈主机的代码推的（例如调度提问「确定＝保留、取消＝调度」）。
- 独弈的牌组常有不带印次的行（`16 Mountain`）。桥接时要由独弈按本地印次库补齐再发，这是桌面端的事。

**没做：**
- 三局两胜的换备（Hexproof 的 `sideboard.*`）：第一局打完之后桥接会停住。
- 断线重连（独弈的 `rejoin`）、观战桥接房。
- 牌面信息：Hexproof 只给牌名、系列、收藏编号，费用、类型、规则文字为空，要独弈按牌名从本地牌库补。
- `$pass-stack`（让到堆叠结算完）没有对应的独弈指令，只接了普通让过。
