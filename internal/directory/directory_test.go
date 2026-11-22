package directory

import (
	"crypto/ed25519"
	"io"
	"log/slog"
	"testing"
	"time"

	"tablenet/pkg/proto"
)

func newTestDir(t *testing.T) (*Directory, ed25519.PrivateKey, ed25519.PrivateKey) {
	t.Helper()
	root, key := proto.GenerateKey(), proto.GenerateKey()
	now := time.Now()
	list, _ := proto.SignDirectoryList(root, proto.DirectoryList{Seq: 1, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		Directories: []proto.DirectoryEntry{{ID: proto.IDOfKey(key), URL: "http://127.0.0.1:1", Region: "cn-east"}}})
	d, err := New(Config{Region: "cn-east", Roots: []string{proto.IDOfKey(root)}, MaxNodesPerIP: 2, NewPerPrefixPerHour: 3},
		key, list, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return d, root, key
}

func card(t *testing.T, k ed25519.PrivateKey, seq int64, version string) proto.Signed {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	c := proto.NodeCard{Seq: seq, IssuedAt: now, ExpiresAt: now.Add(20 * time.Minute), Name: "测试节点", Region: "cn-east",
		Endpoints: []proto.Endpoint{{URL: "wss://203.0.113.7:8443/tablenet/1"}}, Protocols: []string{proto.Protocol}, Status: proto.CardOpen}
	c.Software.Version = version
	s, err := proto.SignCard(k, c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRegisterSeqAndUpgrade(t *testing.T) {
	d, _, _ := newTestDir(t)
	d.cfg.MinVersion = "0.3.0"
	k := proto.GenerateKey()
	r, err := d.Register(card(t, k, 10, "0.2.5"), "198.51.100.1")
	if err != nil || r.Listed || r.Reasons[0] != "probe_pending" || r.Upgrade == nil {
		t.Fatal(r, err)
	}
	r, _ = d.Register(card(t, k, 5, "0.3.0"), "198.51.100.1")
	if r.Seq != 10 || r.Reasons[0] != "stale_seq" {
		t.Fatal("旧 seq 的名片要被拒（防重放）", r)
	}
	r, _ = d.Register(card(t, k, 11, "0.3.0"), "198.51.100.1")
	if r.Seq != 11 || r.Upgrade != nil {
		t.Fatal(r)
	}
	if _, err := d.Register(proto.Signed{Text: "{}", Sig: "b64:"}, ""); err == nil {
		t.Fatal("坏名片要报错")
	}
}

func TestRegistrationRateLimits(t *testing.T) {
	d, _, _ := newTestDir(t)
	ok := 0
	for i := 0; i < 5; i++ {
		r, _ := d.Register(card(t, proto.GenerateKey(), 1, "0.2.0"), "198.51.100.7")
		if len(r.Reasons) == 0 || r.Reasons[0] == "probe_pending" {
			ok++
		}
	}
	if ok != 2 {
		t.Fatal("同一地址最多挂 2 台（测试配置）", ok)
	}
	// 同一 /24 的别的地址：每小时最多 3 次新登记（前面已用掉 2 次）。
	r, _ := d.Register(card(t, proto.GenerateKey(), 1, "0.2.0"), "198.51.100.8")
	if r.Reasons[0] != "probe_pending" {
		t.Fatal(r)
	}
	r, _ = d.Register(card(t, proto.GenerateKey(), 1, "0.2.0"), "198.51.100.9")
	if r.Reasons[0] != "rate_limited" {
		t.Fatal("同一地址段登记太多要限流", r)
	}
	// 已登记的节点续期不受限。
	if prefixOf("2001:db8:1:2::5") != "2001:db8:1" || prefixOf("203.0.113.9") != "203.0.113" {
		t.Fatal("地址段算错")
	}
}

func TestObservationSignerMustBeListed(t *testing.T) {
	d, _, key := newTestDir(t)
	k := proto.GenerateKey()
	_, _ = d.Register(card(t, k, 1, "0.2.0"), "")
	obs := proto.Observation{NodeID: proto.IDOfKey(k), Seq: 1, Zone: "cn", OK: true, RTTMs: 30, At: time.Now().UTC()}
	stranger, _ := proto.SignBy(proto.CtxObservation, proto.GenerateKey(), obs)
	if d.AddObservation(stranger) == nil {
		t.Fatal("不在清单里的探测者不认")
	}
	wrongZone := obs
	wrongZone.Zone = "hk"
	s, _ := proto.SignBy(proto.CtxObservation, key, wrongZone)
	if d.AddObservation(s) == nil {
		t.Fatal("探测区和清单不符不认")
	}
	s, _ = proto.SignBy(proto.CtxObservation, key, obs)
	if err := d.AddObservation(s); err != nil {
		t.Fatal(err)
	}
	if ls := d.Nodes(Query{Region: "cn-east", IncludeNew: true}); len(ls) != 1 {
		t.Fatal("探测通过后应当公开", ls)
	}
	// 连续 3 次失败就对这个区隐藏。
	for i := 1; i <= 3; i++ {
		bad := obs
		bad.OK, bad.At = false, time.Now().UTC().Add(time.Duration(i)*time.Second)
		s, _ := proto.SignBy(proto.CtxObservation, key, bad)
		_ = d.AddObservation(s)
		got := len(d.Nodes(Query{Region: "cn-east", IncludeNew: true}))
		if (i < 3 && got != 1) || (i == 3 && got != 0) {
			t.Fatalf("第 %d 次失败后列出 %d 台", i, got)
		}
	}
}

func TestDelistingMustComeFromDirectory(t *testing.T) {
	d, _, _ := newTestDir(t)
	fake, _ := proto.SignBy(proto.CtxDelisting, proto.GenerateKey(), proto.Delisting{NodeID: "ed25519:x", At: time.Now()})
	if d.AddDelisting(fake) == nil {
		t.Fatal("不是名录签的下架记录不认")
	}
	if err := d.Delist("ed25519:x", []string{"intl"}, "测试", false); err != nil {
		t.Fatal(err)
	}
}

func TestSaveLoad(t *testing.T) {
	d, _, key := newTestDir(t)
	k := proto.GenerateKey()
	_, _ = d.Register(card(t, k, 7, "0.2.0"), "")
	s, _ := proto.SignBy(proto.CtxObservation, key, proto.Observation{NodeID: proto.IDOfKey(k), Seq: 7, Zone: "cn", OK: true, RTTMs: 12, At: time.Now().UTC()})
	_ = d.AddObservation(s)
	_ = d.Delist(proto.IDOfKey(proto.GenerateKey()), nil, "", false)
	dir := t.TempDir()
	if err := d.Save(dir); err != nil {
		t.Fatal(err)
	}
	d2, _, _ := newTestDir(t)
	d2.list, d2.cfg.Roots = d.list, d.cfg.Roots
	if err := d2.Load(dir); err != nil {
		t.Fatal(err)
	}
	if ls := d2.Nodes(Query{Region: "cn-east", IncludeNew: true}); len(ls) != 1 || len(ls[0].Probes) != 1 {
		t.Fatal("重启后名片和探测结果应当接着用", ls)
	}
	if len(d2.delist) != 1 {
		t.Fatal("下架记录要读回来")
	}
}

// 只有一台香港名录（11-22 用户定）：国内没有探测点，国内客户端查节点时不按 cn 区过滤，香港连得上就算。
func TestSingleHKDirectory(t *testing.T) {
	root, key := proto.GenerateKey(), proto.GenerateKey()
	now := time.Now()
	list, _ := proto.SignDirectoryList(root, proto.DirectoryList{Seq: 1, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		Directories: []proto.DirectoryEntry{{ID: proto.IDOfKey(key), URL: "http://127.0.0.1:1", Region: "hk"}}})
	d, err := New(Config{Region: "hk", Roots: []string{proto.IDOfKey(root)}}, key, list, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	k := proto.GenerateKey()
	_, _ = d.Register(card(t, k, 1, "0.2.0"), "")
	s, _ := proto.SignBy(proto.CtxObservation, key, proto.Observation{NodeID: proto.IDOfKey(k), Seq: 1, Zone: "hk", OK: true, RTTMs: 40, At: time.Now().UTC()})
	if err := d.AddObservation(s); err != nil {
		t.Fatal(err)
	}
	for _, region := range []string{"cn-east", "hk", "eu"} {
		if ls := d.Nodes(Query{Region: region, IncludeNew: true}); len(ls) != 1 {
			t.Fatalf("%s 的客户端应当查得到：%v", region, ls)
		}
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"0.2.0", "0.10.0", true}, {"0.10.0", "0.2.0", false}, {"1.0.0-dev", "1.0.0", false}, {"0.1.9", "0.2", true}} {
		if versionLess(c.a, c.b) != c.less {
			t.Error(c)
		}
	}
}
