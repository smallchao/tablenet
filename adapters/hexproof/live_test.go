// SPDX-License-Identifier: GPL-3.0-or-later

package adapter

// 连真 Hexproof hub 的实测。平时跳过；设了 HEXPROOF_LIVE_URL（例如 ws://127.0.0.1:57320/ws）才跑：
//
//	HEXPROOF_LIVE_URL=ws://127.0.0.1:57320/ws go test -run Live -v .
//
// 它不改 hub 的任何东西，只开房、进房、交牌组、离开。每一步把 hub 的真实回话打印出来，
// 用来核对 spec/07 里标「待实测」的那几处。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"tablenet/pkg/nodetest"
	"tablenet/pkg/proto"

	"tablenet-adapter-hexproof/hub"
)

func liveURL(t *testing.T) string {
	u := os.Getenv("HEXPROOF_LIVE_URL")
	if u == "" {
		t.Skip("没设 HEXPROOF_LIVE_URL")
	}
	return u
}

func liveCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	go func() { <-ctx.Done(); cancel() }()
	return ctx
}

func dial(t *testing.T, url, name string) *hub.Conn {
	t.Helper()
	c, err := hub.Dial(liveCtx(), url, name, Version)
	if err != nil {
		t.Fatalf("%s 连不上：%v", name, err)
	}
	t.Cleanup(c.Close)
	return c
}

func show(t *testing.T, label string, e hub.Envelope, err error) {
	t.Helper()
	if err != nil {
		t.Logf("%-28s → 出错 %v", label, err)
		return
	}
	t.Logf("%-28s → %s %s", label, e.Type, trunc(string(e.Payload), 600))
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// drain 收一会儿推送，按类型打印。
func drain(t *testing.T, who string, c *hub.Conn, d time.Duration) []hub.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	out := []hub.Envelope{}
	for {
		e, err := c.Next(ctx)
		if err != nil {
			return out
		}
		out = append(out, e)
		t.Logf("  [%s 推送] %s %s", who, e.Type, trunc(string(e.Payload), 400))
	}
}

func TestLiveHub(t *testing.T) {
	url := liveURL(t)
	host := dial(t, url, "实测房主")
	t.Logf("welcome: v=%s server=%s forgeRules=%v", host.Welcome.V, host.Welcome.ServerVersion, host.Welcome.ForgeRulesAvailable)

	// 规则房：没装 Forge 运行时的 hub 应当拒。
	e, err := host.Request(liveCtx(), "room.create", map[string]any{"name": "实测-规则房", "format": "modern", "deckFormat": "modern",
		"maxSeats": 2, "allowSpectators": true, "spectatorsSeeHands": false, "matchMode": "bo1", "rulesMode": "forge", "hostingMode": "server"})
	show(t, "room.create(forge)", e, err)
	if err == nil {
		_, _ = host.Request(liveCtx(), "room.leave", nil)
		drain(t, "房主", host, 300*time.Millisecond)
	}

	// 手动房：带密码。
	e, err = host.Request(liveCtx(), "room.create", map[string]any{"name": "实测-手动房", "format": "modern", "deckFormat": "modern",
		"maxSeats": 2, "allowSpectators": true, "spectatorsSeeHands": false, "matchMode": "bo1", "rulesMode": "manual",
		"hostingMode": "server", "password": "sesame"})
	show(t, "room.create(manual)", e, err)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RoomID   string `json:"roomId"`
		HostSeat int    `json:"hostSeat"`
	}
	_ = e.Decode(&created)
	drain(t, "房主", host, 300*time.Millisecond)

	lobby := dial(t, url, "实测大厅")
	e, err = lobby.Request(liveCtx(), "room.list", nil)
	show(t, "room.list", e, err)
	var listed struct {
		Rooms []hub.RoomListEntry `json:"rooms"`
	}
	_ = e.Decode(&listed)
	for _, r := range listed.Rooms {
		if r.RoomID == created.RoomID {
			t.Logf("房间表条目：%+v", r)
			if f, g := tablenetFormat(r); f == "" {
				t.Errorf("tablenetFormat 认不出真 hub 的房：%+v", r)
			} else {
				t.Logf("映射成 Tablenet：format=%s gameType=%s", f, g)
			}
		}
	}

	guest := dial(t, url, "实测客人")
	e, err = guest.Request(liveCtx(), "room.join", map[string]any{"roomId": created.RoomID, "asSpectator": false, "password": "wrong"})
	show(t, "room.join(错密码)", e, err)
	var he *hub.HubError
	if err != nil && errorsAs(err, &he) {
		t.Logf("错密码的错误码：%q → 适配器映射成 %q", he.Code, joinCode(err))
	}
	e, err = guest.Request(liveCtx(), "room.join", map[string]any{"roomId": created.RoomID, "asSpectator": false, "password": "sesame"})
	show(t, "room.join", e, err)
	var joined struct {
		Seat int `json:"seat"`
	}
	_ = e.Decode(&joined)
	t.Logf("房主座位 %d，客人座位 %d", created.HostSeat, joined.Seat)
	drain(t, "客人", guest, 300*time.Millisecond)
	drain(t, "房主", host, 300*time.Millisecond)

	// 牌组：一副只写牌名（没有系列和收藏编号），一副带全。
	main, side, _ := parseDeck("[Main]\n4 Lightning Bolt\n4 Goblin Guide\n52 Mountain\n[Sideboard]\n2 Smash to Smithereens\n")
	e, err = guest.Request(liveCtx(), "deck.select", hub.DeckSelect{Name: "只有牌名", Format: "modern", DeckFormat: "modern", Mainboard: main, Sideboard: side})
	show(t, "deck.select(只有牌名)", e, err)
	if err == nil {
		t.Error("hub 收了不带印次的牌组：missingPrintings 那道拦截可以放宽了")
	}
	main, side, _ = parseDeck("[Main]\n4 Lightning Bolt|M10|146\n4 Goblin Guide|ZEN|126\n52 Mountain|M10|242\n[Sideboard]\n2 Smash to Smithereens|M11|161\n")
	e, err = guest.Request(liveCtx(), "deck.select", hub.DeckSelect{Name: "带全印次", Format: "modern", DeckFormat: "modern", Mainboard: main, Sideboard: side})
	show(t, "deck.select(带全印次)", e, err)
	main, side, _ = parseDeck("[Main]\n4 Lightning Bolt|M10|146\n56 Mountain|M10|242\n")
	e, err = host.Request(liveCtx(), "deck.select", hub.DeckSelect{Name: "带系列", Format: "modern", DeckFormat: "modern", Mainboard: main, Sideboard: side})
	show(t, "deck.select(带系列)", e, err)

	for _, c := range []*hub.Conn{host, guest} {
		e, err = c.Request(liveCtx(), "player.ready", map[string]any{"ready": true})
		show(t, "player.ready", e, err)
	}
	gp := drain(t, "客人", guest, 1500*time.Millisecond)
	hp := drain(t, "房主", host, 300*time.Millisecond)
	for _, pair := range []struct {
		c  *hub.Conn
		ev []hub.Envelope
	}{{guest, gp}, {host, hp}} {
		for _, ev := range pair.ev {
			if ev.Type == "match.load_required" {
				var p struct {
					LoadID int64 `json:"loadId"`
				}
				_ = ev.Decode(&p)
				e, err := pair.c.Request(liveCtx(), "client.load_complete", map[string]any{"loadId": p.LoadID})
				show(t, "client.load_complete", e, err)
			}
		}
	}
	gs := drain(t, "客人", guest, 2*time.Second)
	for _, ev := range gs {
		if ev.Type == "game.snapshot" {
			var raw map[string]any
			_ = json.Unmarshal(ev.Payload, &raw)
			t.Logf("手动房局面的阶段字段：currentPhase=%v", raw["currentPhase"])
		}
	}

	// 自己认输：game.conceded 是作为回话回来（带 id），还是推送？
	e, err = guest.Request(liveCtx(), "game.concede", nil)
	show(t, "game.concede(回话)", e, err)
	drain(t, "客人", guest, 500*time.Millisecond)
	drain(t, "房主", host, 300*time.Millisecond)

	// 房主离开：房间是解散还是转让？
	e, err = host.Request(liveCtx(), "room.leave", nil)
	show(t, "房主 room.leave", e, err)
	drain(t, "客人", guest, 800*time.Millisecond)
	e, err = lobby.Request(liveCtx(), "room.list", nil)
	still := strings.Contains(string(e.Payload), created.RoomID)
	t.Logf("房主离开后房间还在房间表里：%v", still)
}

