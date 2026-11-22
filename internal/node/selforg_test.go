package node

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tablenet/internal/directory"
	"tablenet/pkg/client"
	"tablenet/pkg/discovery"
	"tablenet/pkg/proto"
)

// 自组网端到端（spec/09 第一期）：两台名录（cn-east、hk）、根密钥签的清单、一台用自签证书的节点。
// 节点自己登记 → 两个区都探测通过 → 名录之间同步 → 客户端按指纹连上并验节点身份 → 认证 → 排空 → 下架。

type swap struct{ h http.Handler }

func (s *swap) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.h.ServeHTTP(w, r) }

type selfOrg struct {
	root    ed25519.PrivateKey
	roots   []string
	listF   string
	dirs    map[string]*directory.Directory // zone → 名录
	urls    map[string]string
	cancel  context.CancelFunc
	discard *slog.Logger
}

func startDirectories(t *testing.T) *selfOrg {
	t.Helper()
	so := &selfOrg{root: proto.GenerateKey(), dirs: map[string]*directory.Directory{}, urls: map[string]string{},
		discard: slog.New(slog.NewTextHandler(io.Discard, nil))}
	so.roots = []string{proto.IDOfKey(so.root)}
	keys := map[string]ed25519.PrivateKey{"cn-east": proto.GenerateKey(), "hk": proto.GenerateKey()}
	swaps := map[string]*swap{}
	entries := []proto.DirectoryEntry{}
	for _, region := range []string{"cn-east", "hk"} {
		sw := &swap{h: http.NotFoundHandler()}
		ts := httptest.NewServer(sw)
		t.Cleanup(ts.Close)
		swaps[region], so.urls[proto.ProbeZone(region)] = sw, ts.URL
		entries = append(entries, proto.DirectoryEntry{ID: proto.IDOfKey(keys[region]), URL: ts.URL, Region: region})
	}
	now := time.Now()
	list, err := proto.SignDirectoryList(so.root, proto.DirectoryList{Seq: 1, IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour), Directories: entries})
	if err != nil {
		t.Fatal(err)
	}
	so.listF = filepath.Join(t.TempDir(), "directories.json")
	b, _ := json.Marshal(list)
	_ = os.WriteFile(so.listF, b, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	so.cancel = cancel
	t.Cleanup(cancel)
	for _, region := range []string{"cn-east", "hk"} {
		d, err := directory.New(directory.Config{Region: region, ProbeEvery: 200 * time.Millisecond, PullEvery: 100 * time.Millisecond,
			NewPerPrefixPerHour: 100, MaxNodesPerIP: 100, Roots: so.roots, MinVersion: "9.0.0", MinNote: "测试用的升级提醒"},
			keys[region], list, so.discard)
		if err != nil {
			t.Fatal(err)
		}
		swaps[region].h = d.Handler()
		so.dirs[proto.ProbeZone(region)] = d
		go d.Run(ctx)
	}
	return so
}

