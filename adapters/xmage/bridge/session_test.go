// SPDX-License-Identifier: MIT

package bridge

import (
	"encoding/json"
	"testing"
)

// rig：记下会话发给客人的帧和交给 XMage 的回答。
type rig struct {
	s       *Session
	frames  []map[string]any
	answers [][2]any
}

func newRig() *rig {
	r := &rig{}
	r.s = NewSession("zh-CN", Hooks{
		Send:   func(f map[string]any) { r.frames = append(r.frames, f) },
		Answer: func(op string, v any) error { r.answers = append(r.answers, [2]any{op, v}); return nil },
	})
	return r
}

func (r *rig) cb(method string, d any) {
	b, _ := json.Marshal(d)
	r.s.OnCallback(method, b)
}

func (r *rig) last(typ string) map[string]any {
	for i := len(r.frames) - 1; i >= 0; i-- {
		if r.frames[i]["type"] == typ {
			return r.frames[i]
		}
	}
	return nil
}

func (r *rig) cmd(command string, f func(c *Command)) {
	c := Command{Type: "command", PromptID: r.s.pid, Command: command}
	if f != nil {
		f(&c)
	}
	r.s.OnCommand(c)
}

func (r *rig) lastAnswer() [2]any {
	if len(r.answers) == 0 {
		return [2]any{}
	}
	return r.answers[len(r.answers)-1]
}

func ptr(n int) *int { return &n }

const me, opp = "p-me", "p-opp"

func view(extra func(v map[string]any)) map[string]any {
	v := map[string]any{
		"turn": 3, "phase": "PRECOMBAT_MAIN", "step": "PRECOMBAT_MAIN", "activePlayerId": me, "myPlayerId": me,
		"players": []map[string]any{
			{"id": me, "name": "我", "life": 20, "libraryCount": 40, "handCount": 1, "manaPool": map[string]int{},
				"battlefield": []map[string]any{
					{"id": "land1", "name": "Mountain", "types": "Basic Land - Mountain", "land": true},
					{"id": "land2", "name": "Mountain", "types": "Basic Land - Mountain", "land": true},
					{"id": "bear", "name": "Grizzly Bears", "types": "Creature - Bear", "creature": true, "power": "2", "toughness": "2"},
				}},
			{"id": opp, "name": "对手", "life": 20, "libraryCount": 40, "handCount": 3, "manaPool": map[string]int{},
				"battlefield": []map[string]any{
					{"id": "wall", "name": "Wall of Wood", "types": "Creature - Wall", "creature": true, "power": "0", "toughness": "3"},
					{"id": "pw", "name": "Chandra", "types": "Legendary Planeswalker - Chandra", "loyalty": "4"},
				}},
		},
		"hand":     []map[string]any{{"id": "bolt", "name": "Lightning Bolt", "cost": "{R}", "types": "Instant"}},
		"stack":    []any{},
		"combat":   []any{},
		"playable": map[string]any{"bolt": map[string]any{"abilityIds": []string{"a1"}, "names": []string{"Cast Lightning Bolt"}}},
	}
	if extra != nil {
		extra(v)
	}
	return v
}

func TestPriorityPlayAndPass(t *testing.T) {
	r := newRig()
	r.cb("GAME_SELECT", map[string]any{"message": "Play spells and abilities", "options": map[string]any{"queryType": "SELECT"}, "view": view(nil)})
	st := r.last("state")
	if st == nil || st["phase"] != "MAIN1" || len(st["players"].([]map[string]any)) != 2 {
		t.Fatal("要先发局面", st)
	}
	cr := r.last("choice_request")
	if cr["kind"] != "action" || len(cr["legalCardIds"].([]int)) != 1 {
		t.Fatal(cr)
	}
	bolt := cr["legalCardIds"].([]int)[0]
	r.cmd("select_card", func(c *Command) { c.EntityID = ptr(r.s.ids.of("land1")) })
	if r.last("error") == nil || len(r.answers) != 0 {
		t.Fatal("不在可用名单里的牌要拦下")
	}
	r.cmd("select_card", func(c *Command) { c.EntityID = ptr(bolt) })
	if a := r.lastAnswer(); a[0] != "uuid" || a[1] != "bolt" {
		t.Fatal(a)
	}
	r.cb("GAME_SELECT", map[string]any{"message": "Play spells and abilities", "options": map[string]any{}, "view": view(nil)})
	r.cmd("pass", nil)
	if a := r.lastAnswer(); a[0] != "bool" || a[1] != false {
		t.Fatal("让过＝false", a)
	}
}

