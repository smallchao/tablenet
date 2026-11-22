// SPDX-License-Identifier: GPL-3.0-or-later

package adapter

// 规则房桥接的真机实测：要 hub 装了 Forge 运行时。
//
//	HEXPROOF_LIVE_URL=ws://127.0.0.1:57320/ws HEXPROOF_LIVE_RULES=1 go test -run LiveRules -v -timeout 10m .
//
// 一边是 hub 上的「Hexproof 玩家」（直接连 hub，每个提问回默认答案），
// 一边是经适配器桥接的「独弈玩家」（在对局通道里讲 arcane.v0，能下地就下地、能施放就施放、其余让过）。
// 打满一段时间或打完一局，统计两边遇到的提问种类、hub 拒掉的回答，把发给独弈的帧交给 BRIDGE_DUMP 做 schema 校验。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"tablenet/pkg/client"
	"tablenet/pkg/nodetest"
	"tablenet/pkg/proto"

	"tablenet-adapter-hexproof/hub"
)

// 两副一样的简单红色牌：地、烧、熊，覆盖下地、施放、指定目标、付费、宣攻宣挡。
// 印次取自 Forge 运行时自带的 res/editions（Forge 不认的印次开不了局）。
const liveDeck = "[Main]\n24 Mountain|DFT|275\n4 Lightning Bolt|ATH|43\n4 Goblin Guide|2XM|127\n4 Shock|AER|98\n4 Lava Spike|CHK|178\n4 Monastery Swiftspear|KTK|118\n4 Grizzly Bears|8ED|256\n4 Searing Spear|E01|56\n4 Raging Goblin|ANB|81\n4 Fireblast|DMR|119\n"

func deckSelectOf(text string) hub.DeckSelect {
	main, side, _ := parseDeck(text)
	return hub.DeckSelect{Name: "实测红烧", Format: "modern", DeckFormat: "modern", Mainboard: main, Sideboard: side}
}

// autoAnswer：Hexproof 一侧的默认回答。
func autoAnswer(p hub.Prompt) hub.Respond {
	r := hub.Respond{PromptID: p.PromptID, ResponseID: "$submit"}
	switch p.Kind {
	case "acknowledge", "diceRolled", "revealCards":
		r.ResponseID = "$ack"
	case "chooseAction":
		r.ResponseID = "$pass"
	case "mulligan":
		r.ResponseID = "$keep"
	case "payManaCost":
		r.ResponseID = "$auto-pay"
	case "mulliganPutBack":
		for i := 0; i < p.RequiredSelections && i < len(p.Cards); i++ {
			r.CardIDs = append(r.CardIDs, p.Cards[i].ID)
		}
	case "chooseCards":
		for i := 0; i < p.MinCardSelections && i < len(p.Cards); i++ {
			r.CardIDs = append(r.CardIDs, p.Cards[i].ID)
		}
	case "chooseBoardTargets":
		for i := 0; i < max(p.MinSelections, 1) && i < len(p.Targets); i++ {
			r.TargetIDs = append(r.TargetIDs, p.Targets[i].ResponseID)
		}
	case "chooseBoolean", "chooseColor", "chooseFromSelection":
		for i := 0; i < max(p.MinChoiceTotal, 1) && i < len(p.Choices); i++ {
			r.ChoiceIDs = append(r.ChoiceIDs, p.Choices[i].ResponseID)
		}
	case "chooseNumber":
		n := p.MinNumber
		r.ChosenNumber = &n
	case "chooseCardName":
		r.Name = "Lightning Bolt"
	case "reorder":
		for _, it := range p.OrderItems {
			r.OrderedIDs = append(r.OrderedIDs, it.ResponseID)
		}
	case "chooseDamageAssignmentOrder":
		for _, t := range p.DamageTargets {
			r.DamageOrderIDs = append(r.DamageOrderIDs, t.ResponseID)
		}
	case "chooseCombatDamageAssignment":
		if len(p.DamageTargets) > 0 {
			r.DamageAssignments = []hub.DamageAssignment{{TargetID: p.DamageTargets[0].ResponseID, Damage: p.TotalDamage}}
		}
	case "scry":
		ids := []string{}
		for _, c := range p.Cards {
			ids = append(ids, c.ID)
		}
		if len(p.ScryDestinations) > 0 {
			r.ScryPiles = []hub.ScryPile{{Destination: p.ScryDestinations[0], CardIDs: ids}}
		}
	}
	return r
}

