package node

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"tablenet/adapters/example"
	"tablenet/pkg/client"
	"tablenet/pkg/proto"
)

// ---- 测试用的节点和玩家 ----

type testNode struct {
	srv *Server
	url string
	dir string
}

func startNode(t *testing.T, tweak func(*Config)) *testNode {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Node.PublicHost = "test.local"
	cfg.Node.DataDir = dir
	cfg.Node.KeyFile = filepath.Join(dir, "node.key")
	cfg.Limits.MessagesPerSecond = 500
	cfg.Adapters = map[string]AdapterConfig{example.PlatformID: {Servers: []string{"xmage.example.org"}}}
	if tweak != nil {
		tweak(&cfg)
	}
	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-tk.C:
				srv.Hub.Tick()
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop); ts.Close() })
	return &testNode{srv: srv, url: "ws" + strings.TrimPrefix(ts.URL, "http") + proto.WSPath, dir: dir}
}

func ctx5() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = cancel // 测试进程很短，交给超时回收
	return ctx
}

func dialPlayer(t *testing.T, n *testNode, key ed25519.PrivateKey, name string, venues, codecs []string) *client.Client {
	t.Helper()
	if key == nil {
		key = proto.GenerateKey()
	}
	c, err := client.Dial(ctx5(), n.url, key, map[string]any{
		"role": "player", "displayName": name, "venues": venues, "codecs": codecs,
		"client": map[string]string{"name": "test", "version": "0"},
	})
	if err != nil {
		t.Fatalf("%s 连不上：%v", name, err)
	}
	t.Cleanup(c.Close)
	return c
}

func native(t *testing.T, n *testNode, name string) *client.Client {
	return dialPlayer(t, n, nil, name, []string{"native"}, []string{"arcane.v0"})
}

func must(t *testing.T, e proto.Envelope, err error, want string) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("等 %s：%v", want, err)
	}
	if e.Type != want {
		t.Fatalf("要 %s，收到 %s：%s", want, e.Type, e.Body)
	}
	return client.DecodeBody(e)
}

func mustError(t *testing.T, e proto.Envelope, err error, code string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if e.ErrorCode() != code {
		t.Fatalf("要错误 %s，收到 %s：%s", code, e.Type, e.Body)
	}
}

func openRoom(t *testing.T, host *client.Client, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"title": "来打摩登", "format": "modern", "gameType": "duel", "codec": "arcane.v0",
		"seats": map[string]int{"total": 2}, "hasPassword": true}
	for k, v := range extra {
		body[k] = v
	}
	e, err := host.Request(ctx5(), "room.open", body)
	m := must(t, e, err, "room.opened")
	return m["room"].(map[string]any)["roomId"].(string)
}

// ---- 握手 ----

