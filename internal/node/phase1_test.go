package node

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tablenet/pkg/client"
	"tablenet/pkg/proto"
)

func TestNodeProvesIdentity(t *testing.T) {
	n := startNode(t, nil)
	key := proto.GenerateKey()
	hello := func() map[string]any {
		return map[string]any{"role": "player", "displayName": "验证者", "venues": []string{"native"}, "codecs": []string{"arcane.v0"}}
	}
	c, err := client.DialWith(ctx5(), n.url, key, hello(), client.Options{ExpectNodeID: n.srv.Hub.NodeID})
	if err != nil {
		t.Fatal("真节点应当证明得了自己：", err)
	}
	c.Close()
	// 冒充：客户端以为连的是另一台节点。
	_, err = client.DialWith(ctx5(), n.url, key, hello(), client.Options{ExpectNodeID: proto.IDOfKey(proto.GenerateKey())})
	if err == nil || !strings.Contains(err.Error(), "不是要连的节点") {
		t.Fatal("编号对不上要拒", err)
	}
	// 坏的 nodeChallenge 要拒。
	if _, err := client.Dial(ctx5(), n.url, key, map[string]any{"role": "player", "displayName": "x", "nodeChallenge": "b64:AAAA"}); err == nil {
		t.Fatal("太短的 nodeChallenge 要拒")
	}
}

func TestDrainRefusesNewGames(t *testing.T) {
	n := startNode(t, nil)
	host := native(t, n, "房主")
	roomID := openRoom(t, host, nil)
	n.srv.Hub.SetDraining(true)
	e, err := host.Request(ctx5(), "room.open", map[string]any{"format": "modern", "codec": "arcane.v0"})
	mustError(t, e, err, proto.CodeDraining)
	guest := native(t, n, "客人")
	e, err = guest.Request(ctx5(), "room.join", map[string]any{"roomId": roomID})
	mustError(t, e, err, proto.CodeDraining)
	e, err = guest.Request(ctx5(), "queue.enter", map[string]any{"pools": []map[string]string{{"format": "modern"}}})
	mustError(t, e, err, proto.CodeDraining)
	n.srv.Hub.SetDraining(false)
	e, err = guest.Request(ctx5(), "room.join", map[string]any{"roomId": roomID})
	must(t, e, err, "room.joined")
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	tweak := func(c *Config) {
		c.Node.DataDir = dir
		c.Node.KeyFile = filepath.Join(dir, "node.key")
	}
	n1 := startNode(t, tweak)
	key := proto.GenerateKey()
	host := dialPlayer(t, n1, key, "房主", []string{"native"}, []string{"arcane.v0"})
	roomID := openRoom(t, host, nil)
	q := native(t, n1, "排队的人")
	enter(t, q, nil)
	if err := n1.srv.Hub.SaveState(); err != nil {
		t.Fatal(err)
	}

	// 「重启」：同一个数据目录起一台新节点。
	n2 := startNode(t, tweak)
	if n2.srv.Hub.NodeID != n1.srv.Hub.NodeID {
		t.Fatal("同一个密钥文件，节点编号应当不变")
	}
	rooms, tickets, err := n2.srv.Hub.LoadState()
	if err != nil || rooms != 1 || tickets != 1 {
		t.Fatal(rooms, tickets, err)
	}
	back := dialPlayer(t, n2, key, "房主", []string{"native"}, []string{"arcane.v0"})
	resumed := back.Welcome["resumed"].(map[string]any)["rooms"].([]any)
	if len(resumed) != 1 || resumed[0] != roomID {
		t.Fatal("重启后房主重连应当接回房间", back.Welcome)
	}
	g := native(t, n2, "客人")
	e, err := g.Request(ctx5(), "room.join", map[string]any{"roomId": roomID})
	must(t, e, err, "room.joined")
}

func TestMetrics(t *testing.T) {
	n := startNode(t, nil)
	_ = native(t, n, "在线的人")
	pub := httptest.NewServer(n.srv.Handler())
	defer pub.Close()
	resp, err := http.Get(pub.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"tablenet_players 1", "tablenet_rooms 0", "# TYPE tablenet_relay_bytes_total counter"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("缺 %q：\n%s", want, b)
		}
	}
}