type tally struct {
	mu      sync.Mutex
	kinds   map[string]int
	steps   map[string]int
	notes   []string
	samples map[string]string
}

func newTally() *tally {
	return &tally{kinds: map[string]int{}, steps: map[string]int{}, samples: map[string]string{}}
}

func (t *tally) add(m map[string]int, k string) { t.mu.Lock(); m[k]++; t.mu.Unlock() }
func (t *tally) note(s string)                  { t.mu.Lock(); t.notes = append(t.notes, s); t.mu.Unlock() }
func (t *tally) sample(k, v string) {
	t.mu.Lock()
	if _, ok := t.samples[k]; !ok {
		t.samples[k] = v
	}
	t.mu.Unlock()
}

func sortedKeys(m map[string]int) string {
	ks := []string{}
	for k, v := range m {
		ks = append(ks, fmt.Sprintf("%s×%d", k, v))
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}

// logHandler 把适配器日志转进测试输出，顺便记下桥接玩家收到的提问种类和步骤名。
type logHandler struct {
	t  *testing.T
	ta *tally
}

func (h logHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h logHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h logHandler) WithGroup(string) slog.Handler            { return h }
func (h logHandler) Handle(_ context.Context, r slog.Record) error {
	parts := []string{r.Message}
	r.Attrs(func(a slog.Attr) bool { parts = append(parts, a.Key+"="+a.Value.String()); return true })
	line := strings.Join(parts, " ")
	h.t.Logf("[适配器] %s", line)
	if r.Message == "bridge.degraded" || r.Level >= slog.LevelWarn {
		h.ta.note("适配器：" + line)
	}
	return nil
}

func TestLiveRules(t *testing.T) {
	url := liveURL(t)
	if os.Getenv("HEXPROOF_LIVE_RULES") == "" {
		t.Skip("没设 HEXPROOF_LIVE_RULES")
	}
	hostTally, guestTally := newTally(), newTally()
	if os.Getenv("HEXPROOF_TRACE") != "" {
		hub.Trace = func(conn, dir string, data []byte) {
			if conn == "独弈玩家" && !strings.Contains(string(data), "room.snapshot") {
				t.Logf("[hub %s %s] %s", conn, dir, trunc(string(data), 220))
			}
		}
		defer func() { hub.Trace = nil }()
	}

	// —— Hexproof 一侧：开规则房、交牌组、准备，之后自动回答 ——
	host := dial(t, url, "Hexproof 玩家")
	if !host.Welcome.ForgeRulesAvailable {
		t.Fatal("hub 没开 Forge 规则房")
	}
	e, err := host.Request(liveCtx(), "room.create", map[string]any{"name": "实测-规则房", "format": "modern", "deckFormat": "modern",
		"maxSeats": 2, "allowSpectators": false, "spectatorsSeeHands": false, "matchMode": "bo1", "rulesMode": "forge",
		"hostingMode": "server", "password": "sesame"})
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RoomID string `json:"roomId"`
	}
	_ = e.Decode(&created)
	if _, err := host.Request(liveCtx(), "deck.select", deckSelectOf(liveDeck)); err != nil {
		t.Fatal("房主交牌组：", err)
	}
	// 座位坐满之前 hub 不让准备：等桥接玩家进来再准备（见下面 room.snapshot）。
	hostReady := false
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	hostDone := make(chan struct{})
	go func() {
		defer close(hostDone)
		for {
			ev, err := host.Next(ctx)
			if err != nil {
				return
			}
			if ev.Type != "room.snapshot" {
				t.Logf("[房主收到] %s %s", ev.Type, trunc(string(ev.Payload), 160))
			}
			switch ev.Type {
			case "room.snapshot":
				var rs struct {
					Seats []struct {
						Occupied bool `json:"occupied"`
					} `json:"seats"`
				}
				_ = ev.Decode(&rs)
				full := len(rs.Seats) > 0
				for _, s := range rs.Seats {
					full = full && s.Occupied
				}
				if full && !hostReady {
					if _, err := host.Request(ctx, "player.ready", map[string]any{"ready": true}); err == nil {
						hostReady = true
					} else {
						hostTally.note("房主准备失败：" + err.Error())
					}
				}
			case "match.load_required":
				var p struct {
					LoadID int64 `json:"loadId"`
				}
				_ = ev.Decode(&p)
				_, _ = host.Request(ctx, "client.load_complete", map[string]any{"loadId": p.LoadID})
			case "rules.snapshot":
				var s hub.Snapshot
				_ = ev.Decode(&s)
				hostTally.add(hostTally.steps, s.Step)
				if s.GameOver {
					return
				}
			case "rules.prompt":
				var p hub.Prompt
				_ = ev.Decode(&p)
				if !p.Pending {
					continue
				}
				hostTally.add(hostTally.kinds, p.Kind)
				hostTally.sample(p.Kind, string(ev.Payload))
				if _, err := host.Request(ctx, "rules.respond", autoAnswer(p)); err != nil {
					hostTally.note(fmt.Sprintf("房主的默认回答被拒：%s %v", p.Kind, err))
				}
			}
		}
	}()

	// —— 独弈一侧：节点＋适配器＋桥接 ——
	n := nodetest.Start(t, nodetest.Options{Adapters: map[string]nodetest.Adapter{PlatformID: {Servers: []string{"127.0.0.1"}}}})
	go func() {
		_ = Run(ctx, Config{NodeURL: n.URL, HubURL: url, PollEvery: 200 * time.Millisecond}, proto.GenerateKey(),
			slog.New(logHandler{t: t, ta: guestTally}))
	}()
	r := &rig{node: n}
	p := player(t, r, []string{"hexproof"}, []string{Codec})
	roomID := "test.local/hexproof-" + created.RoomID
	deadline := time.Now().Add(10 * time.Second)
	for {
		found := false
		for _, room := range listRooms(t, p, 1) {
			found = found || room["roomId"] == roomID
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("规则房没出现在节点大厅")
		}
		time.Sleep(100 * time.Millisecond)
	}
	je, err := p.Request(ctx5(), "room.join", map[string]any{"roomId": roomID, "password": "sesame"})
	if err != nil || je.Type != "room.joined" {
		t.Fatal(je, err)
	}
	g := &guest{t: t, c: p, ch: uint32(client.DecodeBody(je)["channel"].(float64))}
	defer g.dump()
	g.send(map[string]any{"type": "join", "protocolVersion": arcaneProtocolVersion, "name": "独弈玩家", "deck": liveDeck})

	answered, rejected := 0, 0
	samePrompt, lastPid := 0, any(nil)
	skip, lastCard := map[any]bool{}, any(nil) // 取消过的牌这一局不再试
	var lastPhase any
	stop := time.Now().Add(3 * time.Minute)
	for time.Now().Before(stop) {
		m, ok := g.next(ctx)
		if !ok {
			break
		}
		typ, _ := m["type"].(string)
		if typ != "ping" {
			t.Logf("[独弈收到] %s", trunc(string(mustJSON(m)), 300))
		}
		switch typ {
		case "join_rejected":
			t.Fatalf("桥接被拒：%v", m)
		case "state":
			guestTally.add(guestTally.steps, fmt.Sprint(m["phase"]))
			lastPhase = m["phase"]
		case "error":
			rejected++
			guestTally.note(fmt.Sprintf("独弈收到 error：%v %v", m["code"], m["message"]))
		case "game_over", "match_over":
			guestTally.note(fmt.Sprintf("结束：%v", m))
			stop = time.Now()
		}
		if c := guestAnswer(m, skip); c != nil {
			if c["command"] == "select_card" && m["kind"] == "action" {
				lastCard = c["entityId"]
			}
			// 同一个提问反复出现（比如付不起费用、自动支付也付不了）：改按取消，别原地打转。
			if c["promptId"] != nil && c["promptId"] == lastPid {
				if samePrompt++; samePrompt >= 3 {
					c = map[string]any{"type": "command", "promptId": c["promptId"], "command": "cancel"}
					samePrompt = 0
					skip[lastCard] = true
				}
			} else {
				samePrompt, lastPid = 0, c["promptId"]
			}
			k := typ
			if kind, ok := m["kind"].(string); ok {
				k += "/" + kind
			}
			guestTally.add(guestTally.kinds, k)
			g.send(c)
			answered++
		}
	}
	t.Logf("独弈一侧：答了 %d 次，收到 error %d 次，最后阶段 %v", answered, rejected, lastPhase)
	t.Logf("独弈收到的提问帧：%s", sortedKeys(guestTally.kinds))
	t.Logf("独弈看到的阶段：%s", sortedKeys(guestTally.steps))
	t.Logf("Hexproof 一侧的提问：%s", sortedKeys(hostTally.kinds))
	t.Logf("Hexproof 的步骤名：%s", sortedKeys(hostTally.steps))
	for _, s := range append(hostTally.notes, guestTally.notes...) {
		t.Log(s)
	}
	for k, v := range hostTally.samples {
		t.Logf("样本 %s：%s", k, trunc(v, 700))
	}
	if answered == 0 {
		t.Fatal("桥接玩家一次都没被问到")
	}
	g.send(map[string]any{"type": "command", "command": "concede"})
	cancel()
	<-hostDone
}