func TestHandshakeRejectsBadSignature(t *testing.T) {
	n := startNode(t, nil)
	ws, _, err := websocket.Dial(ctx5(), n.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	_, data, _ := ws.Read(ctx5())
	if e, _ := proto.Parse(data); e.Type != "node.hello" {
		t.Fatalf("先来的不是 node.hello：%s", data)
	}
	k := proto.GenerateKey()
	bad := proto.New("hello", "c1", "", map[string]any{"protocol": proto.Protocol, "role": "player", "displayName": "x",
		"playerId": proto.IDOfKey(k), "sig": proto.SignHello(k, []byte("别的随机数"), "ed25519:x")})
	_ = ws.Write(ctx5(), websocket.MessageText, bad.Bytes())
	_, data, err = ws.Read(ctx5())
	if err != nil {
		t.Fatal(err)
	}
	e, _ := proto.Parse(data)
	if e.ErrorCode() != proto.CodeBadSignature {
		t.Fatalf("%s", data)
	}
	if _, _, err := ws.Read(ctx5()); err == nil {
		t.Fatal("签名不对之后应当被断开")
	}
}

func TestMustHelloFirst(t *testing.T) {
	n := startNode(t, nil)
	ws, _, _ := websocket.Dial(ctx5(), n.url, nil)
	defer ws.CloseNow()
	_, _, _ = ws.Read(ctx5())
	_ = ws.Write(ctx5(), websocket.MessageText, proto.New("room.list", "c1", "", nil).Bytes())
	_, data, _ := ws.Read(ctx5())
	if e, _ := proto.Parse(data); e.ErrorCode() != proto.CodeNotAuthenticated {
		t.Fatalf("%s", data)
	}
}

// ---- 原生房：开房、大厅、入座、通道 ----

func TestNativeRoomChannelRelay(t *testing.T) {
	n := startNode(t, nil)
	host := native(t, n, "小明")
	watcher := native(t, n, "看客")
	if e, err := watcher.Request(ctx5(), "room.watch", nil); err != nil || e.Type != "room.watching" {
		t.Fatal(e, err)
	}
	roomID := openRoom(t, host, nil)
	if !strings.HasPrefix(roomID, "test.local/") || !proto.ValidRoomCode(strings.TrimPrefix(roomID, "test.local/")) {
		t.Fatal(roomID)
	}
	upd := exp(t, watcher, "room.updated")
	if upd["room"].(map[string]any)["roomId"] != roomID {
		t.Fatal(upd)
	}

	guest := native(t, n, "阿强")
	e, err := guest.Request(ctx5(), "room.list", map[string]any{"format": "modern"})
	rooms := must(t, e, err, "room.listing")["rooms"].([]any)
	if len(rooms) != 1 || rooms[0].(map[string]any)["host"].(map[string]any)["displayName"] != "小明" {
		t.Fatalf("%v", rooms)
	}

	e, err = guest.Request(ctx5(), "room.join", map[string]any{"roomId": roomID, "password": "hunter2"})
	joined := must(t, e, err, "room.joined")
	ch := uint32(joined["channel"].(float64))
	opened := exp(t, host, "channel.opened")
	if uint32(opened["channel"].(float64)) != ch || opened["password"] != "hunter2" ||
		opened["player"].(map[string]any)["playerId"] != guest.PlayerID {
		t.Fatalf("%v", opened)
	}

	// 通道是透明管子：两头的字节原样到对面。
	_ = guest.SendFrame(ctx5(), ch, []byte(`{"type":"join"}`+"\n"))
	if gotCh, p, err := host.ExpectFrame(ctx5()); err != nil || gotCh != ch || string(p) != `{"type":"join"}`+"\n" {
		t.Fatal(gotCh, string(p), err)
	}
	big := make([]byte, 200_000)
	for i := range big {
		big[i] = byte(i)
	}
	_ = host.SendFrame(ctx5(), ch, big)
	if _, p, err := guest.ExpectFrame(ctx5()); err != nil || len(p) != len(big) || p[12345] != big[12345] {
		t.Fatal(len(p), err)
	}
	// 不是通道两端的人发帧，节点不转。
	_ = watcher.SendFrame(ctx5(), ch, []byte("偷发"))

	e, err = guest.Request(ctx5(), "channel.close", map[string]any{"channel": ch, "code": "bye"})
	must(t, e, err, "channel.closed")
	closed := exp(t, host, "channel.closed")
	if closed["code"] != "bye" {
		t.Fatal(closed)
	}

	// 房主关房，大厅里跟着消失。
	e, err = host.Request(ctx5(), "room.close", map[string]any{"roomId": roomID})
	must(t, e, err, "room.closed")
	gone := exp(t, watcher, "room.removed")
	if gone["roomId"] != roomID {
		t.Fatal(gone)
	}
}

func exp(t *testing.T, c *client.Client, typ string) map[string]any {
	t.Helper()
	e, err := c.Expect(ctx5(), typ)
	return must(t, e, err, typ)
}

func TestJoinChecks(t *testing.T) {
	n := startNode(t, nil)
	host := native(t, n, "房主")
	roomID := openRoom(t, host, map[string]any{"spectate": map[string]bool{"allowed": false}})

	noCodec := dialPlayer(t, n, nil, "老客户端", []string{"native"}, []string{"other.v9"})
	e, err := noCodec.Request(ctx5(), "room.join", map[string]any{"roomId": roomID})
	mustError(t, e, err, proto.CodeCodecMismatch)

	e, err = noCodec.Request(ctx5(), "room.join", map[string]any{"roomId": roomID, "as": "spectator"})
	mustError(t, e, err, proto.CodeSpectateDisabled)

	e, err = noCodec.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/ZZZZZZ"})
	mustError(t, e, err, proto.CodeNoRoom)

	other := native(t, n, "别人")
	e, err = other.Request(ctx5(), "room.close", map[string]any{"roomId": roomID})
	mustError(t, e, err, proto.CodeNotOwner)

	// 开房数上限（默认每人 2 间）。
	openRoom(t, host, nil)
	e, err = host.Request(ctx5(), "room.open", map[string]any{"format": "modern", "codec": "arcane.v0"})
	mustError(t, e, err, proto.CodeLimitReached)

	// 只声明了外部平台的客户端不能开原生房。
	ext := dialPlayer(t, n, nil, "外部", []string{"example"}, nil)
	e, err = ext.Request(ctx5(), "room.open", map[string]any{"format": "modern", "codec": "arcane.v0"})
	mustError(t, e, err, proto.CodeBadRequest)
}

// ---- 掉线挂起与续接 ----

func TestHostDisconnectSuspendsAndResumes(t *testing.T) {
	n := startNode(t, nil)
	key := proto.GenerateKey()
	host := dialPlayer(t, n, key, "房主", []string{"native"}, []string{"arcane.v0"})
	roomID := openRoom(t, host, nil)
	host.Close()

	guest := native(t, n, "客人")
	deadline := time.Now().Add(3 * time.Second)
	for {
		e, err := guest.Request(ctx5(), "room.list", nil)
		if len(must(t, e, err, "room.listing")["rooms"].([]any)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("房主掉线后房间应当从大厅消失（挂起）")
		}
		time.Sleep(20 * time.Millisecond)
	}
	e, err := guest.Request(ctx5(), "room.join", map[string]any{"roomId": roomID})
	mustError(t, e, err, proto.CodeHostOffline)

	back := dialPlayer(t, n, key, "房主", []string{"native"}, []string{"arcane.v0"})
	resumed := back.Welcome["resumed"].(map[string]any)["rooms"].([]any)
	if len(resumed) != 1 || resumed[0] != roomID {
		t.Fatalf("重连应当接回房间：%v", back.Welcome)
	}
	e, err = guest.Request(ctx5(), "room.join", map[string]any{"roomId": roomID})
	must(t, e, err, "room.joined")
}

func TestSecondLoginReplacesFirst(t *testing.T) {
	n := startNode(t, nil)
	key := proto.GenerateKey()
	first := dialPlayer(t, n, key, "我", []string{"native"}, []string{"arcane.v0"})
	_ = dialPlayer(t, n, key, "我", []string{"native"}, []string{"arcane.v0"})
	e, err := first.Expect(ctx5(), "error")
	mustError(t, e, err, proto.CodeReplaced)
	select {
	case <-first.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("旧连接应当被断开")
	}
}

// ---- 排队匹配 ----

func enter(t *testing.T, c *client.Client, extra map[string]any) {
	t.Helper()
	body := map[string]any{"pools": []map[string]string{{"format": "modern", "gameType": "duel", "structure": "bo1"}}}
	for k, v := range extra {
		body[k] = v
	}
	e, err := c.Request(ctx5(), "queue.enter", body)
	must(t, e, err, "queue.entered")
}

func TestQueueNativeMatch(t *testing.T) {
	n := startNode(t, nil)
	a, b := native(t, n, "甲"), native(t, n, "乙")
	enter(t, a, nil)
	enter(t, b, nil)
	ma := exp(t, a, "queue.match")
	mb := exp(t, b, "queue.match")
	if ma["matchId"] != mb["matchId"] || ma["venue"] != "native" {
		t.Fatal(ma, mb)
	}
	id := ma["matchId"]
	e, err := a.Request(ctx5(), "queue.accept", map[string]any{"matchId": id})
	must(t, e, err, "queue.accepted")
	e, err = b.Request(ctx5(), "queue.accept", map[string]any{"matchId": id})
	must(t, e, err, "queue.accepted")

	hostMsg := exp(t, a, "match.host") // 先接受的当主机
	joinMsg := exp(t, b, "match.join")
	room := hostMsg["room"].(map[string]any)
	if room["roomId"] != joinMsg["roomId"] || room["visibility"] != "private" {
		t.Fatal(hostMsg, joinMsg)
	}
	// 私密房：配对之外的人进不去，配对的人进得去。
	outsider := native(t, n, "路人")
	e, err = outsider.Request(ctx5(), "room.join", map[string]any{"roomId": room["roomId"]})
	mustError(t, e, err, proto.CodeNoRoom)
	e, err = b.Request(ctx5(), "room.join", map[string]any{"roomId": room["roomId"]})
	must(t, e, err, "room.joined")
	exp(t, a, "channel.opened")
}

func TestQueueDeclineRequeuesOther(t *testing.T) {
	n := startNode(t, nil)
	a, b := native(t, n, "甲"), native(t, n, "乙")
	enter(t, a, nil)
	enter(t, b, nil)
	m := exp(t, a, "queue.match")
	_ = exp(t, b, "queue.match")
	e, err := b.Request(ctx5(), "queue.decline", map[string]any{"matchId": m["matchId"]})
	must(t, e, err, "queue.declined")
	exp(t, a, "queue.match_cancelled")
	exp(t, b, "queue.left")

	c := native(t, n, "丙")
	enter(t, c, nil)
	m2 := exp(t, a, "queue.match")
	if m2["opponents"].([]any)[0].(map[string]any)["displayName"] != "丙" {
		t.Fatal("甲应当还在排队，并配上丙", m2)
	}
}

func TestQueueNoMatchAcrossPools(t *testing.T) {
	n := startNode(t, nil)
	a, b := native(t, n, "甲"), native(t, n, "乙")
	enter(t, a, nil)
	enter(t, b, map[string]any{"pools": []map[string]string{{"format": "pioneer"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if e, err := a.Expect(ctx, "queue.match"); err == nil {
		t.Fatal("不同池子不该配上", e)
	}
}

func TestPairUpRules(t *testing.T) {
	modern := pool{"modern", "duel", "bo1"}
	any := func(string, pool) bool { return true }
	tk := func(venues, codecs []string, pools ...pool) *ticket {
		return &ticket{venues: venues, codecs: codecs, pools: pools}
	}
	cases := []struct {
		name         string
		a, b         *ticket
		ok           bool
		venue, codec string
	}{
		{"原生优先", tk([]string{"native", "xmage"}, []string{"arcane.v0"}, modern), tk([]string{"xmage", "native"}, []string{"arcane.v0"}, modern), true, "native", "arcane.v0"},
		{"编码不交就去外部平台", tk([]string{"native", "xmage"}, []string{"arcane.v0"}, modern), tk([]string{"native", "xmage"}, []string{"other"}, modern), true, "xmage", ""},
		{"场地不交", tk([]string{"xmage"}, nil, modern), tk([]string{"hexproof"}, nil, modern), false, "", ""},
		{"按先排的人的偏好", tk([]string{"hexproof", "xmage"}, nil, modern), tk([]string{"xmage", "hexproof"}, nil, modern), true, "hexproof", ""},
		{"池子不同", tk([]string{"native"}, []string{"arcane.v0"}, modern), tk([]string{"native"}, []string{"arcane.v0"}, pool{"modern", "duel", "bo3"}), false, "", ""},
	}
	for _, c := range cases {
		_, v, codec, ok := pairUp(c.a, c.b, any)
		if ok != c.ok || v != c.venue || codec != c.codec {
			t.Errorf("%s：得到 ok=%v venue=%q codec=%q", c.name, ok, v, codec)
		}
	}
	offline := func(v string, _ pool) bool { return v != "hexproof" }
	if _, v, _, ok := pairUp(tk([]string{"hexproof", "xmage"}, nil, modern), tk([]string{"xmage", "hexproof"}, nil, modern), offline); !ok || v != "xmage" {
		t.Errorf("适配器不在线的平台要跳过：%v %v", v, ok)
	}
}

func TestMatchTimeoutStrikes(t *testing.T) {
	n := startNode(t, func(c *Config) { c.Limits.MatchAcceptSeconds = 0 })
	a, b := native(t, n, "甲"), native(t, n, "乙")
	enter(t, a, nil)
	enter(t, b, nil)
	exp(t, a, "queue.match")
	left := exp(t, a, "queue.left")
	if left["reason"] != "timeout" {
		t.Fatal(left)
	}
}

// ---- 适配器 ----

func startExample(t *testing.T, n *testNode) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = example.Run(ctx, n.url, proto.GenerateKey(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n.srv.Hub.mu.Lock()
		cnt := 0
		for _, r := range n.srv.Hub.rooms {
			if r.venue == example.PlatformID {
				cnt++
			}
		}
		n.srv.Hub.mu.Unlock()
		if cnt == len(example.Rooms) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("示例适配器没上线")
}

func TestAdapterHandoffAndBridge(t *testing.T) {
	n := startNode(t, nil)
	startExample(t, n)
	p := dialPlayer(t, n, nil, "玩家", []string{"native", "example"}, []string{"arcane.v0", example.Codec})
	if pl := p.Welcome["platforms"].([]any); len(pl) != 1 || pl[0].(map[string]any)["platformId"] != "example" {
		t.Fatalf("welcome 里应当列出在线平台：%v", p.Welcome)
	}
	e, err := p.Request(ctx5(), "room.list", map[string]any{"venue": "example"})
	rooms := must(t, e, err, "room.listing")["rooms"].([]any)
	if len(rooms) != 2 {
		t.Fatal(rooms)
	}

	e, err = p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/example-lobby-1"})
	ho := must(t, e, err, "room.joined")["handoff"].(map[string]any)
	if !strings.HasPrefix(ho["url"].(string), example.Homepage+"/t/lobby-1") {
		t.Fatal(ho)
	}

	e, err = p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/example-echo-1"})
	ch := uint32(must(t, e, err, "room.joined")["channel"].(float64))
	_ = p.SendFrame(ctx5(), ch, []byte("你好"))
	if gotCh, got, err := p.ExpectFrame(ctx5()); err != nil || gotCh != ch || string(got) != "echo: 你好" {
		t.Fatal(string(got), err)
	}

	// 解不了桥接编码的玩家：示例平台也支持交接，就退成交接。
	q := dialPlayer(t, n, nil, "旧客户端", []string{"example"}, nil)
	e, err = q.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/example-echo-1"})
	if must(t, e, err, "room.joined")["handoff"] == nil {
		t.Fatal("应当退成交接")
	}
}

func TestQueueLandsOnAdapterVenue(t *testing.T) {
	n := startNode(t, nil)
	startExample(t, n)
	a := dialPlayer(t, n, nil, "能桥接", []string{"example"}, []string{example.Codec})
	b := dialPlayer(t, n, nil, "只能交接", []string{"example"}, nil)
	enter(t, a, nil)
	enter(t, b, nil)
	m := exp(t, a, "queue.match")
	if m["venue"] != "example" {
		t.Fatal(m)
	}
	exp(t, b, "queue.match")
	for _, c := range []*client.Client{a, b} {
		e, err := c.Request(ctx5(), "queue.accept", map[string]any{"matchId": m["matchId"]})
		must(t, e, err, "queue.accepted")
	}
	br := exp(t, a, "match.bridged")
	ho := exp(t, b, "match.handoff")
	if ho["handoff"].(map[string]any)["url"] == nil || br["channel"] == nil {
		t.Fatal(br, ho)
	}
	ch := uint32(br["channel"].(float64))
	_ = a.SendFrame(ctx5(), ch, []byte("开打"))
	if _, got, err := a.ExpectFrame(ctx5()); err != nil || string(got) != "echo: 开打" {
		t.Fatal(string(got), err)
	}
}

// fakeAdapter：按给定的回话应答入座请求，用来测节点对坏回话的处理。
func fakeAdapter(t *testing.T, n *testNode, key ed25519.PrivateKey, seat map[string]any) *client.Client {
	t.Helper()
	ad, err := client.Dial(ctx5(), n.url, key, example.Hello())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ad.Close)
	e, err := ad.Request(ctx5(), "venue.room.upsert", map[string]any{"localId": "r1", "format": "modern", "join": "handoff"})
	must(t, e, err, "venue.room.upserted")
	go func() {
		for {
			m, err := ad.Next(context.Background())
			if err != nil {
				return
			}
			if m.Env.Type == "venue.seat.request" {
				if seat == nil {
					continue // 装死，测超时
				}
				_ = ad.Reply(context.Background(), m.Env.ID, "venue.seat.granted", seat)
			}
		}
	}()
	return ad
}

// 外部平台的房间从大厅撤下（比如那边开局了），正在用的桥接通道不能跟着断。
func TestVenueRemoveKeepsBridgeChannel(t *testing.T) {
	n := startNode(t, nil)
	ad, err := client.Dial(ctx5(), n.url, proto.GenerateKey(), example.Hello())
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	e, err := ad.Request(ctx5(), "venue.room.upsert", map[string]any{"localId": "b1", "format": "modern", "join": "bridge", "codec": example.Codec})
	must(t, e, err, "venue.room.upserted")
	go func() {
		for {
			m, err := ad.Next(context.Background())
			if err != nil {
				return
			}
			switch {
			case m.IsFrame():
				_ = ad.SendFrame(context.Background(), m.Channel, append([]byte("echo: "), m.Payload...))
			case m.Env.Type == "venue.seat.request":
				_ = ad.Reply(context.Background(), m.Env.ID, "venue.seat.bridged", map[string]any{})
			}
		}
	}()
	p := dialPlayer(t, n, nil, "玩家", []string{"example"}, []string{example.Codec})
	e, err = p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/example-b1"})
	ch := uint32(must(t, e, err, "room.joined")["channel"].(float64))
	if e, err := ad.Request(ctx5(), "venue.room.remove", map[string]any{"localId": "b1"}); err != nil || e.Type != "venue.room.removed" {
		t.Fatal(e, err)
	}
	_ = p.SendFrame(ctx5(), ch, []byte("还在吗"))
	if _, got, err := p.ExpectFrame(ctx5()); err != nil || string(got) != "echo: 还在吗" {
		t.Fatal("撤房后桥接通道应当还通", string(got), err)
	}
}

func TestBadHandoffIsDropped(t *testing.T) {
	cases := []map[string]any{
		{"url": "http://play.example.org/t/1"},                                 // 不是 https
		{"url": "https://evil.example.com/t/1"},                                // 不在平台域名下
		{"url": "https://play.example.org.evil.com/t/1"},                       // 仿冒后缀
		{"connect": map[string]any{"client": "xmage", "server": "evil.org:1"}}, // 服务器没登记
		{"connect": map[string]any{"server": "xmage.example.org:17171"}},       // 缺 client
		{"note": "什么都没给"},
	}
	for i, seat := range cases {
		n := startNode(t, nil)
		fakeAdapter(t, n, proto.GenerateKey(), seat)
		p := dialPlayer(t, n, nil, "玩家", []string{"example"}, nil)
		e, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/example-r1"})
		if err != nil || e.ErrorCode() != proto.CodeVenueUnavailable {
			t.Errorf("第 %d 种坏回话应当被拦下：%s %v", i, e.Body, err)
		}
	}
	// 合规的 connect 放行。
	n := startNode(t, nil)
	fakeAdapter(t, n, proto.GenerateKey(), map[string]any{"connect": map[string]any{"client": "xmage", "server": "xmage.example.org:17171", "room": "T1"}})
	p := dialPlayer(t, n, nil, "玩家", []string{"example"}, nil)
	e, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/example-r1"})
	must(t, e, err, "room.joined")
}

func TestSeatRequestTimeout(t *testing.T) {
	n := startNode(t, func(c *Config) { c.Limits.SeatRequestSeconds = 0 })
	fakeAdapter(t, n, proto.GenerateKey(), nil)
	p := dialPlayer(t, n, nil, "玩家", []string{"example"}, nil)
	e, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/example-r1"})
	mustError(t, e, err, proto.CodeVenueUnavailable)
}

func TestAdapterAdmission(t *testing.T) {
	n := startNode(t, nil)
	// 没放行的平台。
	h := example.Hello()
	h["platform"].(map[string]any)["platformId"] = "xmage"
	if _, err := client.Dial(ctx5(), n.url, proto.GenerateKey(), h); err == nil || !strings.Contains(err.Error(), proto.CodePlatformNotAllowed) {
		t.Fatal(err)
	}
	// 先到先得：第一把公钥记下后，换一把就进不来。
	k1 := proto.GenerateKey()
	first, err := client.Dial(ctx5(), n.url, k1, example.Hello())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Dial(ctx5(), n.url, k1, example.Hello()); err == nil || !strings.Contains(err.Error(), proto.CodePlatformConflict) {
		t.Fatal("同一平台不能同时两个适配器在线", err)
	}
	first.Close()
	time.Sleep(100 * time.Millisecond)
	if _, err := client.Dial(ctx5(), n.url, proto.GenerateKey(), example.Hello()); err == nil || !strings.Contains(err.Error(), proto.CodePlatformConflict) {
		t.Fatal("换了公钥应当被拒", err)
	}
	if again, err := client.Dial(ctx5(), n.url, k1, example.Hello()); err != nil {
		t.Fatal("原来的公钥应当还能上线", err)
	} else {
		again.Close()
	}
}

func TestAdapterOfflineRoomsExpire(t *testing.T) {
	n := startNode(t, nil)
	ad := fakeAdapter(t, n, proto.GenerateKey(), map[string]any{"url": example.Homepage + "/x"})
	ad.Close()
	time.Sleep(100 * time.Millisecond)
	n.srv.Hub.mu.Lock()
	n.srv.Hub.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	n.srv.Hub.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	n.srv.Hub.mu.Lock()
	left := len(n.srv.Hub.rooms)
	n.srv.Hub.mu.Unlock()
	if left != 0 {
		t.Fatal("适配器掉线一分钟后它的房间应当撤下")
	}
}

// ---- 战绩 ----

func TestResultNeedsAllSignatures(t *testing.T) {
	n := startNode(t, nil)
	host, guest := native(t, n, "房主"), native(t, n, "客人")
	roomID := openRoom(t, host, nil)
	e, err := guest.Request(ctx5(), "result.report", map[string]any{"roomId": roomID, "reason": "normal",
		"players": []map[string]any{{"playerId": host.PlayerID, "games": 0}, {"playerId": guest.PlayerID, "games": 2}}})
	mustError(t, e, err, proto.CodeNotOwner)

	e, err = host.Request(ctx5(), "result.report", map[string]any{"roomId": roomID, "reason": "normal",
		"players": []map[string]any{{"playerId": host.PlayerID, "games": 2}, {"playerId": guest.PlayerID, "games": 1}}})
	rep := must(t, e, err, "result.reported")
	text := rep["reportText"].(string)
	pend := exp(t, guest, "result.pending")
	if pend["reportText"] != text {
		t.Fatal("客人收到的原文要和房主的一样")
	}
	id := rep["resultId"]
	e, err = guest.Request(ctx5(), "result.confirm", map[string]any{"resultId": id, "sig": proto.SignResult(guest.Key(), text+"改")})
	mustError(t, e, err, proto.CodeBadSignature)
	for _, c := range []*client.Client{host, guest} {
		e, err = c.Request(ctx5(), "result.confirm", map[string]any{"resultId": id, "sig": proto.SignResult(c.Key(), text)})
		must(t, e, err, "result.signed")
	}
	exp(t, host, "result.confirmed")
	f, err := os.Open(filepath.Join(n.dir, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() || !strings.Contains(sc.Text(), id.(string)) || sc.Scan() {
		t.Fatal("results.jsonl 应当正好一行")
	}
}

// ---- 管理接口与 HTTP ----

func TestAdminAndHTTP(t *testing.T) {
	n := startNode(t, nil)
	admin := httptest.NewServer(n.srv.AdminHandler())
	defer admin.Close()
	if resp, _ := http.Get(admin.URL + "/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("没令牌应当 401")
	}
	victim := native(t, n, "捣乱的")
	req, _ := http.NewRequest(http.MethodPost, admin.URL+"/ban?player="+victim.PlayerID+"&reason=spam", nil)
	req.Header.Set("Authorization", "Bearer "+n.srv.token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatal(resp, err)
	}
	select {
	case <-victim.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("拉黑应当踢下线")
	}
	if _, err := client.Dial(ctx5(), n.url, victim.Key(), map[string]any{"role": "player", "displayName": "又来"}); err == nil ||
		!strings.Contains(err.Error(), proto.CodeBanned) {
		t.Fatal("拉黑后进不来", err)
	}

	pub := httptest.NewServer(n.srv.Handler())
	defer pub.Close()
	for _, path := range []string{"/healthz", "/node.json", "/stats.json"} {
		resp, err := http.Get(pub.URL + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatal(path, resp, err)
		}
	}
}

func TestRateLimit(t *testing.T) {
	n := startNode(t, func(c *Config) { c.Limits.MessagesPerSecond = 5 })
	p := native(t, n, "刷屏")
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		e, err := p.Request(ctx5(), "room.list", nil)
		if err != nil {
			t.Fatal(err)
		}
		limited = e.ErrorCode() == proto.CodeRateLimited
	}
	if !limited {
		t.Fatal("应当被限流")
	}
}
