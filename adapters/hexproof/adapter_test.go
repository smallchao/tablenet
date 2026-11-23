// SPDX-License-Identifier: GPL-3.0-or-later

package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"tablenet/pkg/client"
	"tablenet/pkg/nodetest"
	"tablenet/pkg/proto"

	"tablenet-adapter-hexproof/hub"
)

// ---- 模拟的 Hexproof hub：只实现适配器用到的那几条消息，行为照 protocol/v3 的 schema ----

type fakeHub struct {
	t     *testing.T
	url   string
	mu    sync.Mutex
	rooms []hub.RoomListEntry
	log   []hub.Envelope // 收到的全部请求
	conns []*websocket.Conn
	// 进房的玩家准备好后推给他的局面和提问
	snapshot hub.Snapshot
	prompt   hub.Prompt
}

func startHub(t *testing.T) *fakeHub {
	h := &fakeHub{t: t}
	ts := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(ts.Close)
	h.url = "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	return h
}

func (h *fakeHub) got(typ string) []hub.Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []hub.Envelope{}
	for _, e := range h.log {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (h *fakeHub) waitFor(typ string, n int) []hub.Envelope {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g := h.got(typ); len(g) >= n {
			return g
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("hub 没收到 %d 条 %s", n, typ)
	return nil
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.conns = append(h.conns, ws)
	h.mu.Unlock()
	ctx := context.Background()
	send := func(e hub.Envelope) { b, _ := json.Marshal(e); _ = ws.Write(ctx, websocket.MessageText, b) }
	reply := func(req hub.Envelope, typ string, body any) {
		b, _ := json.Marshal(body)
		send(hub.Envelope{Type: typ, ID: req.ID, Payload: b})
	}
	seat := 1
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var e hub.Envelope
		_ = json.Unmarshal(data, &e)
		h.mu.Lock()
		h.log = append(h.log, e)
		rooms := append([]hub.RoomListEntry(nil), h.rooms...)
		snap, prompt := h.snapshot, h.prompt
		h.mu.Unlock()
		switch e.Type {
		case "session.hello":
			var p struct{ Protocol string }
			_ = e.Decode(&p)
			reply(e, "session.welcome", map[string]any{"v": p.Protocol, "connectionId": "x", "serverVersion": "fake", "resumeToken": "r", "forgeRulesAvailable": true, "aiModelsAvailable": false})
		case "room.list":
			reply(e, "room.listed", map[string]any{"rooms": rooms})
		case "room.join":
			var p struct{ Password string }
			_ = e.Decode(&p)
			if p.Password != "" && p.Password != "sesame" {
				reply(e, "error", map[string]string{"code": "ErrWrongPassword", "message": "wrong"})
				continue
			}
			reply(e, "room.joined", map[string]any{"roomId": "R1", "role": "player", "seat": seat})
		case "room.create":
			seat = 0
			reply(e, "room.created", map[string]any{"roomId": "NEW1", "settings": map[string]any{}, "hostSeat": 0})
		case "deck.select":
			reply(e, "deck.selected", map[string]any{"roomId": "R1", "seat": seat})
		case "player.ready":
			reply(e, "player.ready_changed", map[string]any{"roomId": "R1", "seat": seat, "ready": true})
			// 和真 hub 一样：开局后房间不再是「等人」。适配器不能因此把房撤掉、掐断桥接（11-22 实测出的 bug）。
			h.mu.Lock()
			for i := range h.rooms {
				if h.rooms[i].RoomID == "R1" {
					h.rooms[i].Phase, h.rooms[i].PlayerJoinable = "started", false
				}
			}
			h.mu.Unlock()
			send(hub.Envelope{Type: "match.load_required", Payload: json.RawMessage(`{"roomId":"R1","loadId":7}`)})
		case "client.load_complete":
			if e.ID == "" { // 和真 hub 一样：必须是带 id 的请求
				send(hub.Envelope{Type: "error", Payload: json.RawMessage(`{"code":"invalid_message","message":"id required for client.load_complete"}`)})
				continue
			}
			reply(e, "client.load_completed", map[string]any{"roomId": "R1", "loadId": 7})
			sb, _ := json.Marshal(snap)
			pb, _ := json.Marshal(prompt)
			send(hub.Envelope{Type: "match.started", Payload: json.RawMessage(`{"roomId":"R1","loadId":7}`)})
			send(hub.Envelope{Type: "rules.snapshot", Payload: sb})
			send(hub.Envelope{Type: "rules.prompt", Payload: pb})
		case "rules.respond":
			reply(e, "rules.responded", map[string]any{"roomId": "R1", "promptId": 1})
		case "game.concede":
			reply(e, "game.conceded", map[string]any{"roomId": "R1", "gameNumber": 1, "concededSeat": seat, "winnerSeat": 1 - seat, "score": []int{}, "matchFinished": true})
		}
	}
}

// ---- 测试台：节点＋假 hub＋适配器 ----

type rig struct {
	node *nodetest.Node
	hub  *fakeHub
}

func startRig(t *testing.T) *rig {
	h := startHub(t)
	h.rooms = []hub.RoomListEntry{
		{RoomID: "R1", Name: "Forge 摩登", Format: "modern", DeckFormat: "modern", MatchMode: "bo1", RulesMode: "forge", HostingMode: "server", MaxSeats: 2, PlayerCount: 1, Phase: "waiting", PlayerJoinable: true, HasPassword: true},
		{RoomID: "R2", Name: "手动指挥官", Format: "edh", DeckFormat: "commander", MatchMode: "bo1", RulesMode: "manual", MaxSeats: 4, PlayerCount: 2, Phase: "waiting", PlayerJoinable: true},
		{RoomID: "R3", Name: "玩家主机", Format: "modern", DeckFormat: "modern", MatchMode: "bo1", RulesMode: "forge", HostingMode: "player", MaxSeats: 2, Phase: "waiting", PlayerJoinable: true},
		{RoomID: "R4", Name: "已开打", Format: "modern", DeckFormat: "modern", MatchMode: "bo1", RulesMode: "forge", MaxSeats: 2, Phase: "started"},
	}
	h.snapshot = hub.Snapshot{Turn: 1, Step: "MAIN1", ActiveSeat: 1, PrioritySeat: 1,
		Players: []hub.PlayerState{{Seat: 0, Name: "Hexproof 玩家", Life: 20}, {Seat: 1, Name: "独弈玩家", Life: 20}},
		Zones: []hub.ZoneState{{Zone: "hand", OwnerSeat: 1, Count: 1, Cards: []hub.CardState{
			{ID: "c1", Visible: true, Identity: &hub.CardIdentity{Name: "Lightning Bolt"}, OwnerSeat: 1, ControllerSeat: 1}}}}}
	h.prompt = hub.Prompt{Pending: true, PromptID: 1, Kind: "chooseAction", Supported: true,
		Options: []hub.PromptOption{{ResponseID: "act-1", Kind: "cast", Label: "Cast Lightning Bolt", CardID: "c1"}}}
	n := nodetest.Start(t, nodetest.Options{Adapters: map[string]nodetest.Adapter{PlatformID: {Servers: []string{"127.0.0.1"}}}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = Run(ctx, Config{NodeURL: n.URL, HubURL: h.url, PollEvery: 50 * time.Millisecond}, proto.GenerateKey(),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	return &rig{node: n, hub: h}
}

func ctx5() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() { <-ctx.Done(); cancel() }()
	return ctx
}

func player(t *testing.T, r *rig, venues, codecs []string) *client.Client {
	t.Helper()
	c, err := client.Dial(ctx5(), r.node.URL, proto.GenerateKey(), map[string]any{"role": "player", "displayName": "独弈玩家",
		"venues": venues, "codecs": codecs, "locale": "zh-CN"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func listRooms(t *testing.T, c *client.Client, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e, err := c.Request(ctx5(), "room.list", map[string]any{"venue": PlatformID})
		if err != nil {
			t.Fatal(err)
		}
		rooms := client.DecodeBody(e)["rooms"].([]any)
		if len(rooms) >= want || time.Now().After(deadline) {
			out := []map[string]any{}
			for _, x := range rooms {
				out = append(out, x.(map[string]any))
			}
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// guest 是对局通道上的「独弈客人」：按行收发 JSON。
type guest struct {
	t   *testing.T
	c   *client.Client
	ch  uint32
	buf bytes.Buffer
	all []map[string]any
}

func (g *guest) send(v any) {
	b, _ := json.Marshal(v)
	_ = g.c.SendFrame(ctx5(), g.ch, append(b, '\n'))
}

func (g *guest) expect(typ string) map[string]any {
	g.t.Helper()
	ctx := ctx5()
	for {
		for {
			line, err := g.buf.ReadBytes('\n')
			if err != nil {
				g.buf.Write(line)
				break
			}
			var m map[string]any
			_ = json.Unmarshal(line, &m)
			g.all = append(g.all, m)
			if m["type"] == typ {
				return m
			}
		}
		ch, p, err := g.c.ExpectFrame(ctx)
		if err != nil {
			g.t.Fatalf("等 %s：%v（收到过 %d 行）", typ, err, len(g.all))
		}
		if ch == g.ch {
			g.buf.Write(p)
		}
	}
}

// dump 把客人收到的帧写给 schema 校验（BRIDGE_DUMP）。
func (g *guest) dump() {
	p := os.Getenv("BRIDGE_DUMP")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, m := range g.all {
		b, _ := json.Marshal(m)
		_, _ = w.Write(append(b, '\n'))
	}
	_ = w.Flush()
}

// dckText：同一副牌的 .dck 原文，| 后面是 Forge 的画稿序号（不是收藏编号）
const dckText = "[metadata]\nName=Burn\n[Main]\n4 Lightning Bolt|M10|1\n16 Mountain|M10|3\n[Sideboard]\n2 Smash to Smithereens|M11\n"

const deckText = "[metadata]\nName=Burn\n[Main]\n4 Lightning Bolt|M10|146\n16 Mountain|M10|242\n[Sideboard]\n2 Smash to Smithereens|M11|161\n"

func TestLobbyMirror(t *testing.T) {
	r := startRig(t)
	p := player(t, r, []string{"hexproof"}, []string{Codec})
	rooms := listRooms(t, p, 2)
	if len(rooms) != 2 {
		t.Fatalf("只该发布等人的、服务器主机的两间：%v", rooms)
	}
	byID := map[string]map[string]any{}
	for _, x := range rooms {
		byID[x["roomId"].(string)] = x
	}
	forge, manual := byID["test.local/hexproof-R1"], byID["test.local/hexproof-R2"]
	if forge["join"].(map[string]any)["kind"] != "bridge" || forge["codec"] != Codec || forge["format"] != "modern" || forge["hasPassword"] != true {
		t.Fatal(forge)
	}
	if manual["join"].(map[string]any)["kind"] != "handoff" || manual["format"] != "commander" || manual["gameType"] != "ffa" ||
		manual["x-hexproof-rules"] != "manual" {
		t.Fatal(manual)
	}
	// 房间在 hub 上没了，节点那边跟着撤。
	r.hub.mu.Lock()
	r.hub.rooms = r.hub.rooms[:1]
	r.hub.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for len(listRooms(t, p, 0)) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("hub 上没了的房应当撤下")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHandoffToManualRoom(t *testing.T) {
	r := startRig(t)
	p := player(t, r, []string{"hexproof"}, nil)
	listRooms(t, p, 2)
	e, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/hexproof-R2"})
	if err != nil || e.Type != "room.joined" {
		t.Fatal(e, err)
	}
	conn := client.DecodeBody(e)["handoff"].(map[string]any)["connect"].(map[string]any)
	if conn["client"] != "hexproof" || conn["room"] != "R2" || conn["server"] != r.hub.url {
		t.Fatal(conn)
	}
}

func TestBridgeEndToEnd(t *testing.T) {
	r := startRig(t)
	p := player(t, r, []string{"hexproof"}, []string{Codec})
	listRooms(t, p, 2)
	e, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/hexproof-R1", "password": "sesame"})
	if err != nil || e.Type != "room.joined" || client.DecodeBody(e)["channel"] == nil {
		t.Fatal(e, err)
	}
	g := &guest{t: t, c: p, ch: uint32(client.DecodeBody(e)["channel"].(float64))}
	defer g.dump()
	// 独弈客机连主机时的第一行。
	// 独弈 0.4 的样子：deck 是 .dck 原文（| 后面是画稿序号），printedDeck 才是收藏编号，适配器要用后者
	g.send(map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "room": "R1", "name": "独弈玩家", "deck": dckText, "printedDeck": deckText, "password": "x"})
	joined := g.expect("joined")
	if joined["seat"] != float64(1) || joined["protocolVersion"] != float64(arcaneProtocolVersion) {
		t.Fatal(joined)
	}
	// hub 那边：带密码进房、交了牌组、准备、确认加载完。
	jr := r.hub.waitFor("room.join", 1)[0]
	var jb map[string]any
	_ = jr.Decode(&jb)
	if jb["password"] != "sesame" || jb["roomId"] != "R1" {
		t.Fatal(jb)
	}
	var deck hub.DeckSelect
	_ = r.hub.waitFor("deck.select", 1)[0].Decode(&deck)
	if len(deck.Mainboard) != 2 || deck.Mainboard[0] != (hub.DeckCard{Name: "Lightning Bolt", Count: 4, SetCode: "M10", CollectorNumber: "146"}) ||
		len(deck.Sideboard) != 1 || deck.Format != "modern" {
		t.Fatalf("%+v", deck)
	}
	r.hub.waitFor("client.load_complete", 1)

	g.expect("match_started")
	st := g.expect("state")
	if st["phase"] != "MAIN1" || st["viewingPlayerId"] != float64(1) {
		t.Fatal(st)
	}
	cr := g.expect("choice_request")
	card := cr["legalCardIds"].([]any)[0]
	g.send(map[string]any{"type": "command", "promptId": cr["promptId"], "command": "select_card", "entityId": card})
	resp := r.hub.waitFor("rules.respond", 1)[0]
	var rb hub.Respond
	_ = resp.Decode(&rb)
	if rb.ResponseID != "act-1" || rb.PromptID != 1 {
		t.Fatalf("%+v", rb)
	}
	// 心跳：5 秒一条，等不了那么久就不测节奏，只认一下格式在 schema 里（由 dump 校验）。
	g.send(map[string]any{"type": "command", "command": "concede"})
	r.hub.waitFor("game.concede", 1)
	g.expect("match_over")
}

func TestBridgeRejectsBadJoin(t *testing.T) {
	r := startRig(t)
	p := player(t, r, []string{"hexproof"}, []string{Codec})
	listRooms(t, p, 2)
	cases := []struct {
		join map[string]any
		code string
	}{
		{map[string]any{"type": "join", "protocolVersion": 2, "deck": deckText}, "version_mismatch"},
		{map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "deck": ""}, "missing_deck"},
		{map[string]any{"type": "spectate", "protocolVersion": arcaneProtocolVersion}, "invalid_join"},
		{map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "deck": "[Main]\n4 Lightning Bolt|M10|146\n16 Mountain\n"}, "deck_not_legal"},
	}
	for _, c := range cases {
		e, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/hexproof-R1", "password": "sesame"})
		if err != nil || e.Type != "room.joined" {
			t.Fatal(e, err)
		}
		g := &guest{t: t, c: p, ch: uint32(client.DecodeBody(e)["channel"].(float64))}
		g.send(c.join)
		if got := g.expect("join_rejected"); got["code"] != c.code {
			t.Fatalf("要 %s，得到 %v", c.code, got)
		}
		g.dump()
	}
	// 密码错：hub 拒，客人收到 bad_password。
	e, _ := p.Request(ctx5(), "room.join", map[string]any{"roomId": "test.local/hexproof-R1", "password": "wrong"})
	g := &guest{t: t, c: p, ch: uint32(client.DecodeBody(e)["channel"].(float64))}
	g.send(map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "deck": deckText})
	if got := g.expect("join_rejected"); got["code"] != "bad_password" {
		t.Fatal(got)
	}
}

func TestQueueCreatesRoomThroughBridgedPlayer(t *testing.T) {
	r := startRig(t)
	a := player(t, r, []string{"hexproof"}, []string{Codec}) // 能桥接
	b := player(t, r, []string{"hexproof"}, nil)             // 只能交接
	listRooms(t, a, 2)
	pools := map[string]any{"pools": []map[string]string{{"format": "pioneer", "gameType": "duel", "structure": "bo3"}}}
	for _, c := range []*client.Client{a, b} {
		if e, err := c.Request(ctx5(), "queue.enter", pools); err != nil || e.Type != "queue.entered" {
			t.Fatal(e, err)
		}
	}
	m, err := a.Expect(ctx5(), "queue.match")
	if err != nil || client.DecodeBody(m)["venue"] != PlatformID {
		t.Fatal(m, err)
	}
	id := client.DecodeBody(m)["matchId"]
	for _, c := range []*client.Client{a, b} {
		if e, err := c.Request(ctx5(), "queue.accept", map[string]any{"matchId": id}); err != nil || e.Type != "queue.accepted" {
			t.Fatal(e, err)
		}
	}
	br, err := a.Expect(ctx5(), "match.bridged")
	if err != nil {
		t.Fatal(err)
	}
	ho, err := b.Expect(ctx5(), "match.handoff")
	if err != nil {
		t.Fatal(err)
	}
	var create map[string]any
	_ = r.hub.waitFor("room.create", 1)[0].Decode(&create)
	if create["deckFormat"] != "pioneer" || create["rulesMode"] != "forge" || create["hostingMode"] != "server" || create["matchMode"] != "bo3" {
		t.Fatal(create)
	}
	conn := client.DecodeBody(ho)["handoff"].(map[string]any)["connect"].(map[string]any)
	if conn["room"] != "NEW1" || conn["password"] != create["password"] {
		t.Fatal("交接的人要拿到 hub 的房号和密码", conn)
	}
	// 建房的那位桥接玩家已经在房里了：交牌组之后不该再 room.join。
	g := &guest{t: t, c: a, ch: uint32(client.DecodeBody(br)["channel"].(float64))}
	g.send(map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "deck": deckText})
	if j := g.expect("joined"); j["seat"] != float64(0) {
		t.Fatal(j)
	}
	r.hub.waitFor("deck.select", 1)
	if n := len(r.hub.got("room.join")); n != 0 {
		t.Fatalf("建房的人不该再进房：%d", n)
	}
	g.dump()
}

func TestQueueBothHandoffIsRefused(t *testing.T) {
	r := startRig(t)
	a := player(t, r, []string{"hexproof"}, nil)
	b := player(t, r, []string{"hexproof"}, nil)
	listRooms(t, a, 2)
	pools := map[string]any{"pools": []map[string]string{{"format": "modern"}}}
	for _, c := range []*client.Client{a, b} {
		_, _ = c.Request(ctx5(), "queue.enter", pools)
	}
	m, _ := a.Expect(ctx5(), "queue.match")
	for _, c := range []*client.Client{a, b} {
		_, _ = c.Request(ctx5(), "queue.accept", map[string]any{"matchId": client.DecodeBody(m)["matchId"]})
	}
	if _, err := a.Expect(ctx5(), "queue.match_cancelled"); err != nil {
		t.Fatal("双方都不能桥接时适配器开不了房，节点应当把两人放回排队", err)
	}
	if n := len(r.hub.got("room.create")); n != 0 {
		t.Fatal("不该在 hub 上建房", n)
	}
}

func TestParseDeck(t *testing.T) {
	main, side, cmdr := parseDeck("[Commander]\n1 Atraxa, Praetors' Voice|C16|28\n[Main]\n1 Sol Ring\n# 注释\n30 Forest|M19\n[Sideboard]\n[Companion]\n1 Lurrus of the Dream-Den\n")
	if len(main) != 3 || len(side) != 0 || len(cmdr) != 1 || cmdr[0] != "Atraxa, Praetors' Voice" {
		t.Fatal(main, side, cmdr)
	}
	if main[2] != (hub.DeckCard{Name: "Forest", Count: 30, SetCode: "M19"}) {
		t.Fatal(main[2])
	}
}

func TestFormatMapping(t *testing.T) {
	for _, f := range Formats {
		table, deck := hexproofFormat(f)
		if table == "" {
			t.Fatal(f)
		}
		back, _ := tablenetFormat(hub.RoomListEntry{Format: table, DeckFormat: deck, MaxSeats: 2})
		if back != f {
			t.Errorf("%s → (%s,%s) → %s", f, table, deck, back)
		}
	}
}