// TestLiveRulesTimeline：不经适配器，两条 hub 连接直接开一局规则房，打印开局阶段每条推送的时间，
// 看清 Forge 冷启动要多久、load_complete 什么时候回、第一条 rules.prompt 什么时候来。
func TestLiveRulesTimeline(t *testing.T) {
	url := liveURL(t)
	if os.Getenv("HEXPROOF_LIVE_RULES") == "" {
		t.Skip("没设 HEXPROOF_LIVE_RULES")
	}
	a, b := dial(t, url, "甲"), dial(t, url, "乙")
	e, err := a.Request(liveCtx(), "room.create", map[string]any{"name": "时间线", "format": "modern", "deckFormat": "modern",
		"maxSeats": 2, "allowSpectators": false, "spectatorsSeeHands": false, "matchMode": "bo1", "rulesMode": "forge", "hostingMode": "server"})
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		RoomID string `json:"roomId"`
	}
	_ = e.Decode(&created)
	if _, err := b.Request(liveCtx(), "room.join", map[string]any{"roomId": created.RoomID, "asSpectator": false}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for _, c := range []*hub.Conn{a, b} {
		if _, err := c.Request(liveCtx(), "deck.select", deckSelectOf(liveDeck)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []*hub.Conn{a, b} {
		e, err := c.Request(liveCtx(), "player.ready", map[string]any{"ready": true})
		show(t, "player.ready", e, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for name, c := range map[string]*hub.Conn{"甲": a, "乙": b} {
		name, c := name, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			prompts := 0
			for {
				ev, err := c.Next(ctx)
				if err != nil {
					return
				}
				at := time.Since(start).Round(100 * time.Millisecond)
				if ev.Type != "room.snapshot" {
					t.Logf("%6v %s 推送 %s %s", at, name, ev.Type, trunc(string(ev.Payload), 300))
				}
				switch ev.Type {
				case "match.load_required":
					var p struct {
						LoadID int64 `json:"loadId"`
					}
					_ = ev.Decode(&p)
					go func() {
						t0 := time.Now()
						e, err := c.Request(ctx, "client.load_complete", map[string]any{"loadId": p.LoadID})
						t.Logf("%6v %s load_complete 回话（等了 %v）：%s %v", time.Since(start).Round(100*time.Millisecond), name,
							time.Since(t0).Round(100*time.Millisecond), e.Type, err)
					}()
				case "rules.prompt":
					var p hub.Prompt
					_ = ev.Decode(&p)
					if !p.Pending {
						continue
					}
					if prompts++; prompts > 6 {
						return
					}
					_, err := c.Request(ctx, "rules.respond", autoAnswer(p))
					if err != nil {
						t.Logf("%s 回答 %s 被拒：%v", name, p.Kind, err)
					}
				}
			}
		}()
	}
	wg.Wait()
}

// next 收客人那边的下一行（没有就等）。
func (g *guest) next(ctx context.Context) (map[string]any, bool) {
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
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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

// guestAnswer：独弈一侧像真客户端那样回应提问帧。能下地就下地、能施放就施放；
// 进攻全派、阻挡不挡；选牌、选目标选第一个；其余让过或确定。返回 nil 表示这帧不用回。
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
		if xs, ok := m[key].([]any); ok && len(xs) > 0 {
			return xs[0]
		}
		return nil
	}
	switch m["type"] {
	case "choice_request":
		switch m["kind"] {
		case "action":
			if m["manaPayment"] != nil {
				return c("ok", nil) // 自动支付
			}
			// Hexproof 的动作标签是「牌名 — play land／cast spell」：先下地，再施放。
			for _, want := range []string{"play land", "cast spell"} {
				for _, a := range asList(m["cardActions"]) {
					am := a.(map[string]any)
					label, _ := am["label"].(string)
					if strings.Contains(strings.ToLower(label), want) && !skip[am["cardId"]] {
						return c("select_card", map[string]any{"entityId": am["cardId"]})
					}
				}
			}
			return c("pass", nil)
		case "mulligan":
			if m["mulliganMode"] == "bottom_cards" {
				n := int(m["cardsToBottom"].(float64))
				legal := asList(m["legalCardIds"])
				sel := asList(m["selectedCardIds"])
				if len(sel) < n && len(sel) < len(legal) {
					return c("select_card", map[string]any{"entityId": legal[len(sel)]})
				}
				return c("ok", nil)
			}
			return c("ok", nil)
		case "attack":
			if len(asList(m["declaredAttackerIds"])) == 0 && len(asList(m["attackerCandidateIds"])) > 0 {
				return c("alpha_strike", nil)
			}
			return c("ok", nil)
		case "block", "confirm":
			return c("ok", nil)
		case "select":
			if id := first("candidatePlayerIds"); id != nil && len(asList(m["legalCardIds"])) == 0 {
				return c("select_player", map[string]any{"entityId": id})
			}
			if id := first("legalCardIds"); id != nil && len(asList(m["selectedCardIds"])) == 0 {
				return c("select_card", map[string]any{"entityId": id})
			}
			return c("ok", nil)
		case "locked":
			return nil
		}
	case "modal_request":
		if len(asList(m["selectedOptionIds"])) == 0 {
			if o, ok := first("options").(map[string]any); ok {
				return c("select_option", map[string]any{"entityId": o["id"]})
			}
		}
		return c("ok", nil)
	case "number_request":
		return c("submit_number", map[string]any{"value": m["min"]})
	case "text_request":
		return c("submit_text", map[string]any{"text": "Lightning Bolt"})
	case "order_request":
		ids := []any{}
		for _, it := range asList(m["sourceItems"]) {
			ids = append(ids, it.(map[string]any)["id"])
		}
		return c("submit_order", map[string]any{"orderedIds": ids})
	case "allocation_request":
		ts := asList(m["targets"])
		if len(ts) == 0 {
			return nil
		}
		return c("submit_allocation", map[string]any{"allocations": []map[string]any{{"id": ts[0].(map[string]any)["id"], "amount": m["total"]}}})
	case "reveal_request":
		return c("close_reveal", nil)
	}
	return nil
}

func asList(v any) []any {
	xs, _ := v.([]any)
	return xs
}

var _ = io.Discard
