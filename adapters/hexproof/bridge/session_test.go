// SPDX-License-Identifier: GPL-3.0-or-later

package bridge

import (
	"encoding/json"
	"os"
	"sync"
	"testing"

	"tablenet-adapter-hexproof/hub"
)

// 设了 BRIDGE_DUMP=<文件> 时，所有测试发出的帧都追加写进去，交给独弈仓库的 arcane.v0 schema 校验
// （tools/validate-bridge-frames.mjs）。
var dumpMu sync.Mutex

type rec struct {
	t        *testing.T
	frames   []map[string]any
	answers  []hub.Respond
	conceded int
	degraded []string
}

func newRec(t *testing.T) (*rec, *Session) {
	r := &rec{t: t}
	s := NewSession(1, "zh-CN", Hooks{
		Send: func(f map[string]any) {
			// 走一遍 JSON，和真发出去的一样（int 变 float64）。
			b, _ := json.Marshal(f)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			r.frames = append(r.frames, m)
			if p := os.Getenv("BRIDGE_DUMP"); p != "" {
				dumpMu.Lock()
				fh, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = fh.Write(append(b, '\n'))
					fh.Close()
				}
				dumpMu.Unlock()
			}
		},
		Respond:  func(a hub.Respond) error { r.answers = append(r.answers, a); return nil },
		Concede:  func() error { r.conceded++; return nil },
		Degraded: func(kind, why string) { r.degraded = append(r.degraded, kind) },
	})
	return r, s
}

func (r *rec) last(typ string) map[string]any {
	r.t.Helper()
	for i := len(r.frames) - 1; i >= 0; i-- {
		if r.frames[i]["type"] == typ {
			return r.frames[i]
		}
	}
	r.t.Fatalf("没发过 %s；发过的：%v", typ, types(r.frames))
	return nil
}

func types(fs []map[string]any) []any {
	out := []any{}
	for _, f := range fs {
		out = append(out, f["type"])
	}
	return out
}

func (r *rec) answer() hub.Respond {
	r.t.Helper()
	if len(r.answers) == 0 {
		r.t.Fatal("没交回答")
	}
	return r.answers[len(r.answers)-1]
}

func ptr(n int) *int { return &n }

func cmd(s *Session, command string, ent *int) {
	s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: command, EntityID: ent})
}

func (s *Session) idOf(hexID string) *int { n := s.ids.of(hexID); return &n }

func snapshot() hub.Snapshot {
	return hub.Snapshot{
		Turn: 3, Step: "MAIN1", ActiveSeat: 1, PrioritySeat: 1,
		Players: []hub.PlayerState{{Seat: 0, Name: "对手", Life: 18}, {Seat: 1, Name: "我", Life: 20,
			Mana: []hub.Counter{{Name: "R", Value: 1}}}},
		Zones: []hub.ZoneState{
			{Zone: "hand", OwnerSeat: 1, Count: 2, Cards: []hub.CardState{
				{ID: "c-bolt", Visible: true, Identity: &hub.CardIdentity{Name: "Lightning Bolt", SetCode: "M10"}, OwnerSeat: 1, ControllerSeat: 1},
				{ID: "c-grizzly", Visible: true, Identity: &hub.CardIdentity{Name: "Grizzly Bears"}, OwnerSeat: 1, ControllerSeat: 1}}},
			{Zone: "hand", OwnerSeat: 0, Count: 3},
			{Zone: "library", OwnerSeat: 1, Count: 40},
			{Zone: "battlefield", OwnerSeat: 1, Count: 2, Cards: []hub.CardState{
				{ID: "c-mountain", Visible: true, Identity: &hub.CardIdentity{Name: "Mountain"}, OwnerSeat: 1, ControllerSeat: 1, Tapped: true},
				{ID: "c-goblin", Visible: true, Identity: &hub.CardIdentity{Name: "Goblin Guide"}, OwnerSeat: 1, ControllerSeat: 1,
					Power: "2", Toughness: "2", Attacking: true, AttackingPlayer: ptr(0), Counters: []hub.Counter{{Name: "P1P1", Value: 1}}}}},
			{Zone: "battlefield", OwnerSeat: 0, Count: 1, Cards: []hub.CardState{
				{ID: "c-wall", Visible: true, Identity: &hub.CardIdentity{Name: "Wall of Omens"}, OwnerSeat: 0, ControllerSeat: 0,
					Power: "0", Toughness: "4", Blocking: []string{"c-goblin"}}}},
		},
		Stack: []hub.StackObject{{ID: "s1", SourceID: "c-bolt", ControllerSeat: 1, Identity: hub.CardIdentity{Name: "Lightning Bolt"},
			Text: "deal 3", Targets: []hub.StackTarget{{Kind: "player", Seat: ptr(0)}}}},
	}
}

