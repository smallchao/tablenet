// Package nodetest 在测试进程里起一台真的 Tablenet 节点，给适配器作者写端到端测试用。
//
//	n := nodetest.Start(t, nodetest.Options{Adapters: map[string]nodetest.Adapter{"myplatform": {}}})
//	c, _ := client.Dial(ctx, n.URL, key, hello)
package nodetest

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tablenet/internal/node"
	"tablenet/pkg/proto"
)

// Adapter 是放行一个外部平台时的配置（对应 node.toml 的 [adapters.<平台>]）。
type Adapter struct {
	Servers      []string // 交接时 connect.server 允许的主机名
	TrustResults bool
}

type Options struct {
	PublicHost string // 默认 test.local
	Adapters   map[string]Adapter
	// MessagesPerSecond 默认 500，测试里别被限流。
	MessagesPerSecond int
}

type Node struct {
	URL  string // ws://…/tablenet/1
	Dir  string // 数据目录
	stop chan struct{}
}

// Start 起节点，测试结束时自动关。
func Start(t testing.TB, o Options) *Node {
	t.Helper()
	dir := t.TempDir()
	cfg := node.DefaultConfig()
	cfg.Node.PublicHost = o.PublicHost
	if cfg.Node.PublicHost == "" {
		cfg.Node.PublicHost = "test.local"
	}
	cfg.Node.DataDir = dir
	cfg.Node.KeyFile = filepath.Join(dir, "node.key")
	cfg.Limits.MessagesPerSecond = o.MessagesPerSecond
	if cfg.Limits.MessagesPerSecond == 0 {
		cfg.Limits.MessagesPerSecond = 500
	}
	cfg.Adapters = map[string]node.AdapterConfig{}
	for id, a := range o.Adapters {
		cfg.Adapters[id] = node.AdapterConfig{Servers: a.Servers, TrustResults: a.TrustResults}
	}
	srv, err := node.NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	n := &Node{URL: "ws" + strings.TrimPrefix(ts.URL, "http") + proto.WSPath, Dir: dir, stop: make(chan struct{})}
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				srv.Hub.Tick()
			case <-n.stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(n.stop); ts.Close() })
	return n
}
