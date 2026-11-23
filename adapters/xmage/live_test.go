// SPDX-License-Identifier: MIT

package xmage

// 连真 XMage 服务器的实测。平时跳过；设了下面这些才跑：
//
//	XMAGE_LIVE=127.0.0.1:17171         XMage 服务器
//	XMAGE_LIB=…/mage-server/lib        XMage 服务器的 lib 目录
//	XMAGE_DRIVER=…/driver-classes      Java 驱动（jar 或 classes 目录）
//	XMAGE_DRIVER_DIR=…                 驱动的工作目录（本地牌库建在 <它>/adapter 和 <它>/opponent 下）
//	BRIDGE_DUMP=frames.jsonl           （可选）把发给独弈客人的每一帧写出来，拿 arcane.v0 schema 校验
//
//	go test -run Live -v ./adapters/xmage/
//
// 测法：测试自己开一个 XMage 会话当「XMage 那边的玩家」，建桌、坐下、自动应答；
// 适配器把这张桌发布到真 Tablenet 节点；脚本化的独弈客人在节点上找到它、桥接入座，像真客户端那样一路打。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tablenet/pkg/client"
	"tablenet/pkg/nodetest"
	"tablenet/pkg/proto"
)

func liveEnv(t *testing.T) (server string, drv DriverConfig) {
	server = os.Getenv("XMAGE_LIVE")
	if server == "" {
		t.Skip("没设 XMAGE_LIVE")
	}
	drv = DriverConfig{XMageLib: os.Getenv("XMAGE_LIB"), Jar: os.Getenv("XMAGE_DRIVER"), WorkDir: os.Getenv("XMAGE_DRIVER_DIR")}
	if drv.XMageLib == "" || drv.Jar == "" {
		t.Fatal("要同时设 XMAGE_LIB 和 XMAGE_DRIVER")
	}
	return
}

func ctxN(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	go func() { <-ctx.Done(); cancel() }()
	return ctx
}

const liveDeck = "[Main]\n24 Mountain\n4 Lightning Bolt\n4 Goblin Guide\n4 Shock\n4 Lava Spike\n4 Monastery Swiftspear\n4 Grizzly Bears\n4 Searing Spear\n4 Raging Goblin\n4 Fireblast\n"

type tally struct {
	mu    sync.Mutex
	kinds map[string]int
	notes []string
}

func (t *tally) add(k string) {
	t.mu.Lock()
	t.kinds[k]++
	t.mu.Unlock()
}

func (t *tally) note(s string) {
	t.mu.Lock()
	if len(t.notes) < 60 {
		t.notes = append(t.notes, s)
	}
	t.mu.Unlock()
}

func (t *tally) report(tt *testing.T, who string) {
	keys := []string{}
	for k := range t.kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, t.kinds[k]))
	}
	tt.Logf("%s 遇到的：%s", who, strings.Join(parts, " "))
	for _, n := range t.notes {
		tt.Logf("  %s：%s", who, n)
	}
}

// opponent：XMage 那边的玩家（直接用驱动开会话，像 XMage 客户端一样逐个回答，能下地就下地，其余让过）。
func opponent(t *testing.T, ctx context.Context, drv *Driver, server, password string, tl *tally) (tableID string) {
	return xmagePlayer(t, ctx, drv, server, "", password, tl)
}