func TestStateFrame(t *testing.T) {
	r, s := newRec(t)
	s.OnSnapshot(snapshot())
	st := r.last("state")
	if st["phase"] != "MAIN1" || st["viewingPlayerId"] != float64(1) || st["stackSize"] != float64(1) {
		t.Fatal(st)
	}
	players := st["players"].([]any)
	me := players[1].(map[string]any)
	if me["isYou"] != true || me["libraryCount"] != float64(40) || len(me["hand"].([]any)) != 2 {
		t.Fatal(me)
	}
	if me["manaPool"].(map[string]any)["R"] != float64(1) {
		t.Fatal(me["manaPool"])
	}
	opp := players[0].(map[string]any)
	hand := opp["hand"].([]any)
	if len(hand) != 3 || hand[0].(map[string]any)["hidden"] != true {
		t.Fatal("对手手牌应当是 3 张牌背", hand)
	}
	att := st["combat"].(map[string]any)["attackers"].([]any)[0].(map[string]any)
	if att["attackerId"] != float64(*s.idOf("c-goblin")) || len(att["blockerIds"].([]any)) != 1 {
		t.Fatal(att)
	}
	goblin := me["battlefield"].([]any)[1].(map[string]any)
	if goblin["power"] != float64(2) || goblin["name"] != "Goblin Guide" {
		t.Fatal(goblin)
	}
	if st["stack"].([]any)[0].(map[string]any)["targetPlayerIds"].([]any)[0] != float64(0) {
		t.Fatal(st["stack"])
	}
}

func TestPriorityActions(t *testing.T) {
	r, s := newRec(t)
	s.OnSnapshot(snapshot())
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 41, Kind: "chooseAction", Supported: true, Title: "Choose an action",
		Options: []hub.PromptOption{
			{ResponseID: "a1", Kind: "cast", Label: "Cast Lightning Bolt", CardID: "c-bolt"},
			{ResponseID: "a2", Kind: "cast", Label: "Cast Grizzly Bears", CardID: "c-grizzly"},
			{ResponseID: "a3", Kind: "activateAbility", Label: "Exert", CardID: "c-goblin"},
			{ResponseID: "a4", Kind: "activateAbility", Label: "Sacrifice", CardID: "c-goblin"},
			{ResponseID: "$pass", Kind: "pass", Label: "Pass priority"}}})
	cr := r.last("choice_request")
	if cr["kind"] != "action" || len(cr["legalCardIds"].([]any)) != 3 {
		t.Fatal(cr)
	}
	if b := r.last("buttons"); b["okEnabled"] != true {
		t.Fatal(b)
	}
	// 只有一种动作的牌：点一下就交。
	cmd(s, "select_card", s.idOf("c-bolt"))
	if a := r.answer(); a.ResponseID != "a1" || a.PromptID != 41 {
		t.Fatal(a)
	}
	// 有两种动作的牌：先弹选单，再选。
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 42, Kind: "chooseAction", Supported: true, Options: []hub.PromptOption{
		{ResponseID: "a3", Kind: "activateAbility", Label: "Exert", CardID: "c-goblin"},
		{ResponseID: "a4", Kind: "activateAbility", Label: "Sacrifice", CardID: "c-goblin"}}})
	cmd(s, "select_card", s.idOf("c-goblin"))
	modal := r.last("modal_request")
	if modal["kind"] != "ability" || len(modal["options"].([]any)) != 2 {
		t.Fatal(modal)
	}
	cmd(s, "select_option", ptr(1))
	if a := r.answer(); a.ResponseID != "a4" {
		t.Fatal(a)
	}
	// 让过。
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 43, Kind: "chooseAction", Supported: true})
	cmd(s, "pass", nil)
	if a := r.answer(); a.ResponseID != "$pass" || a.PromptID != 43 {
		t.Fatal(a)
	}
	// 提问已经交了，再发指令不该再交一次。
	n := len(r.answers)
	cmd(s, "pass", nil)
	if len(r.answers) != n {
		t.Fatal("交过的提问不能再交")
	}
}