// startPublicNode 起一台用自签证书、并入自组网的节点，返回服务和它的公开地址。
func startPublicNode(t *testing.T, so *selfOrg, name, region string) *Server {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath, err := GenerateSelfSigned(filepath.Join(dir, "tls"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Node.Name, cfg.Node.PublicHost, cfg.Node.DataDir, cfg.Node.KeyFile = name, "127.0.0.1", dir, filepath.Join(dir, "node.key")
	cfg.Listen.TLSCert, cfg.Listen.TLSKey = certPath, keyPath
	cfg.Limits.MessagesPerSecond = 500
	cfg.Directory = DirectorySection{Enabled: true, Region: region, Status: proto.CardOpen, Roots: so.roots, ListFile: so.listF, CapacityPlayers: 100}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, so.discard)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	srv.cfg.Directory.PublicURLs = []string{"wss://" + strings.TrimPrefix(ts.URL, "https://") + proto.WSPath}
	srv.pub.s.cfg = srv.cfg
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.pub.run(ctx)
	return srv
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("等不到：" + what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func listed(t *testing.T, dirURL string, q discovery.Query, nodeID string) *discovery.Listing {
	ls, err := discovery.ListNodes(context.Background(), dirURL, q)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ls {
		if ls[i].Node.NodeID == nodeID {
			return &ls[i]
		}
	}
	return nil
}

func TestSelfOrganizingNetwork(t *testing.T) {
	old := cardEvery
	cardEvery = 300 * time.Millisecond
	defer func() { cardEvery = old }()

	so := startDirectories(t)
	srv := startPublicNode(t, so, "华东私服", "cn-east")
	id := srv.Hub.NodeID
	cn, hk := so.urls["cn"], so.urls["hk"]
	q := discovery.Query{Region: "cn-east", Protocol: proto.Protocol, IncludeNew: true}

	// 1. 节点自己登记，国内名录探测通过后公开（新节点要 include=new 才看得到）。
	waitFor(t, "国内名录公开这台节点", func() bool { return listed(t, cn, q, id) != nil })
	l := listed(t, cn, q, id)
	if l.Tier != directory.TierNew || l.Node.Name != "华东私服" || l.Node.Endpoints[0].TLSSHA256 == "" {
		t.Fatalf("%+v", l)
	}
	if listed(t, cn, discovery.Query{Region: "cn-east"}, id) != nil {
		t.Fatal("新节点不该进默认推荐")
	}
	if pub := srv.pub.status()["directories"].(map[string]string); !strings.Contains(pub[cn], "建议升级到 9.0.0") {
		t.Fatal("名录的升级提醒应当传回节点", pub)
	}

	// 2. 香港名录也有它（登记到本区＋另一区，名录之间也会同步），并从 hk 探测区探测过。
	waitFor(t, "香港名录从 hk 区探测到这台节点", func() bool {
		l := listed(t, hk, discovery.Query{Region: "hk", IncludeNew: true}, id)
		return l != nil && len(l.Probes) == 2
	})

	// 3. 客户端按名片连：指纹校验＋节点证明身份。
	c, err := client.DialNode(ctx5(), l.Node, 0, proto.GenerateKey(), map[string]any{"role": "player", "displayName": "客户端"})
	if err != nil {
		t.Fatal("按名片连节点：", err)
	}
	c.Close()
	fake := l.Node
	fake.Endpoints = []proto.Endpoint{{URL: l.Node.Endpoints[0].URL, TLSSHA256: strings.Repeat("0", 64)}}
	if _, err := client.DialNode(ctx5(), fake, 0, proto.GenerateKey(), map[string]any{"role": "player", "displayName": "x"}); err == nil {
		t.Fatal("证书指纹对不上应当连不上")
	}

	// 4. 根密钥认证后进默认推荐，认证记录同步到另一台名录。
	now := time.Now().UTC()
	e, _ := proto.SignBy(proto.CtxEndorsement, so.root, proto.Endorsement{NodeID: id, IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour), Note: "测试"})
	if err := so.dirs["cn"].AddEndorsement(e); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "认证同步到香港名录", func() bool {
		l := listed(t, hk, discovery.Query{Region: "hk"}, id)
		return l != nil && l.Tier == directory.TierVerified
	})
	stranger, _ := proto.SignBy(proto.CtxEndorsement, proto.GenerateKey(), proto.Endorsement{NodeID: id, IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
	if so.dirs["cn"].AddEndorsement(stranger) == nil {
		t.Fatal("不是根密钥签的认证不认")
	}

	// 5. 排空：名片马上改成 draining，退出推荐；凭编号还找得到。
	srv.Hub.SetDraining(true)
	srv.pub.poke()
	waitFor(t, "排空后退出推荐", func() bool { return listed(t, cn, discovery.Query{Region: "cn-east"}, id) == nil })
	if l := listed(t, cn, discovery.Query{Region: "cn-east", NodeID: id}, id); l == nil || l.Node.Status != proto.CardDraining {
		t.Fatal("排空的节点凭编号应当还找得到")
	}
	srv.Hub.SetDraining(false)
	srv.pub.poke()
	waitFor(t, "取消排空后回到推荐", func() bool { return listed(t, cn, discovery.Query{Region: "cn-east"}, id) != nil })

	// 6. 项目方下架：国内名录签下架记录，同步到香港名录后两边都不再列出。
	if err := so.dirs["cn"].Delist(id, nil, "测试下架", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "下架同步到香港名录", func() bool { return listed(t, hk, discovery.Query{Region: "hk", IncludeNew: true}, id) == nil })
	if listed(t, cn, q, id) != nil {
		t.Fatal("国内名录也该不再列出")
	}
	_ = so.dirs["cn"].Delist(id, nil, "", true)
	waitFor(t, "撤销下架", func() bool { return listed(t, hk, discovery.Query{Region: "hk"}, id) != nil })
}

func TestDirectoryListFetchAndRejectForged(t *testing.T) {
	so := startDirectories(t)
	l, err := discovery.FetchList(context.Background(), []string{so.urls["cn"] + "/dir/v1/directories.json"}, so.roots)
	if err != nil || len(l.Directories) != 2 {
		t.Fatal(l, err)
	}
	if _, err := discovery.FetchList(context.Background(), []string{so.urls["cn"] + "/dir/v1/directories.json"}, []string{proto.IDOfKey(proto.GenerateKey())}); err == nil {
		t.Fatal("根公钥对不上的清单不认")
	}
}