// xmagePlayer：XMage 那边的一位玩家。joinName 为空时自己建桌当桌主（坐满后开打）；
// 否则照交接信息在主房间里按桌名找到那张桌、凭密码坐下（像拿着交接信息去 XMage 客户端里的玩家）。
func xmagePlayer(t *testing.T, ctx context.Context, drv *Driver, server, joinName, password string, tl *tally) (tableID string) {
	host, portS, _ := net.SplitHostPort(server)
	port, _ := strconv.Atoi(portS)
	ev := drv.Subscribe("opp")
	if _, err := drv.Call(ctxN(30*time.Second), "connect", map[string]any{"s": "opp", "host": host, "port": port, "user": "opp_live"}); err != nil {
		t.Fatal(err)
	}
	var tb Table
	owner := joinName == ""
	if owner {
		res, err := drv.Call(ctxN(30*time.Second), "create_table", map[string]any{"s": "opp", "name": "Tablenet live", "gameType": "Two Player Duel",
			"deckType": "Constructed - Freeform", "seats": 2, "password": password})
		if err != nil {
			t.Fatal(err)
		}
		_ = decodeAs(res, "table", &tb)
	} else {
		deadline := time.Now().Add(20 * time.Second)
		for tb.ID == "" {
			res, err := drv.Call(ctxN(10*time.Second), "tables", map[string]any{"s": "opp"})
			if err == nil {
				var ts []Table
				_ = decodeAs(res, "tables", &ts)
				for _, x := range ts {
					if x.Name == joinName {
						tb = x
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("XMage 主房间里找不到桌子 %q", joinName)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if _, err := drv.Call(ctxN(30*time.Second), "join_table", map[string]any{"s": "opp", "table": tb.ID, "name": "opp_live",
		"deck": parseDeck(liveDeck), "password": password}); err != nil {
		t.Fatal(err)
	}
	go func() {
		game := ""
		started := !owner
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if started {
					continue
				}
				r, err := drv.Call(ctxN(10*time.Second), "table", map[string]any{"s": "opp", "table": tb.ID})
				if err != nil {
					continue
				}
				var cur Table
				_ = decodeAs(r, "table", &cur)
				if cur.State == "READY_TO_START" {
					if _, err := drv.Call(ctxN(20*time.Second), "start_match", map[string]any{"s": "opp", "table": tb.ID}); err == nil {
						started = true
					} else {
						tl.note("开打失败：" + err.Error())
					}
				}
			case e := <-ev:
				tl.add(e.M)
				var d struct {
					Game    string         `json:"game"`
					Message string         `json:"message"`
					Targets []string       `json:"targets"`
					Options map[string]any `json:"options"`
					Choices []struct {
						ID string `json:"id"`
					} `json:"choices"`
					View *struct {
						Playable map[string]struct {
							Names []string `json:"names"`
						} `json:"playable"`
					} `json:"view"`
				}
				_ = json.Unmarshal(e.D, &d)
				ans := func(op string, v any) {
					go func() {
						time.Sleep(30 * time.Millisecond)
						if _, err := drv.Call(ctxN(15*time.Second), op, map[string]any{"s": "opp", "game": game, "v": v}); err != nil {
							tl.note(fmt.Sprintf("回答 %s 出错：%v", e.M, err))
						}
					}()
				}
				switch e.M {
				case "START_GAME":
					game = d.Game
					_, _ = drv.Call(ctxN(15*time.Second), "join_game", map[string]any{"s": "opp", "game": game})
				case "GAME_ASK":
					ans("bool", false)
				case "GAME_SELECT":
					if d.View != nil {
						for id, p := range d.View.Playable {
							if len(p.Names) > 0 && strings.HasPrefix(p.Names[0], "Play ") {
								ans("uuid", id)
								goto next
							}
						}
					}
					ans("bool", e.M == "GAME_SELECT" && d.Options["possibleAttackers"] != nil)
				case "GAME_TARGET":
					if len(d.Targets) > 0 {
						ans("uuid", d.Targets[0])
					} else {
						ans("bool", true)
					}
				case "GAME_CHOOSE_ABILITY":
					if len(d.Choices) > 0 {
						ans("uuid", d.Choices[0].ID)
					}
				case "GAME_PLAY_MANA", "GAME_PLAY_XMANA":
					ans("bool", false)
				case "GAME_GET_AMOUNT":
					ans("int", 0)
				}
			next:
			}
		}
	}()
	return tb.ID
}

// liveGuest 是对局通道上的「独弈客人」：按行收发 JSON。
type liveGuest struct {
	c   *client.Client
	ch  uint32
	buf bytes.Buffer
	all []map[string]any
}

func (g *liveGuest) send(v any) {
	b, _ := json.Marshal(v)
	_ = g.c.SendFrame(ctxN(5*time.Second), g.ch, append(b, '\n'))
}

func (g *liveGuest) next(ctx context.Context) (map[string]any, bool) {
	for {
		if line, err := g.buf.ReadBytes('\n'); err == nil {
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				g.all = append(g.all, m)
				return m, true
			}
			continue
		} else {
			g.buf.Write(line)
		}
		rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		ch, p, err := g.c.ExpectFrame(rctx)
		cancel()
		if err != nil {
			return nil, false
		}
		if ch == g.ch {
			g.buf.Write(p)
		}
	}
}

func (g *liveGuest) dump() {
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

func asList(v any) []any {
	xs, _ := v.([]any)
	return xs
}

// guestAnswer：独弈客人像真客户端那样回应。先下地、再施放；进攻全派；阻挡不挡；选目标选对手；其余确定。
func guestAnswer(m map[string]any, skip map[any]bool) map[string]any {
	pid := m["promptId"]
	c := func(command string, extra map[string]any) map[string]any {
		out := map[string]any{"type": "command", "promptId": pid, "command": command}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	first := func(key string) any {
		if xs := asList(m[key]); len(xs) > 0 {
			return xs[0]
		}
		return nil
	}
	switch m["type"] {
	case "choice_request":
		switch m["kind"] {
		case "action":
			if m["manaPayment"] != nil {
				return c("ok", nil)
			}
			for _, want := range []string{"play ", "cast "} {
				for _, a := range asList(m["cardActions"]) {
					am := a.(map[string]any)
					label, _ := am["label"].(string)
					if strings.HasPrefix(strings.ToLower(label), want) && !skip[am["cardId"]] {
						skip[am["cardId"]] = true // 同一张只试一次（付不起费用会取消）
						return c("select_card", map[string]any{"entityId": am["cardId"]})
					}
				}
			}
			return c("pass", nil)
		case "mulligan":
			return c("ok", nil)
		case "attack":
			if len(asList(m["declaredAttackerIds"])) == 0 && len(asList(m["attackerCandidateIds"])) > 0 {
				return c("alpha_strike", nil)
			}
			return c("ok", nil)
		case "block", "confirm":
			return c("ok", nil)
		case "select":
			// 先选对手（烧脸），再选牌
			if ps := asList(m["candidatePlayerIds"]); len(ps) > 0 && len(asList(m["selectedPlayerIds"])) == 0 {
				return c("select_player", map[string]any{"entityId": ps[len(ps)-1]})
			}
			if id := first("legalCardIds"); id != nil && len(asList(m["selectedCardIds"])) == 0 {
				return c("select_card", map[string]any{"entityId": id})
			}
			return c("ok", nil)
		}
	case "modal_request":
		if o, ok := first("options").(map[string]any); ok {
			return c("select_option", map[string]any{"entityId": o["id"]})
		}
	case "number_request":
		return c("submit_number", map[string]any{"value": m["min"]})
	case "text_request":
		return c("submit_text", map[string]any{"text": "Lightning Bolt"})
	case "allocation_request":
		ts := asList(m["targets"])
		if len(ts) == 0 {
			return nil
		}
		return c("submit_allocation", map[string]any{"allocations": []map[string]any{{"id": ts[0].(map[string]any)["id"], "amount": m["total"]}}})
	}
	return nil
}

type tlog struct{ t *testing.T }

func (l tlog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func TestLiveBridge(t *testing.T) {
	server, dc := liveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	oppTally := &tally{kinds: map[string]int{}}
	guestTally := &tally{kinds: map[string]int{}}

	// XMage 那边的玩家：单独一个驱动（两个驱动不能共用一个牌库目录，H2 会锁）。
	oc := dc
	oc.WorkDir = filepath.Join(dc.WorkDir, "opponent")
	if f, err := os.Create(filepath.Join(dc.WorkDir, "opponent.log")); err == nil {
		oc.Stderr = f
	}
	od, err := StartDriver(oc)
	if err != nil {
		t.Fatal(err)
	}
	defer od.Close()
	if _, err := od.Call(ctxN(5*time.Minute), "cards", nil); err != nil {
		t.Fatal(err)
	}
	tableID := opponent(t, ctx, od, server, "sesame", oppTally)
	t.Logf("XMage 桌子 %s", tableID)

	// 独弈一侧：节点＋适配器＋桥接
	n := nodetest.Start(t, nodetest.Options{Adapters: map[string]nodetest.Adapter{PlatformID: {Servers: []string{"127.0.0.1"}, TrustResults: true}}})
	ac := dc
	ac.WorkDir = filepath.Join(dc.WorkDir, "adapter")
	if f, err := os.Create(filepath.Join(dc.WorkDir, "adapter.log")); err == nil {
		ac.Stderr = f
	}
	go func() {
		err := Run(ctx, Config{NodeURL: n.URL, Server: server, PollEvery: 300 * time.Millisecond, Driver: ac}, proto.GenerateKey(),
			slog.New(slog.NewTextHandler(tlog{t}, &slog.HandlerOptions{Level: slog.LevelInfo})))
		if err != nil && ctx.Err() == nil {
			t.Logf("适配器退出：%v", err)
		}
	}()
	p, err := client.Dial(ctxN(10*time.Second), n.URL, proto.GenerateKey(), map[string]any{"role": "player", "displayName": "独弈玩家",
		"venues": []string{PlatformID}, "codecs": []string{Codec}, "locale": "zh-CN"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	roomID := "test.local/xmage-" + localID("x", tableID)
	deadline := time.Now().Add(4 * time.Minute) // 适配器第一次要建本地牌库
	for {
		e, err := p.Request(ctxN(5*time.Second), "room.list", map[string]any{"venue": PlatformID})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range asList(client.DecodeBody(e)["rooms"]) {
			found = found || r.(map[string]any)["roomId"] == roomID
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("XMage 的桌子没出现在节点大厅")
		}
		time.Sleep(300 * time.Millisecond)
	}
	je, err := p.Request(ctxN(10*time.Second), "room.join", map[string]any{"roomId": roomID, "password": "sesame"})
	if err != nil || je.Type != "room.joined" {
		t.Fatal(je, err)
	}
	g := &liveGuest{c: p, ch: uint32(client.DecodeBody(je)["channel"].(float64))}
	defer g.dump()
	g.send(map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "name": "Arcane Guest", "deck": liveDeck})

	skip := map[any]bool{}
	answered, errorsSeen := 0, 0
	sawState, sawStarted := false, false
	var over map[string]any
	stop := time.Now().Add(4 * time.Minute)
	for time.Now().Before(stop) && over == nil {
		m, ok := g.next(ctx)
		if !ok {
			break
		}
		typ, _ := m["type"].(string)
		switch typ {
		case "join_rejected":
			t.Fatalf("桥接被拒：%v", m)
		case "state":
			sawState = true
		case "match_started":
			sawStarted = true
		case "error":
			errorsSeen++
			guestTally.note(fmt.Sprintf("error %v %v", m["code"], m["message"]))
		case "match_over":
			over = m
		case "choice_request", "modal_request", "number_request", "text_request", "allocation_request":
			k := typ
			if kind, ok := m["kind"].(string); ok {
				k += ":" + kind
			}
			guestTally.add(k)
			if cmd := guestAnswer(m, skip); cmd != nil {
				answered++
				g.send(cmd)
			}
		}
		// 打满 120 次回答还没结束，就认输收场（测的是桥接全程，不是谁赢）
		if answered == 120 {
			g.send(map[string]any{"type": "command", "command": "concede"})
			answered++
		}
	}
	oppTally.report(t, "XMage 玩家")
	guestTally.report(t, "独弈客人")
	t.Logf("回答 %d 次，收到 error %d 次，共 %d 帧", answered, errorsSeen, len(g.all))
	if !sawStarted || !sawState {
		t.Fatalf("没收到 match_started 或 state（started=%v state=%v）", sawStarted, sawState)
	}
	if over == nil {
		t.Fatal("没打完：没收到 match_over")
	}
	if errorsSeen > 5 {
		t.Fatalf("客人收到 %d 次 error：多半是某种提问原地打转", errorsSeen)
	}
	t.Logf("结束：%v", over)
}

// 排队配到 XMage：一位能桥接、一位只能交接。机器人建带密码的桌（不坐下），桥接的那位由适配器替他坐下，
// 交接的那位拿着服务器、桌名、密码去 XMage 里坐（这里用驱动会话代替 XMage 客户端），坐满后机器人开打。
func TestLiveQueue(t *testing.T) {
	server, dc := liveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	oppTally, guestTally := &tally{kinds: map[string]int{}}, &tally{kinds: map[string]int{}}
	n := nodetest.Start(t, nodetest.Options{Adapters: map[string]nodetest.Adapter{PlatformID: {Servers: []string{"127.0.0.1"}, TrustResults: true}}})
	ac := dc
	ac.WorkDir = filepath.Join(dc.WorkDir, "adapter")
	if f, err := os.Create(filepath.Join(dc.WorkDir, "adapter-queue.log")); err == nil {
		ac.Stderr = f
	}
	go func() {
		h := slog.New(slog.NewTextHandler(tlog{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
		_ = Run(ctx, Config{NodeURL: n.URL, Server: server, LobbyUser: "tablenet_q", PollEvery: 300 * time.Millisecond, Driver: ac}, proto.GenerateKey(), h)
	}()
	dialP := func(codecs []string) *client.Client {
		c, err := client.Dial(ctxN(10*time.Second), n.URL, proto.GenerateKey(), map[string]any{"role": "player", "displayName": "排队玩家",
			"venues": []string{PlatformID}, "codecs": codecs, "locale": "zh-CN"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	a, b := dialP([]string{Codec}), dialP(nil)
	// 等适配器上线（节点 welcome 里有了 xmage 平台才能配到它）
	deadline := time.Now().Add(4 * time.Minute)
	pools := map[string]any{"pools": []map[string]string{{"format": "casual", "gameType": "duel", "structure": "bo1"}}}
	var matchID any
	for matchID == nil {
		for _, c := range []*client.Client{a, b} {
			_, _ = c.Request(ctxN(5*time.Second), "queue.enter", pools)
		}
		mctx, mcancel := context.WithTimeout(ctx, 3*time.Second)
		m, err := a.Expect(mctx, "queue.match")
		mcancel()
		if err == nil && client.DecodeBody(m)["venue"] == PlatformID {
			matchID = client.DecodeBody(m)["matchId"]
			break
		}
		for _, c := range []*client.Client{a, b} {
			_, _ = c.Request(ctxN(5*time.Second), "queue.leave", nil)
		}
		if time.Now().After(deadline) {
			t.Fatal("排队一直没配到 XMage")
		}
	}
	for _, c := range []*client.Client{a, b} {
		if e, err := c.Request(ctxN(5*time.Second), "queue.accept", map[string]any{"matchId": matchID}); err != nil || e.Type != "queue.accepted" {
			t.Fatal(e, err)
		}
	}
	br, err := a.Expect(ctxN(30*time.Second), "match.bridged")
	if err != nil {
		t.Fatal(err)
	}
	ho, err := b.Expect(ctxN(30*time.Second), "match.handoff")
	if err != nil {
		t.Fatal(err)
	}
	conn := client.DecodeBody(ho)["handoff"].(map[string]any)["connect"].(map[string]any)
	t.Logf("交接信息：%v", conn)
	if conn["client"] != "xmage" || conn["password"] == nil || conn["room"] == nil {
		t.Fatal("交接信息不全", conn)
	}

	// 交接的那位去 XMage 坐下
	oc := dc
	oc.WorkDir = filepath.Join(dc.WorkDir, "opponent")
	od, err := StartDriver(oc)
	if err != nil {
		t.Fatal(err)
	}
	defer od.Close()
	if _, err := od.Call(ctxN(5*time.Minute), "cards", nil); err != nil {
		t.Fatal(err)
	}
	xmagePlayer(t, ctx, od, server, conn["room"].(string), conn["password"].(string), oppTally)

	// 桥接的那位在独弈里打
	g := &liveGuest{c: a, ch: uint32(client.DecodeBody(br)["channel"].(float64))}
	defer g.dump()
	g.send(map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "name": "Queue Guest", "deck": liveDeck})
	skip := map[any]bool{}
	answered, errorsSeen := 0, 0
	var over map[string]any
	stop := time.Now().Add(4 * time.Minute)
	for time.Now().Before(stop) && over == nil {
		m, ok := g.next(ctx)
		if !ok {
			break
		}
		switch m["type"] {
		case "join_rejected":
			t.Fatalf("桥接被拒：%v", m)
		case "error":
			errorsSeen++
			guestTally.note(fmt.Sprintf("error %v %v", m["code"], m["message"]))
		case "match_over":
			over = m
		case "choice_request", "modal_request", "number_request", "text_request", "allocation_request":
			k, _ := m["type"].(string)
			if kind, ok := m["kind"].(string); ok {
				k += ":" + kind
			}
			guestTally.add(k)
			if cmd := guestAnswer(m, skip); cmd != nil {
				answered++
				g.send(cmd)
			}
		}
		if answered == 120 {
			g.send(map[string]any{"type": "command", "command": "concede"})
			answered++
		}
	}
	oppTally.report(t, "交接到 XMage 的玩家")
	guestTally.report(t, "桥接的独弈玩家")
	if over == nil {
		t.Fatal("没打完：没收到 match_over")
	}
	if errorsSeen > 5 {
		t.Fatalf("客人收到 %d 次 error：多半是某种提问原地打转", errorsSeen)
	}
	t.Logf("结束：%v，共 %d 帧", over, len(g.all))
}