func TestStalePromptRejected(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 1, Kind: "chooseAction", Supported: true})
	s.OnCommand(Command{Type: "command", PromptID: s.pid + 7, Command: "pass"})
	if len(r.answers) != 0 || r.last("error")["code"] != "stale_prompt" {
		t.Fatal("旧提问编号的指令要拒")
	}
}

func TestAttackers(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 9, Kind: "chooseAttackers", Supported: true,
		CombatSources: []hub.CombatSource{
			{ResponseID: "src:0", ObjectID: "c-goblin", ValidTargetIDs: []string{"def:p0", "def:pw"}},
			{ResponseID: "src:1", ObjectID: "c-elf", ValidTargetIDs: []string{"def:p0"}}},
		CombatTargets: []hub.CombatTarget{
			{ResponseID: "def:p0", Kind: "player", Seat: ptr(0)},
			{ResponseID: "def:pw", Kind: "card", ObjectID: "c-jace"}}})
	cr := r.last("choice_request")
	if cr["kind"] != "attack" || len(cr["attackerCandidateIds"].([]any)) != 2 || cr["currentDefenderPlayerId"] != float64(0) {
		t.Fatal(cr)
	}
	cmd(s, "select_card", s.idOf("c-jace")) // 之后的进攻者打鹏洛客
	cmd(s, "select_card", s.idOf("c-goblin"))
	cmd(s, "select_card", s.idOf("c-elf")) // 打不了鹏洛客：退到它能打的玩家
	if got := r.last("choice_request")["declaredAttackerIds"].([]any); len(got) != 2 {
		t.Fatal(got)
	}
	cmd(s, "select_card", s.idOf("c-elf")) // 再点一下撤回
	cmd(s, "ok", nil)
	a := r.answer()
	if a.ResponseID != "$submit" || len(a.Assignments) != 1 || a.Assignments[0] != (hub.Assignment{SourceID: "src:0", TargetID: "def:pw"}) {
		t.Fatal(a)
	}
	// 全部进攻。
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 10, Kind: "chooseAttackers", Supported: true,
		CombatSources: []hub.CombatSource{{ResponseID: "src:0", ObjectID: "c-goblin", ValidTargetIDs: []string{"def:p0"}}},
		CombatTargets: []hub.CombatTarget{{ResponseID: "def:p0", Kind: "player", Seat: ptr(0)}}})
	cmd(s, "alpha_strike", nil)
	cmd(s, "ok", nil)
	if a := r.answer(); len(a.Assignments) != 1 {
		t.Fatal(a)
	}
}

func TestBlockers(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 11, Kind: "chooseBlockers", Supported: true,
		CombatSources: []hub.CombatSource{
			{ResponseID: "b:0", ObjectID: "c-wall", ValidTargetIDs: []string{"a:0", "a:1"}, MaxAssignments: 1},
			{ResponseID: "b:1", ObjectID: "c-bear", ValidTargetIDs: []string{"a:1"}, MaxAssignments: 1}},
		CombatTargets: []hub.CombatTarget{{ResponseID: "a:0", ObjectID: "c-goblin"}, {ResponseID: "a:1", ObjectID: "c-ogre"}}})
	cr := r.last("choice_request")
	if cr["kind"] != "block" || len(cr["blockerCandidates"].([]any)) != 2 {
		t.Fatal(cr)
	}
	assign := func(blocker, attacker string) {
		s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: "assign_block", EntityID: s.idOf(blocker), TargetID: s.idOf(attacker)})
	}
	assign("c-wall", "c-goblin")
	assign("c-wall", "c-ogre") // 只能挡一个：改挡
	s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: "block_all"})
	cmd(s, "ok", nil)
	a := r.answer()
	want := []hub.Assignment{{SourceID: "b:0", TargetID: "a:1"}, {SourceID: "b:1", TargetID: "a:1"}}
	if len(a.Assignments) != 2 || a.Assignments[0] != want[0] || a.Assignments[1] != want[1] {
		t.Fatal(a.Assignments)
	}
	// 挡不了的组合要拒。
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 12, Kind: "chooseBlockers", Supported: true,
		CombatSources: []hub.CombatSource{{ResponseID: "b:1", ObjectID: "c-bear", ValidTargetIDs: []string{"a:1"}}},
		CombatTargets: []hub.CombatTarget{{ResponseID: "a:0", ObjectID: "c-goblin"}, {ResponseID: "a:1", ObjectID: "c-ogre"}}})
	assign("c-bear", "c-goblin")
	if r.last("error")["code"] != "rejected" {
		t.Fatal("挡不了的要拒")
	}
}