// TestLiveAdapter：真 hub＋真节点＋适配器。hub 上的人开一间手动房，Tablenet 玩家在大厅看到它、拿到交接信息。
func TestLiveAdapter(t *testing.T) {
	url := liveURL(t)
	host := dial(t, url, "实测房主")
	e, err := host.Request(liveCtx(), "room.create", map[string]any{"name": "实测-大厅镜像", "format": "edh", "deckFormat": "commander",
		"maxSeats": 4, "allowSpectators": true, "spectatorsSeeHands": false, "matchMode": "bo1", "rulesMode": "manual", "hostingMode": "server"})
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RoomID string `json:"roomId"`
	}
	_ = e.Decode(&created)

	r := &rig{node: nodetest.Start(t, nodetest.Options{Adapters: map[string]nodetest.Adapter{PlatformID: {Servers: []string{"127.0.0.1"}}}})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, Config{NodeURL: r.node.URL, HubURL: url, PollEvery: 200 * time.Millisecond}, proto.GenerateKey(),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	p := player(t, r, []string{"hexproof"}, []string{Codec})
	var mine map[string]any
	for _, room := range listRooms(t, p, 1) {
		if room["roomId"] == "test.local/hexproof-"+created.RoomID {
			mine = room
		}
	}
	if mine == nil {
		t.Fatal("大厅里没看到 hub 上开的房")
	}
	t.Logf("节点上的房间：%v", mine)
	if mine["format"] != "commander" || mine["gameType"] != "ffa" || mine["join"].(map[string]any)["kind"] != "handoff" {
		t.Fatal(mine)
	}
	e2, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": mine["roomId"]})
	if err != nil || e2.Type != "room.joined" {
		t.Fatal(e2, err)
	}
	t.Logf("交接信息：%s", e2.Body)
	// 房主走了，房间解散，节点上也要撤。
	_, _ = host.Request(liveCtx(), "room.leave", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		gone := true
		for _, room := range listRooms(t, p, 0) {
			gone = gone && room["roomId"] != mine["roomId"]
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub 上解散的房，节点上没撤")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func errorsAs(err error, target **hub.HubError) bool {
	he, ok := err.(*hub.HubError)
	if ok {
		*target = he
	}
	return ok
}
