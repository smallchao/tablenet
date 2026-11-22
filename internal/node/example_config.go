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