func TestChooseCardsAndTargets(t *testing.T) {
	r, s := newRec(t)
	s.OnSnapshot(snapshot())
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 20, Kind: "chooseCards", Supported: true, MinCardSelections: 1, MaxCardSelections: 2,
		Cards: []hub.PromptCard{{ID: "k1", Name: "Island"}, {ID: "k2", Name: "Swamp"}, {ID: "k3", Name: "Forest", ReadOnly: true}}})
	if rc := r.last("state")["revealedCards"].([]any); len(rc) != 3 {
		t.Fatal("要选的牌要放进 revealedCards", rc)
	}
	if cr := r.last("choice_request"); len(cr["legalCardIds"].([]any)) != 2 || cr["pickFromList"] != true {
		t.Fatal(cr)
	}
	cmd(s, "select_card", s.idOf("k3")) // 只读的不能选
	if len(r.answers) != 0 {
		t.Fatal()
	}
	cmd(s, "select_card", s.idOf("k1"))
	cmd(s, "select_card", s.idOf("k2")) // 选满上限即交
	if a := r.answer(); a.ResponseID != "$submit" || len(a.CardIDs) != 2 {
		t.Fatal(a)
	}

	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 21, Kind: "chooseBoardTargets", Supported: true, MinSelections: 1, MaxSelections: 1,
		Cancellable: true, ContextCards: []hub.PromptCard{{ID: "c-bolt", Name: "Lightning Bolt"}},
		Targets: []hub.PromptTarget{{ResponseID: "target:0", Kind: "player", Seat: ptr(0)}, {ResponseID: "target:1", Kind: "card", ObjectID: "c-wall"}}})
	cr := r.last("choice_request")
	if cr["candidatePlayerIds"].([]any)[0] != float64(0) || cr["targetSource"].(map[string]any)["name"] != "Lightning Bolt" {
		t.Fatal(cr)
	}
	s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: "select_player", EntityID: ptr(0)})
	if a := r.answer(); a.TargetIDs[0] != "target:0" {
		t.Fatal(a)
	}
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 22, Kind: "chooseBoardTargets", Supported: true, MinSelections: 1, MaxSelections: 1, Cancellable: true,
		Targets: []hub.PromptTarget{{ResponseID: "target:0", Kind: "player", Seat: ptr(0)}}})
	cmd(s, "cancel", nil)
	if a := r.answer(); a.ResponseID != "$cancel" {
		t.Fatal(a)
	}
}