func TestPublisherCard(t *testing.T) {
	dir := t.TempDir()
	cert, key, err := GenerateSelfSigned(filepath.Join(dir, "tls"), []string{"203.0.113.7"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Node.Name, cfg.Node.PublicHost, cfg.Node.DataDir, cfg.Node.KeyFile = "华东私服", "203.0.113.7", dir, filepath.Join(dir, "node.key")
	cfg.Listen.WS, cfg.Listen.TLSCert, cfg.Listen.TLSKey = "0.0.0.0:8443", cert, key
	cfg.Directory = DirectorySection{Enabled: true, Region: "cn-east"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := srv.pub.Card()
	if err != nil {
		t.Fatal(err)
	}
	c, err := proto.VerifyCard(s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := CertFingerprint(cert)
	if c.Status != proto.CardPrivate || c.Region != "cn-east" || c.NodeID != srv.Hub.NodeID ||
		c.Endpoints[0].URL != "wss://203.0.113.7:8443/tablenet/1" || c.Endpoints[0].TLSSHA256 != fp || len(fp) != 64 {
		t.Fatalf("%+v", c)
	}
	srv.Hub.SetDraining(true)
	s, _ = srv.pub.Card()
	if c, _ := proto.VerifyCard(s, time.Now()); c.Status != proto.CardDraining {
		t.Fatal("排空时名片要标 draining")
	}
	// 地区不认、名字过长都在配置阶段就拦下。
	bad := cfg
	bad.Directory.Region = "mars"
	if bad.Validate() == nil {
		t.Fatal("地区不认要报错")
	}
}

// codecInfo：节点不解释，原样存、原样给；更新可换；限长、只认对象；重启后还在。
func TestCodecInfoRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tweak := func(c *Config) {
		c.Node.DataDir = dir
		c.Node.KeyFile = filepath.Join(dir, "node.key")
	}
	n := startNode(t, tweak)
	host := native(t, n, "房主")
	roomID := openRoom(t, host, map[string]any{"hasPassword": false, "codecInfo": map[string]any{"seatMode": "archenemy", "variants": []string{"planechase"}}})
	g := native(t, n, "客人")
	info := func() map[string]any {
		e, err := g.Request(ctx5(), "room.list", nil)
		rooms := must(t, e, err, "room.listing")["rooms"].([]any)
		if len(rooms) != 1 {
			t.Fatalf("房间数 %d", len(rooms))
		}
		ci, _ := rooms[0].(map[string]any)["codecInfo"].(map[string]any)
		return ci
	}
	if ci := info(); ci["seatMode"] != "archenemy" {
		t.Fatalf("开房带的 codecInfo 没原样给：%v", ci)
	}
	e, err := host.Request(ctx5(), "room.update", map[string]any{"roomId": roomID, "codecInfo": map[string]any{"seatMode": "duel"}})
	must(t, e, err, "room.updated")
	if ci := info(); ci["seatMode"] != "duel" || ci["variants"] != nil {
		t.Fatalf("更新应整份替换：%v", ci)
	}
	e, err = host.Request(ctx5(), "room.update", map[string]any{"roomId": roomID, "codecInfo": []int{1}})
	mustError(t, e, err, proto.CodeBadRequest)
	e, err = host.Request(ctx5(), "room.update", map[string]any{"roomId": roomID, "codecInfo": map[string]any{"x": strings.Repeat("a", MaxCodecInfo)}})
	mustError(t, e, err, proto.CodeBadRequest)
	if err := n.srv.Hub.SaveState(); err != nil {
		t.Fatal(err)
	}
	n2 := startNode(t, tweak)
	if _, _, err := n2.srv.Hub.LoadState(); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if json.Unmarshal(n2.srv.Hub.rooms[roomID].codecInfo, &back) != nil || back["seatMode"] != "duel" {
		t.Fatalf("重启后 codecInfo 丢了：%s", n2.srv.Hub.rooms[roomID].codecInfo)
	}
}

// 只报房号也进得去（玩家口头报的是那 6 位），大小写不论。
func TestJoinByBareCode(t *testing.T) {
	n := startNode(t, nil)
	host := native(t, n, "房主")
	roomID := openRoom(t, host, map[string]any{"hasPassword": false})
	code := strings.ToLower(roomID[strings.Index(roomID, "/")+1:])
	g := native(t, n, "客人")
	e, err := g.Request(ctx5(), "room.join", map[string]any{"roomId": code})
	must(t, e, err, "room.joined")
}

// 指定房号：新开就用这个号；自己的房再开一次是接着用（排队房的私密与邀请名单保留）；别人的号拒掉。
func TestRequestedRoomCode(t *testing.T) {
	n := startNode(t, nil)
	key := proto.GenerateKey()
	host := dialPlayer(t, n, key, "房主", []string{"native"}, []string{"arcane.v0"})
	id := openRoom(t, host, map[string]any{"code": "rm42ab", "hasPassword": false})
	if id != "test.local/RM42AB" {
		t.Fatalf("指定房号没用上：%s", id)
	}
	again := openRoom(t, host, map[string]any{"code": "RM42AB", "title": "再来一局", "hasPassword": false})
	if again != id || n.srv.Hub.rooms[id].title != "再来一局" || len(n.srv.Hub.rooms) != 1 {
		t.Fatalf("自己的房重开应当接着用同一间：%s", again)
	}
	other := native(t, n, "别人")
	e, err := other.Request(ctx5(), "room.open", map[string]any{"title": "x", "format": "modern", "codec": "arcane.v0", "code": "RM42AB"})
	mustError(t, e, err, proto.CodeRoomTaken)
	e, err = other.Request(ctx5(), "room.open", map[string]any{"title": "x", "format": "modern", "codec": "arcane.v0", "code": "a!"})
	mustError(t, e, err, proto.CodeBadRequest)
	// 节点替你开的私密房（排队配上）：重开不变公开，邀请名单还在
	n.srv.Hub.rooms[id].visibility = "private"
	n.srv.Hub.rooms[id].allowed = map[string]bool{"ed25519:friend": true}
	openRoom(t, host, map[string]any{"code": "RM42AB", "visibility": "public", "hasPassword": false})
	if r := n.srv.Hub.rooms[id]; r.visibility != "private" || !r.allowed["ed25519:friend"] {
		t.Fatalf("排队房重开丢了私密或名单：%v %v", r.visibility, r.allowed)
	}
}

// 暂停排队：位置留着、不配对；恢复后立刻配。
func TestQueuePauseResume(t *testing.T) {
	n := startNode(t, nil)
	a, b := native(t, n, "甲"), native(t, n, "乙")
	enter(t, a, nil)
	e, err := a.Request(ctx5(), "queue.pause", nil)
	must(t, e, err, "queue.paused")
	enter(t, b, nil)
	time.Sleep(100 * time.Millisecond)
	if len(n.srv.Hub.proposals) != 0 {
		t.Fatal("暂停的人不该被配上")
	}
	e, err = a.Request(ctx5(), "queue.resume", nil)
	must(t, e, err, "queue.resumed")
	exp(t, a, "queue.match")
	exp(t, b, "queue.match")
}
