package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config 对应 node.toml，字段说明见 spec/06-部署与运维.md。
type Config struct {
	Node       NodeSection              `toml:"node"`
	Listen     ListenSection            `toml:"listen"`
	Limits     LimitsSection            `toml:"limits"`
	Adapters   map[string]AdapterConfig `toml:"adapters"`
	Federation FederationSection        `toml:"federation"`
}

type NodeSection struct {
	Name       string `toml:"name"`
	PublicHost string `toml:"public_host"`
	KeyFile    string `toml:"key_file"`
	DataDir    string `toml:"data_dir"`
	Motd       string `toml:"motd"`
}

type ListenSection struct {
	WS                string `toml:"ws"`
	TLSCert           string `toml:"tls_cert"`
	TLSKey            string `toml:"tls_key"`
	Admin             string `toml:"admin"`
	TrustForwardedFor bool   `toml:"trust_forwarded_for"`
}

type LimitsSection struct {
	MaxConnections      int `toml:"max_connections"`
	MaxConnectionsPerIP int `toml:"max_connections_per_ip"`
	MaxRoomsPerPlayer   int `toml:"max_rooms_per_player"`
	MessagesPerSecond   int `toml:"messages_per_second"`
	SuspendMinutes      int `toml:"suspend_minutes"`
	ResumeSeconds       int `toml:"resume_seconds"`
	SeatRequestSeconds  int `toml:"seat_request_seconds"`
	MatchAcceptSeconds  int `toml:"match_accept_seconds"`
}

// AdapterConfig：允许接入的一个外部平台。
type AdapterConfig struct {
	Key          string   `toml:"key"`           // 钉死的适配器公钥；空表示第一次见到时记下
	Servers      []string `toml:"servers"`       // 交接时允许出现在 connect.server 里的主机名
	TrustResults bool     `toml:"trust_results"` // 是否把它报的战绩当作平台担保
	Run          string   `toml:"run"`           // 由节点代管时的可执行文件
	Args         []string `toml:"args"`
}

type FederationSection struct {
	Enabled bool     `toml:"enabled"`
	Peers   []string `toml:"peers"`
	Block   []string `toml:"block"`
}

func DefaultConfig() Config {
	return Config{
		Node:   NodeSection{Name: "Tablenet node", PublicHost: "localhost", KeyFile: "data/node.key", DataDir: "data"},
		Listen: ListenSection{WS: "0.0.0.0:8080", Admin: "127.0.0.1:7070"},
		Limits: LimitsSection{
			MaxConnections: 2000, MaxConnectionsPerIP: 8, MaxRoomsPerPlayer: 2, MessagesPerSecond: 30,
			SuspendMinutes: 10, ResumeSeconds: 120, SeatRequestSeconds: 10, MatchAcceptSeconds: 15,
		},
		Adapters: map[string]AdapterConfig{},
	}
}

// LoadConfig 读配置文件；相对路径（密钥、数据目录、代管程序）按配置文件所在目录解析。
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, fmt.Errorf("读配置 %s：%w", path, err)
	}
	base := filepath.Dir(path)
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	cfg.Node.KeyFile = abs(cfg.Node.KeyFile)
	cfg.Node.DataDir = abs(cfg.Node.DataDir)
	cfg.Listen.TLSCert = abs(cfg.Listen.TLSCert)
	cfg.Listen.TLSKey = abs(cfg.Listen.TLSKey)
	for id, a := range cfg.Adapters {
		if a.Run != "" && (strings.HasPrefix(a.Run, ".") || strings.ContainsAny(a.Run, `/\`)) {
			a.Run = abs(a.Run)
		}
		cfg.Adapters[id] = a
	}
	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	if c.Node.PublicHost == "" {
		return fmt.Errorf("node.public_host 不能为空")
	}
	if strings.ContainsAny(c.Node.PublicHost, "/ ") {
		return fmt.Errorf("node.public_host 只写主机名：%q", c.Node.PublicHost)
	}
	if (c.Listen.TLSCert == "") != (c.Listen.TLSKey == "") {
		return fmt.Errorf("listen.tls_cert 和 listen.tls_key 要么都填、要么都不填")
	}
	if c.Listen.Admin != "" && !isLoopbackAddr(c.Listen.Admin) {
		return fmt.Errorf("listen.admin 只许监听本机（127.0.0.1 或 [::1]），现在是 %q", c.Listen.Admin)
	}
	for id := range c.Adapters {
		if !validPlatform(id) {
			return fmt.Errorf("adapters.%s：平台编号不合规", id)
		}
	}
	return nil
}

func isLoopbackAddr(addr string) bool {
	return strings.HasPrefix(addr, "127.0.0.1:") || strings.HasPrefix(addr, "[::1]:") || strings.HasPrefix(addr, "localhost:")
}

func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