func TestScalarPrompts(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 30, Kind: "chooseBoolean", Supported: true,
		Choices: []hub.PromptChoice{{ResponseID: "yes", Label: "Yes"}, {ResponseID: "no", Label: "No"}}})
	if b := r.last("buttons"); b["ok"] != "Yes" || b["cancel"] != "No" {
		t.Fatal(b)
	}
	cmd(s, "cancel", nil)
	if a := r.answer(); a.ChoiceIDs[0] != "no" {
		t.Fatal(a)
	}

	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 31, Kind: "chooseNumber", Supported: true, MinNumber: 0, MaxNumber: 5})
	s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: "submit_number", Value: ptr(9)})
	if len(r.answers) != 1 {
		t.Fatal("超出范围要拒")
	}
	s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: "submit_number", Value: ptr(3)})
	if a := r.answer(); *a.ChosenNumber != 3 {
		t.Fatal(a)
	}

	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 32, Kind: "chooseColor", Supported: true, MinChoiceTotal: 1, MaxChoiceTotal: 2,
		Choices: []hub.PromptChoice{{ResponseID: "W", Label: "White"}, {ResponseID: "U", Label: "Blue"}, {ResponseID: "B", Label: "Black"}}})
	cmd(s, "select_option", ptr(0))
	cmd(s, "select_option", ptr(2))
	cmd(s, "ok", nil)
	if a := r.answer(); len(a.ChoiceIDs) != 2 || a.ChoiceIDs[1] != "B" {
		t.Fatal(a)
	}

	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 33, Kind: "chooseCardName", Supported: true})
	s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: "submit_text", Text: " Lightning Bolt "})
	if a := r.answer(); a.Name != "Lightning Bolt" {
		t.Fatal(a)
	}

	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 34, Kind: "mulligan", Supported: true})
	cmd(s, "cancel", nil)
	if a := r.answer(); a.ResponseID != "$mulligan" {
		t.Fatal(a)
	}
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 35, Kind: "mulliganPutBack", Supported: true, RequiredSelections: 1,
		Cards: []hub.PromptCard{{ID: "h1", Name: "Island"}, {ID: "h2", Name: "Swamp"}}})
	cmd(s, "ok", nil) // 还没选
	cmd(s, "select_card", s.idOf("h2"))
	cmd(s, "ok", nil)
	if a := r.answer(); a.CardIDs[0] != "h2" || a.PromptID != 35 {
		t.Fatal(a)
	}
}

func TestOrderAndDamage(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 50, Kind: "reorder", Supported: true,
		OrderItems: []hub.PromptOrderItem{{ResponseID: "o1", Name: "Trigger A"}, {ResponseID: "o2", Name: "Trigger B"}}})
	items := r.last("order_request")["sourceItems"].([]any)
	second := int(items[1].(map[string]any)["id"].(float64))
	first := int(items[0].(map[string]any)["id"].(float64))
	s.OnCommand(Command{Type: "command", PromptID: s.pid, Command: "submit_order", OrderedIDs: []int{second, first}})
	if a := r.answer(); a.OrderedIDs[0] != "o2" || a.OrderedIDs[1] != "o1" {
		t.Fatal(a)
	}

	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 51, Kind: "chooseCombatDamageAssignment", Supported: true, TotalDamage: 5,
		DamageAssignmentMode: "ordered", DamageTargets: []hub.DamageTarget{
			{ResponseID: "d0", Kind: "card", ObjectID: "c-wall", LethalDamage: 4}, {ResponseID: "d1", Kind: "player"}}})
	targets := r.last("allocation_request")["targets"].([]any)
	id0 := int(targets[0].(map[string]any)["id"].(float64))
	id1 := int(targets[1].(map[string]any)["id"].(float64))
	bad := Command{Type: "command", PromptID: s.pid, Command: "submit_allocation"}
	bad.Allocations = append(bad.Allocations, struct {
		ID     int `json:"id"`
		Amount int `json:"amount"`
	}{id0, 4})
	s.OnCommand(bad) // 只分了 4 点
	if len(r.answers) != 1 {
		t.Fatal("没分完要拒")
	}
	good := bad
	good.Allocations = append(good.Allocations, struct {
		ID     int `json:"id"`
		Amount int `json:"amount"`
	}{id1, 1})
	s.OnCommand(good)
	if a := r.answer(); len(a.DamageAssignments) != 2 || a.DamageAssignments[1] != (hub.DamageAssignment{TargetID: "d1", Damage: 1}) {
		t.Fatal(a)
	}
}