func TestOpponentHandIsHidden(t *testing.T) {
	r := newRig()
	r.cb("GAME_UPDATE", map[string]any{"view": view(nil)})
	for _, p := range r.last("state")["players"].([]map[string]any) {
		if p["name"] == "对手" {
			hand := p["hand"].([]map[string]any)
			if len(hand) != 3 || hand[0]["hidden"] != true {
				t.Fatal("对手的手牌只给张数，要补成牌背", hand)
			}
		}
	}
}

func TestMulligan(t *testing.T) {
	r := newRig()
	ask := map[string]any{"message": "Mulligan <font color=#ffff00>down to 6 cards</font>?", "options": map[string]any{"UI.left.btn.text": "Mulligan", "UI.right.btn.text": "Keep"}, "view": view(nil)}
	r.cb("GAME_ASK", ask)
	if r.last("choice_request")["kind"] != "mulligan" || r.last("prompt")["message"] != "Mulligan down to 6 cards?" {
		t.Fatal(r.last("prompt"))
	}
	r.cmd("ok", nil)
	if a := r.lastAnswer(); a[1] != false {
		t.Fatal("保留＝false", a)
	}
	r.cb("GAME_ASK", ask)
	r.cmd("cancel", nil)
	if a := r.lastAnswer(); a[1] != true {
		t.Fatal("调度＝true", a)
	}
}

func TestAttackAnswersDefenderQuestion(t *testing.T) {
	r := newRig()
	sel := map[string]any{"message": "Select attackers", "options": map[string]any{"possibleAttackers": []string{"bear"}, "specialButton": "All attack"}, "view": view(nil)}
	r.cb("GAME_SELECT", sel)
	cr := r.last("choice_request")
	if cr["kind"] != "attack" || cr["currentDefenderPlayerId"] != r.s.seatOf(opp) || len(cr["attackDefenderCardIds"].([]int)) != 1 {
		t.Fatal(cr)
	}
	// 先把防御对象换成鹏洛客，再点熊
	r.cmd("select_card", func(c *Command) { c.EntityID = ptr(r.s.ids.of("pw")) })
	if r.last("choice_request")["currentDefenderCardId"] != r.s.ids.of("pw") || len(r.answers) != 0 {
		t.Fatal("点鹏洛客只换防御对象，不回答 XMage")
	}
	r.cmd("select_card", func(c *Command) { c.EntityID = ptr(r.s.ids.of("bear")) })
	if a := r.lastAnswer(); a[1] != "bear" {
		t.Fatal(a)
	}
	n := len(r.frames)
	r.cb("GAME_TARGET", map[string]any{"message": "Select a player or planeswalker", "targets": []string{opp, "pw"}, "flag": true, "view": view(nil)})
	if a := r.lastAnswer(); a[0] != "uuid" || a[1] != "pw" || len(r.frames) != n {
		t.Fatal("XMage 追问打谁：照独弈选的替玩家答，不发给客人", a)
	}
	r.cb("GAME_SELECT", sel)
	r.cmd("ok", nil)
	if a := r.lastAnswer(); a[0] != "bool" || a[1] != true {
		t.Fatal("宣告完毕＝true", a)
	}
	r.cb("GAME_SELECT", sel)
	r.cmd("alpha_strike", nil)
	if a := r.lastAnswer(); a[0] != "string" || a[1] != "special" {
		t.Fatal("全部进攻＝special", a)
	}
}

