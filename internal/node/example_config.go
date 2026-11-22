package node

// ExampleConfig 是 `tablenet-node init` 写出来的示例配置。
const ExampleConfig = `# Tablenet 节点配置。说明见 spec/06-部署与运维.md。
# 相对路径按本文件所在目录解析。

[node]
name = "我的私服"
public_host = "localhost"        # 写进 roomId 的主机名；上公网时填域名，并和证书一致
key_file = "data/node.key"       # 第一次启动自动生成；丢了就换了一台节点
data_dir = "data"
motd = ""

[listen]
ws = "0.0.0.0:8080"              # 玩家和适配器连 ws://<主机>:8080/tablenet/1
# tls_cert = "/etc/letsencrypt/live/node.example.org/fullchain.pem"
# tls_key  = "/etc/letsencrypt/live/node.example.org/privkey.pem"
admin = "127.0.0.1:7070"         # 管理接口，只许监听本机
trust_forwarded_for = false      # 放在反向代理后面时打开，按 X-Forwarded-For 认客户端地址

[limits]
max_connections = 2000
max_connections_per_ip = 8
max_rooms_per_player = 2
messages_per_second = 30
suspend_minutes = 10             # 对局中房主掉线，房号保留多久
resume_seconds = 120             # 断线多久内重连能接回房间和排队位置
seat_request_seconds = 10        # 等适配器回话的时限
match_accept_seconds = 15        # 匹配确认的时限

# 允许接入的外部平台。没列在这里的平台，适配器连不进来。
# [adapters.example]
# key = ""                       # 钉死适配器公钥；空表示第一次见到时记下
# servers = []                   # 交接时允许出现在 connect.server 里的主机名
# trust_results = false          # 是否把它报的战绩当作平台担保
# run = "./tablenet-adapter-example"   # 由节点代管时填；不填就由你自己跑
# args = []

[federation]
enabled = false
peers = []
block = []
`

// PublicConfig 是 `tablenet-node init --region … --public-host …` 写出来的配置：
// 用自签证书跑 wss://、并入自组网，名片默认 private（只登记、不进推荐），运营者确认合规后再改 open。
func PublicConfig(region, publicHost string) string {
	return `# Tablenet 公网节点配置。说明见 spec/06-部署与运维.md、spec/09-节点自组网.md。
# 相对路径按本文件所在目录解析。

[node]
name = "我的节点"                # 1–40 字，会显示在全网的节点列表里
public_host = "` + publicHost + `"
key_file = "data/node.key"       # 节点身份。丢了就换了一台节点，名录里的在线记录也从头算
data_dir = "data"
motd = ""

[listen]
ws = "0.0.0.0:8443"
tls_cert = "data/tls/cert.pem"   # init 生成的自签证书；指纹会写进名片，客户端按指纹校验，不需要域名
tls_key  = "data/tls/key.pem"
admin = "127.0.0.1:7070"
trust_forwarded_for = false

[limits]
max_connections = 2000
max_connections_per_ip = 8
max_rooms_per_player = 2
messages_per_second = 30
suspend_minutes = 10
resume_seconds = 120
seat_request_seconds = 10
match_accept_seconds = 15

[directory]
enabled = true
region = "` + region + `"
# public_urls = ["wss://` + publicHost + `:8443/tablenet/1"]   # 不填就按 public_host 和端口推
status = "private"               # private：只登记、不进推荐。确认合规后改成 open
operator = ""                    # 自愿公开的联系方式
capacity_players = 500
capacity_rooms = 200
relay_mbps = 50
roots = []                       # 根公钥；空则用代码里写死的
list_urls = []                   # 名录清单地址；空则用代码里写死的

[federation]
enabled = false
peers = []
block = []
`
}

// ComplianceNotice：init 时打印给运营者看。
const ComplianceNotice = `提醒：Tablenet 是开源软件，每台节点由谁运营、谁负责。
在中国大陆面向公众运营网络游戏服务，可能涉及 ICP 备案和网络游戏运营方面的资质要求，请自行确认。
名片默认是 private（只登记、不进推荐）；确认之后把 [directory] status 改成 open。`