func TestScryDegradesToPerCardDestinations(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 60, Kind: "scry", Supported: true,
		Cards:            []hub.PromptCard{{ID: "t1", Name: "Island"}, {ID: "t2", Name: "Ponder"}},
		ScryDestinations: []string{"libraryTop", "libraryBottom"}})
	if len(r.last("modal_request")["options"].([]any)) != 4 {
		t.Fatal()
	}
	cmd(s, "select_option", ptr(1)) // Island → 底
	cmd(s, "select_option", ptr(0)) // Island → 顶（改主意）
	cmd(s, "select_option", ptr(3)) // Ponder → 底
	cmd(s, "ok", nil)
	a := r.answer()
	if len(a.ScryPiles) != 2 || a.ScryPiles[0].Destination != "libraryTop" || a.ScryPiles[0].CardIDs[0] != "t1" {
		t.Fatal(a.ScryPiles)
	}
	if len(r.degraded) == 0 {
		t.Fatal("退化要记一笔")
	}
}

func TestNonQuestionsAndUnsupported(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 70, Kind: "acknowledge", Supported: true, Title: "Game notice", Detail: "你先手"})
	if a := r.answer(); a.ResponseID != "$ack" {
		t.Fatal("通知要替玩家回已看")
	}
	if r.last("message")["message"] != "你先手" {
		t.Fatal()
	}
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 71, Kind: "revealCards", Supported: true, Cards: []hub.PromptCard{{ID: "r1", Name: "Ponder"}}})
	if len(r.last("reveal_request")["items"].([]any)) != 1 {
		t.Fatal()
	}
	cmd(s, "close_reveal", nil)
	if a := r.answer(); a.ResponseID != "$ack" {
		t.Fatal(a)
	}
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 72, Kind: "voteSomething", Supported: false})
	if r.last("choice_request")["kind"] != "locked" || len(r.degraded) == 0 {
		t.Fatal("Hexproof 自己答不了的要锁住并提示")
	}
	cmd(s, "concede", nil)
	if r.conceded != 1 {
		t.Fatal("认输")
	}
}

// 真 hub 实测出的两处：回答被拒要重问（不然卡死）；付不起时不发 Hexproof 没提供的 $auto-pay。
func TestRejectedAnswerIsAskedAgain(t *testing.T) {
	r, s := newRec(t)
	fail := true
	s.h.Respond = func(a hub.Respond) error {
		r.answers = append(r.answers, a)
		if fail {
			fail = false
			return &hub.HubError{ErrorPayload: hub.ErrorPayload{Code: "rules_action_rejected"}}
		}
		return nil
	}
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 80, Kind: "chooseAction", Supported: true,
		Options: []hub.PromptOption{{ResponseID: "a1", Kind: "cast", Label: "Shock — cast spell", CardID: "c-shock"}}})
	first := s.pid
	cmd(s, "select_card", s.idOf("c-shock"))
	if r.last("error")["code"] != "rejected" || s.cur == nil || s.pid == first {
		t.Fatal("被拒后要换个提问编号重问")
	}
	cmd(s, "pass", nil)
	if a := r.answer(); a.ResponseID != "$pass" || a.PromptID != 80 {
		t.Fatal(a)
	}
}

func TestPaymentWithoutAutoPayIsRejectedLocally(t *testing.T) {
	r, s := newRec(t)
	s.OnPrompt(hub.Prompt{Pending: true, PromptID: 81, Kind: "payManaCost", Supported: true, Title: "Pay Mana Cost: {R}",
		Options: []hub.PromptOption{{ResponseID: "$cancel", Kind: "cancel", Label: "Cancel"}}})
	if b := r.last("buttons"); b["okEnabled"] != false {
		t.Fatal("没有自动支付时确定键要灰", b)
	}
	cmd(s, "ok", nil)
	if len(r.answers) != 0 || r.last("error")["code"] != "rejected" {
		t.Fatal("不能把没提供的选项发给 Hexproof")
	}
	cmd(s, "cancel", nil)
	if a := r.answer(); a.ResponseID != "$cancel" {
		t.Fatal(a)
	}
}

func TestGameOver(t *testing.T) {
	r, s := newRec(t)
	snap := snapshot()
	snap.GameOver, snap.WinnerSeat = true, ptr(1)
	s.OnSnapshot(snap)
	if g := r.last("game_over"); g["winner"] != "我" {
		t.Fatal(g)
	}
}