func TestBlockReassign(t *testing.T) {
	r := newRig()
	withCombat := func(blocking string) map[string]any {
		return view(func(v map[string]any) {
			v["activePlayerId"] = opp
			groups := []map[string]any{
				{"defenderId": me, "attackers": []string{"atkA"}, "blockers": []string{}},
				{"defenderId": me, "attackers": []string{"atkB"}, "blockers": []string{}},
			}
			if blocking != "" {
				for _, g := range groups {
					if g["attackers"].([]string)[0] == blocking {
						g["blockers"] = []string{"bear"}
					}
				}
			}
			v["combat"] = groups
		})
	}
	r.cb("GAME_SELECT", map[string]any{"message": "Select blockers", "options": map[string]any{"possibleBlockers": []string{"bear"}}, "view": withCombat("atkA")})
	cr := r.last("choice_request")
	if cr["kind"] != "block" || len(cr["blockerCandidates"].([]map[string]any)) != 1 {
		t.Fatal(cr)
	}
	// 熊正挡着 A，改挡 B：先撤（点熊），XMage 再问一次阻挡时再点熊，追问挡谁时答 B。
	r.cmd("assign_block", func(c *Command) { c.EntityID = ptr(r.s.ids.of("bear")); c.TargetID = ptr(r.s.ids.of("atkB")) })
	if a := r.lastAnswer(); a[1] != "bear" {
		t.Fatal(a)
	}
	n := len(r.frames)
	r.cb("GAME_SELECT", map[string]any{"message": "Select blockers", "options": map[string]any{"possibleBlockers": []string{"bear"}}, "view": withCombat("")})
	if a := r.lastAnswer(); len(r.answers) != 2 || a[1] != "bear" || len(r.frames) != n {
		t.Fatal("排好的第二下要自动点", r.answers)
	}
	r.cb("GAME_TARGET", map[string]any{"message": "Select attacker to block", "targets": []string{"atkA", "atkB"}, "view": withCombat("")})
	if a := r.lastAnswer(); a[1] != "atkB" {
		t.Fatal(a)
	}
}

func TestTargetPlayerAndRequired(t *testing.T) {
	r := newRig()
	r.cb("GAME_TARGET", map[string]any{"message": "Select any target", "targets": []string{opp, me, "wall"}, "flag": true,
		"options": map[string]any{"chosenTargets": []string{}}, "view": view(nil)})
	cr := r.last("choice_request")
	if cr["kind"] != "select" || len(cr["candidatePlayerIds"].([]int)) != 2 || len(cr["legalCardIds"].([]int)) != 1 {
		t.Fatal(cr)
	}
	r.cmd("ok", nil)
	if r.last("error") == nil || len(r.answers) != 0 {
		t.Fatal("必选的提问不能直接完成")
	}
	r.cmd("select_player", func(c *Command) { c.EntityID = ptr(r.s.seatOf(opp)) })
	if a := r.lastAnswer(); a[1] != opp {
		t.Fatal(a)
	}
}

func TestAutopayClicksSources(t *testing.T) {
	r := newRig()
	mana := func() map[string]any {
		return map[string]any{"message": "Pay {1}{R}", "view": view(func(v map[string]any) {
			v["playable"] = map[string]any{"land1": map[string]any{"names": []string{"{T}: Add {R}"}}, "land2": map[string]any{"names": []string{"{T}: Add {R}"}}}
		})}
	}
	r.cb("GAME_PLAY_MANA", mana())
	if r.last("choice_request")["manaPayment"] == nil {
		t.Fatal("付费提问要带 manaPayment")
	}
	r.cmd("ok", nil)
	first := r.lastAnswer()[1]
	r.cb("GAME_PLAY_MANA", mana())
	second := r.lastAnswer()[1]
	if len(r.answers) != 2 || first == second {
		t.Fatal("自动支付要逐个点不同的来源", r.answers)
	}
	r.cb("GAME_PLAY_MANA", mana())
	if len(r.answers) != 2 || r.last("error") == nil || r.last("choice_request")["manaPayment"] == nil {
		t.Fatal("来源点完还没付清：停下来报错、把提问交还玩家", r.answers)
	}
}

func TestMultiAmountAnswerFormat(t *testing.T) {
	r := newRig()
	r.cb("GAME_GET_MULTI_AMOUNT", map[string]any{"message": "", "min": 3, "max": 3, "options": map[string]any{"title": "Add mana"},
		"multi": []map[string]any{{"message": "R", "min": 0, "max": 3}, {"message": "G", "min": 0, "max": 3}}, "view": view(nil)})
	if r.last("allocation_request") == nil {
		t.Fatal("要发分配提问")
	}
	r.cmd("submit_allocation", func(c *Command) {
		c.Allocations = append(c.Allocations, struct {
			ID     int `json:"id"`
			Amount int `json:"amount"`
		}{ID: 1, Amount: 3})
	})
	if a := r.lastAnswer(); a[0] != "string" || a[1] != "0 3" {
		t.Fatal("XMage 要空格分隔的一串数", a)
	}
}

func TestGameOverReadsMatchInfo(t *testing.T) {
	r := newRig()
	r.cb("GAME_UPDATE", map[string]any{"view": view(nil)})
	lost, _ := json.Marshal(map[string]any{"won": false, "wins": 0, "loses": 0, "winsNeeded": 1, "matchInfo": "对手 won the match!"})
	if !r.s.GameOver(lost, 1) || r.last("game_over")["winner"] != "对手" {
		t.Fatal("输家的 loses 也是 0，要看 matchInfo 才知道这盘完了", r.last("game_over"))
	}
	mid, _ := json.Marshal(map[string]any{"won": true, "wins": 1, "winsNeeded": 2, "matchInfo": "Match score: 1 - 0"})
	if r.s.GameOver(mid, 1) {
		t.Fatal("三局两胜赢了第一局，盘还没完")
	}
}

func TestStaleAndIdleCommands(t *testing.T) {
	r := newRig()
	r.cmd("pass", nil)
	if len(r.answers) != 0 {
		t.Fatal("没在等回答时的让过要丢掉")
	}
	r.cb("GAME_SELECT", map[string]any{"message": "Play spells and abilities", "options": map[string]any{}, "view": view(nil)})
	r.s.OnCommand(Command{PromptID: r.s.pid + 5, Command: "pass"})
	if r.last("error")["code"] != "stale_prompt" || len(r.answers) != 0 {
		t.Fatal("过期的提问编号要拒")
	}
	r.cmd("concede", nil)
	if a := r.lastAnswer(); a[0] != "action" || a[1] != "CONCEDE" {
		t.Fatal(a)
	}
}

func TestPickTriggerFromCards(t *testing.T) {
	r := newRig()
	r.cb("GAME_TARGET", map[string]any{"message": "Pick triggered ability (goes to the stack first)", "targets": []string{}, "flag": true,
		"cards1": []map[string]any{{"id": "trig1", "name": "Prowess", "ability": true}, {"id": "trig2", "name": "Prowess", "ability": true}}, "view": view(nil)})
	cr := r.last("choice_request")
	if len(cr["legalCardIds"].([]int)) != 2 || len(r.last("state")["revealedCards"].([]map[string]any)) != 2 {
		t.Fatal("候选在 cards1 里时要拿它当可选，并放进 revealedCards 画出来", cr)
	}
	r.cmd("select_card", func(c *Command) { c.EntityID = ptr(r.s.ids.of("trig2")) })
	if a := r.lastAnswer(); a[1] != "trig2" {
		t.Fatal(a)
	}
}
