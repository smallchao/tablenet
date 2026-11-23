// SPDX-License-Identifier: MIT

// Package bridge 在对局通道上冒充独弈主机：把 XMage 的局面和提问翻成 arcane.v0，把独弈客人前端的指令翻回 XMage 的回答。
//
// XMage 和独弈客人前端都是「点一下答一次」：XMage 每收到一个回答就重新问一次（进攻者一只只点、目标一个个选），
// 独弈客人每点一下发一条指令、等主机发来新的提问。所以这里大多是一对一转发，只有几处要适配器替玩家多答一步：
//   - 点了进攻者、XMage 追问「打谁」：照玩家在独弈里选定的防御对象替他答；
//   - 指定阻挡、XMage 追问「挡哪个进攻者」：照独弈指令里的进攻者替他答；
//   - 自动支付：XMage 没有自动横置，适配器替玩家逐个点可用的法术力来源。
//
// 对照表见 spec/08-桥接模式.md#xmage。
package bridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Hooks 是会话对外的出口。
type Hooks struct {
	// Send 发一帧给独弈客人。
	Send func(frame map[string]any)
	// Answer 回 XMage 当前的提问：op 是驱动的 uuid／bool／int／string／action，v 是值。
	Answer func(op string, v any) error
	// Degraded 在提问退化成通用选项时调用，适配器记日志用。
	Degraded func(kind, why string)
}

type Session struct {
	lab   labels
	ids   *ids
	h     Hooks
	seats map[string]int
	// seatIDs[i] 是 arcane 牌手 i 的 XMage UUID
	seatIDs []string

	view        *View
	cur         *Prompt
	kind        string // 当前提问的种类，见 classify
	pid         int64
	promptCards []Card

	defender        string // 进攻：之后点的进攻者打谁（XMage 的牌手或鹏洛客 UUID）
	pendingDefender string // 刚点了进攻者，XMage 若追问打谁，替玩家答这个
	pendingBlock    string // 刚点了阻挡者，XMage 若追问挡谁，替玩家答这个进攻者
	queue           []queued
	autopay         map[string]bool // 自动支付这一笔时已经点过的来源
	autopaying      bool
}

// queued：替玩家排好的下一步（例如改挡要先撤再挡）。下一个提问来了先做它，不发给客人。
type queued struct {
	kind string
	op   string
	v    any
}

func NewSession(locale string, h Hooks) *Session {
	if h.Degraded == nil {
		h.Degraded = func(string, string) {}
	}
	return &Session{lab: labels{zh: strings.HasPrefix(locale, "zh")}, ids: newIDs(), h: h, seats: map[string]int{}}
}

// HasPrompt：现在有没有在等这位玩家回答。
func (s *Session) HasPrompt() bool { return s.cur != nil }

// OnCallback 收一个 XMage 回调（驱动转来的 m 和 d）。
func (s *Session) OnCallback(method string, raw json.RawMessage) {
	switch method {
	case "GAME_INIT", "GAME_UPDATE":
		var d struct {
			View *View `json:"view"`
		}
		if decode(raw, &d) && d.View != nil {
			s.view = d.View
			s.h.Send(s.stateJSON())
		}
	case "GAME_UPDATE_AND_INFORM":
		var p Prompt
		if decode(raw, &p) && p.View != nil {
			s.view = p.View
			s.h.Send(s.stateJSON())
		}
	case "GAME_INFORM_PERSONAL":
		var p Prompt
		if decode(raw, &p) {
			if p.View != nil {
				s.view = p.View
			}
			if m := plain(p.Message); m != "" {
				s.h.Send(map[string]any{"type": "message", "title": "XMage", "message": m})
			}
		}
	case "GAME_ERROR":
		var p Prompt
		_ = decode(raw, &p)
		msg := plain(p.Message)
		if msg == "" {
			var v struct {
				Value string `json:"value"`
			}
			_ = decode(raw, &v)
			msg = plain(v.Value)
		}
		s.h.Send(map[string]any{"type": "error", "code": "rejected", "message": msg})
		s.autopaying = false
		s.emit()
	case "GAME_SELECT", "GAME_TARGET", "GAME_ASK", "GAME_CHOOSE_ABILITY", "GAME_CHOOSE_PILE", "GAME_CHOOSE_CHOICE",
		"GAME_PLAY_MANA", "GAME_PLAY_XMANA", "GAME_GET_AMOUNT", "GAME_GET_MULTI_AMOUNT":
		var p Prompt
		if !decode(raw, &p) {
			return
		}
		p.Method = method
		s.onPrompt(&p)
	case "SHOW_USERMESSAGE":
		var d struct {
			Lines []string `json:"lines"`
		}
		if decode(raw, &d) && len(d.Lines) > 0 {
			s.h.Send(map[string]any{"type": "message", "title": plain(d.Lines[0]), "message": plain(strings.Join(d.Lines[1:], "\n"))})
		}
	}
}

// GameOver：一局结束（END_GAME_INFO）。返回这一盘是不是也结束了。
func (s *Session) GameOver(raw json.RawMessage, gameNumber int) bool {
	var d struct {
		Won        bool `json:"won"`
		Wins       int  `json:"wins"`
		Loses      int  `json:"loses"`
		WinsNeeded int  `json:"winsNeeded"`
	}
	_ = decode(raw, &d)
	s.cur = nil
	var winner any
	if s.view != nil {
		for _, p := range s.view.Players {
			if (p.ID == s.view.MyPlayerID) == d.Won {
				winner = p.Name
				break
			}
		}
	}
	matchOver := d.WinsNeeded <= 0 || d.Wins >= d.WinsNeeded || d.Loses >= d.WinsNeeded
	s.h.Send(map[string]any{"type": "game_over", "winner": winner, "gameNumber": gameNumber, "matchOver": matchOver,
		"winCondition": "AllOpponentsLost", "winSpellEffect": nil, "outcomes": []string{}})
	return matchOver
}

func (s *Session) classify(p *Prompt) string {
	switch p.Method {
	case "GAME_SELECT":
		if _, ok := p.Options["POSSIBLE_ATTACKERS"]; ok {
			return "attack"
		}
		if _, ok := p.Options["POSSIBLE_BLOCKERS"]; ok {
			return "block"
		}
		return "priority"
	case "GAME_ASK":
		if strings.HasPrefix(plain(p.Message), "Mulligan") {
			return "mulligan"
		}
		return "ask"
	case "GAME_TARGET":
		return "target"
	case "GAME_CHOOSE_ABILITY":
		return "ability"
	case "GAME_CHOOSE_CHOICE":
		return "choice"
	case "GAME_CHOOSE_PILE":
		return "pile"
	case "GAME_PLAY_MANA", "GAME_PLAY_XMANA":
		return "mana"
	case "GAME_GET_AMOUNT":
		return "amount"
	case "GAME_GET_MULTI_AMOUNT":
		return "multi"
	}
	return "other"
}

func (s *Session) onPrompt(p *Prompt) {
	if p.View != nil {
		s.view = p.View
	}
	kind := s.classify(p)

	// 替玩家排好的下一步
	if len(s.queue) > 0 {
		q := s.queue[0]
		s.queue = s.queue[1:]
		if q.kind == kind {
			s.cur, s.kind = p, kind
			s.answer(q.op, q.v)
			return
		}
		s.queue = nil
	}
	// 进攻者追问打谁、阻挡者追问挡谁：照独弈里的选择替玩家答
	if kind == "target" && s.pendingDefender != "" && contains(p.Targets, s.pendingDefender) {
		d := s.pendingDefender
		s.pendingDefender = ""
		s.cur, s.kind = p, kind
		s.answer("uuid", d)
		return
	}
	if kind == "target" && s.pendingBlock != "" && contains(p.Targets, s.pendingBlock) {
		a := s.pendingBlock
		s.pendingBlock = ""
		s.cur, s.kind = p, kind
		s.answer("uuid", a)
		return
	}
	s.pendingDefender, s.pendingBlock = "", ""
	if kind == "mana" && s.autopaying {
		s.cur, s.kind = p, kind
		if s.autopayNext() {
			return
		}
		s.autopaying = false
		s.h.Send(map[string]any{"type": "error", "code": "rejected",
			"message": s.lab.t("自动支付付不起这笔费用：点法术力来源，或者取消", "Can't pay this automatically: tap a mana source or cancel")})
	}
	if kind != "mana" {
		s.autopaying, s.autopay = false, nil
	}

	s.cur, s.kind = p, kind
	s.pid++
	if kind == "attack" && (s.defender == "" || !s.validDefender(s.defender)) {
		s.defender = s.firstOpponent()
	}
	s.promptCards = nil
	if kind == "target" || kind == "pile" {
		s.promptCards = append(append([]Card{}, p.Cards1...), p.Cards2...)
	}
	if s.view != nil {
		s.h.Send(s.stateJSON())
	}
	s.emit()
}

// ---- 发帧 ----

func (s *Session) base(kind string) map[string]any {
	return map[string]any{
		"type": "choice_request", "promptId": s.pid, "kind": kind, "hasAvailableActions": nil,
		"legalCardIds": []int{}, "selectedCardIds": []int{}, "candidatePlayerIds": []int{}, "selectedPlayerIds": []int{},
		"mulliganMode": nil, "cardsToBottom": 0, "startingHandsRemaining": 0, "startingPlayerId": nil,
		"blockerCandidates": []any{}, "selectedAttackerId": nil, "attackDefenderCardIds": []int{},
		"declaredAttackerIds": []int{}, "attackerCandidateIds": []int{}, "cardActions": []any{}, "blockedPlays": []any{},
		"playableCardIds": []int{}, "currentDefenderCardId": nil, "currentDefenderPlayerId": nil,
		"manaPayment": nil, "targetSource": nil, "declinable": false, "pickFromList": false, "min": 0, "max": 0,
		"autoConfirmSeconds": nil,
	}
}

func (s *Session) promptFrame(msg string) map[string]any {
	return map[string]any{"type": "prompt", "promptId": s.pid, "player": s.myName(), "message": msg, "card": nil}
}

func (s *Session) buttons(ok, cancel string, okOn, cancelOn bool) map[string]any {
	return map[string]any{"type": "buttons", "promptId": s.pid, "player": s.myName(), "ok": ok, "cancel": cancel,
		"okEnabled": okOn, "cancelEnabled": cancelOn}
}

func (s *Session) myName() any {
	if s.view != nil {
		for _, p := range s.view.Players {
			if p.ID == s.view.MyPlayerID {
				return p.Name
			}
		}
	}
	return nil
}

func (s *Session) firstOpponent() string {
	if s.view == nil {
		return ""
	}
	for _, p := range s.view.Players {
		if p.ID != s.view.MyPlayerID && !p.HasLeft {
			return p.ID
		}
	}
	return ""
}

// validDefender：还能不能把它当防御对象（对手，或对手的鹏洛客／战役）。
func (s *Session) validDefender(id string) bool {
	if s.view == nil {
		return false
	}
	for _, p := range s.view.Players {
		if p.ID == s.view.MyPlayerID {
			continue
		}
		if p.ID == id && !p.HasLeft {
			return true
		}
		for _, c := range p.Battlefield {
			if c.ID == id {
				return true
			}
		}
	}
	return false
}

func (s *Session) intIDs(xs []string) []int {
	out := []int{}
	for _, x := range xs {
		out = append(out, s.ids.of(x))
	}
	return out
}

// emit 按当前提问发一组帧：prompt、具体的提问帧、buttons。
func (s *Session) emit() {
	p := s.cur
	if p == nil {
		return
	}
	L := s.lab
	msg := plain(p.Message)
	switch s.kind {
	case "priority":
		m := s.base("action")
		legal, actions := []int{}, []map[string]any{}
		for id, pl := range s.view.Playable {
			n := s.ids.of(id)
			legal = append(legal, n)
			for _, name := range pl.Names {
				actions = append(actions, map[string]any{"cardId": n, "label": plain(name)})
			}
		}
		m["legalCardIds"], m["playableCardIds"], m["cardActions"] = legal, legal, actions
		m["hasAvailableActions"] = len(legal) > 0
		s.h.Send(s.promptFrame(msg))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("让过", "Pass"), "", true, false))
	case "mana":
		m := s.base("action")
		legal := []int{}
		for id := range s.view.Playable {
			legal = append(legal, s.ids.of(id))
		}
		m["legalCardIds"] = legal
		m["manaPayment"] = map[string]any{"payerPlayerId": s.mySeat(), "remainingCost": strings.TrimPrefix(msg, "Pay "),
			"lifeAlreadyChosen": 0, "canPayLife": false, "sourceCardId": nil, "sourceName": "", "sourceZhName": "",
			"duringResolution": false, "autoTapCardIds": []int{}}
		s.h.Send(s.promptFrame(msg))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("自动支付", "Auto-pay"), L.t("取消", "Cancel"), len(legal) > 0, true))
	case "mulligan":
		m := s.base("mulligan")
		m["mulliganMode"] = "keep_or_mulligan"
		s.h.Send(s.promptFrame(msg))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("保留", "Keep"), L.t("调度", "Mulligan"), true, true))
	case "ask":
		m := s.base("confirm")
		yes, no := p.optionText("UI.left.btn.text"), p.optionText("UI.right.btn.text")
		if yes == "" {
			yes = L.t("是", "Yes")
		}
		if no == "" {
			no = L.t("否", "No")
		}
		s.h.Send(s.promptFrame(msg))
		s.h.Send(m)
		s.h.Send(s.buttons(yes, no, true, true))
	case "target":
		m := s.base("select")
		cards, players, selCards, selPlayers := []int{}, []int{}, []int{}, []int{}
		chosen := p.optionIDs("chosenTargets")
		for _, t := range p.Targets {
			if s.isPlayer(t) {
				players = append(players, s.seatOf(t))
				if contains(chosen, t) {
					selPlayers = append(selPlayers, s.seatOf(t))
				}
			} else {
				cards = append(cards, s.ids.of(t))
				if contains(chosen, t) {
					selCards = append(selCards, s.ids.of(t))
				}
			}
		}
		m["legalCardIds"], m["candidatePlayerIds"], m["selectedCardIds"], m["selectedPlayerIds"] = cards, players, selCards, selPlayers
		m["pickFromList"] = len(p.Cards1) > 0
		m["min"], m["max"] = 0, len(cards)+len(players)
		if p.Flag {
			m["min"] = 1
		}
		s.h.Send(s.promptFrame(msg))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("完成", "Done"), "", !p.Flag, false))
	case "attack":
		m := s.base("attack")
		cands := s.intIDs(p.optionIDs("POSSIBLE_ATTACKERS"))
		declared := []int{}
		for _, g := range s.view.Combat {
			declared = append(declared, s.intIDs(g.Attackers)...)
		}
		players, pw := []int{}, []int{}
		for _, pl := range s.view.Players {
			if pl.ID == s.view.MyPlayerID || pl.HasLeft {
				continue
			}
			players = append(players, s.seatOf(pl.ID))
			for _, c := range pl.Battlefield {
				if strings.Contains(c.Types, "Planeswalker") || strings.Contains(c.Types, "Battle") {
					pw = append(pw, s.ids.of(c.ID))
				}
			}
		}
		m["attackerCandidateIds"], m["legalCardIds"], m["declaredAttackerIds"] = cands, cands, declared
		m["attackDefenderCardIds"], m["candidatePlayerIds"] = pw, players
		if s.isPlayer(s.defender) {
			m["currentDefenderPlayerId"] = s.seatOf(s.defender)
		} else if s.defender != "" {
			m["currentDefenderCardId"] = s.ids.of(s.defender)
		}
		s.h.Send(s.promptFrame(msg))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("确定", "OK"), "", true, false))
	case "block":
		m := s.base("block")
		attackers := []int{}
		for _, g := range s.view.Combat {
			attackers = append(attackers, s.intIDs(g.Attackers)...)
		}
		cands := []map[string]any{}
		for _, b := range p.optionIDs("POSSIBLE_BLOCKERS") {
			cands = append(cands, map[string]any{"blockerId": s.ids.of(b), "attackerIds": attackers})
		}
		m["blockerCandidates"] = cands
		s.h.Send(s.promptFrame(msg))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("确定", "OK"), "", true, false))
	case "ability":
		opts := []map[string]any{}
		for i, c := range p.Choices {
			opts = append(opts, map[string]any{"id": i, "label": plain(c.Text)})
		}
		s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "ability", "title": firstNonEmpty(msg, L.t("选一种用法", "Choose an action")),
			"message": "", "min": 1, "max": 1, "cancelEnabled": true, "options": opts, "revealedCards": []any{}, "revealTitle": "",
			"selectedOptionIds": []int{}})
	case "choice":
		c := p.Choice
		if c == nil {
			s.degraded()
			return
		}
		title := firstNonEmpty(plain(c.Message), msg)
		if c.Search && len(c.Items) > 40 {
			// 说出牌名一类：可搜索的长名单，用独弈的输入框，答案按文字对回选项
			s.h.Send(map[string]any{"type": "text_request", "promptId": s.pid, "title": title, "message": plain(c.SubMessage),
				"text": "", "cancelEnabled": !c.Required, "maxLength": 256, "suggestions": []any{}})
			return
		}
		opts := []map[string]any{}
		for i, it := range c.Items {
			opts = append(opts, map[string]any{"id": i, "label": plain(it.Text)})
		}
		s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "choices", "title": title,
			"message": plain(c.SubMessage), "min": 1, "max": 1, "cancelEnabled": !c.Required, "options": opts,
			"revealedCards": []any{}, "revealTitle": "", "selectedOptionIds": []int{}})
	case "pile":
		names := func(cs []Card) string {
			n := []string{}
			for _, c := range cs {
				n = append(n, c.Name)
			}
			if len(n) == 0 {
				return L.t("（空）", "(empty)")
			}
			return strings.Join(n, L.t("、", ", "))
		}
		opts := []map[string]any{
			{"id": 0, "label": L.t("第一堆：", "Pile 1: ") + names(p.Cards1)},
			{"id": 1, "label": L.t("第二堆：", "Pile 2: ") + names(p.Cards2)},
		}
		s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "options", "title": msg, "message": "",
			"min": 1, "max": 1, "cancelEnabled": false, "options": opts, "revealedCards": []any{}, "revealTitle": "", "selectedOptionIds": []int{}})
	case "amount":
		s.h.Send(map[string]any{"type": "number_request", "promptId": s.pid, "title": msg, "message": "",
			"min": p.Min, "max": p.Max, "value": p.Min, "cancelEnabled": false, "allowedValues": []int{}})
	case "multi":
		targets := []map[string]any{}
		for i, a := range p.Multi {
			targets = append(targets, map[string]any{"id": i, "label": plain(a.Message), "kind": "card", "entityId": nil,
				"min": a.Min, "max": a.Max, "amount": a.Default, "lethal": 0, "requiresPreviousLethal": false})
		}
		title := firstNonEmpty(plain(p.optionText("title")), plain(p.optionText("header")), msg)
		_, canCancel := p.Options["canCancel"]
		s.h.Send(map[string]any{"type": "allocation_request", "promptId": s.pid, "kind": "generic", "title": title,
			"message": plain(p.optionText("header")), "total": p.Max, "allowSkip": canCancel, "targets": targets})
	default:
		s.degraded()
	}
}

func (s *Session) degraded() {
	s.h.Degraded(s.cur.Method, "没有对照")
	L := s.lab
	s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "options", "title": firstNonEmpty(plain(s.cur.Message), s.cur.Method),
		"message": L.t("这一步独弈还不会显示，只能确定或取消", "Tablenet can't show this step yet; confirm or cancel"),
		"min": 1, "max": 1, "cancelEnabled": false,
		"options":       []map[string]any{{"id": 0, "label": L.t("确定", "OK")}, {"id": 1, "label": L.t("取消", "Cancel")}},
		"revealedCards": []any{}, "revealTitle": "", "selectedOptionIds": []int{}})
}

func firstNonEmpty(a ...string) string {
	for _, x := range a {
		if x != "" {
			return x
		}
	}
	return ""
}

// ---- 指令 ----

type Command struct {
	Type        string `json:"type"`
	PromptID    int64  `json:"promptId"`
	Command     string `json:"command"`
	EntityID    *int   `json:"entityId"`
	TargetID    *int   `json:"targetId"`
	Value       *int   `json:"value"`
	Text        string `json:"text"`
	OrderedIDs  []int  `json:"orderedIds"`
	Allocations []struct {
		ID     int `json:"id"`
		Amount int `json:"amount"`
	} `json:"allocations"`
}

func (s *Session) reject(why string) {
	s.h.Send(map[string]any{"type": "error", "code": "rejected", "message": why})
	s.emit()
}

// answer 把一个回答交给 XMage，当前提问随之作废（XMage 会再发新的）。
func (s *Session) answer(op string, v any) {
	s.cur = nil
	if err := s.h.Answer(op, v); err != nil {
		s.h.Send(map[string]any{"type": "error", "code": "remote_error", "message": err.Error()})
	}
}

// OnCommand 处理独弈客人发来的一条指令。
func (s *Session) OnCommand(c Command) {
	switch c.Command {
	case "concede":
		_ = s.h.Answer("action", "CONCEDE")
		return
	case "undo":
		_ = s.h.Answer("action", "UNDO")
		return
	}
	if s.cur == nil {
		return // 不是在等这位玩家回答：让过类指令丢掉
	}
	if c.PromptID != 0 && c.PromptID != s.pid {
		s.h.Send(map[string]any{"type": "error", "code": "stale_prompt", "message": "提问已经换了",
			"receivedPromptId": c.PromptID, "expectedPromptId": s.pid})
		return
	}
	p := s.cur
	ent := -1
	if c.EntityID != nil {
		ent = *c.EntityID
	}
	entStr, _ := s.ids.back(ent)
	L := s.lab
	switch s.kind {
	case "priority":
		switch c.Command {
		case "pass", "ok":
			s.answer("bool", false)
		case "select_card":
			if _, ok := s.view.Playable[entStr]; !ok {
				// XMage 也允许点不在可用名单里的牌（比如看面朝下的牌），但多半什么都不发生；先拦下说清楚。
				s.reject(L.t("这张牌现在不能用", "That card can't be used now"))
				return
			}
			s.answer("uuid", entStr)
		default:
			s.reject(L.t("现在不能这样做", "You can't do that now"))
		}
	case "mana":
		switch c.Command {
		case "select_card", "use_mana":
			if entStr == "" {
				s.reject(L.t("这张牌付不了这笔费用", "That card can't pay this cost"))
				return
			}
			s.answer("uuid", entStr)
		case "ok":
			s.autopaying, s.autopay = true, map[string]bool{}
			if !s.autopayNext() {
				s.autopaying = false
				s.reject(L.t("没有能用的法术力来源", "No mana source can pay this"))
			}
		case "cancel":
			s.autopaying = false
			s.answer("bool", false)
		}
	case "mulligan":
		switch c.Command {
		case "ok":
			s.answer("bool", false) // XMage：false＝保留
		case "cancel":
			s.answer("bool", true)
		}
	case "ask":
		switch c.Command {
		case "ok":
			s.answer("bool", true)
		case "cancel":
			s.answer("bool", false)
		}
	case "target":
		switch c.Command {
		case "select_card":
			if entStr == "" || !contains(p.Targets, entStr) {
				s.reject(L.t("不能选这个", "That can't be chosen"))
				return
			}
			s.answer("uuid", entStr)
		case "select_player":
			id, ok := s.playerOfSeat(ent)
			if !ok || !contains(p.Targets, id) {
				s.reject(L.t("不能选这位牌手", "That player can't be chosen"))
				return
			}
			s.answer("uuid", id)
		case "ok", "cancel":
			if p.Flag {
				s.reject(L.t("还得再选", "Choose more"))
				return
			}
			s.answer("bool", true)
		}
	case "attack":
		s.onAttack(c, ent, entStr)
	case "block":
		s.onBlock(c, entStr)
	case "ability":
		switch c.Command {
		case "select_option":
			if ent < 0 || ent >= len(p.Choices) {
				s.reject(L.t("没有这个选项", "No such option"))
				return
			}
			s.answer("uuid", p.Choices[ent].ID)
		case "cancel":
			s.answer("bool", false)
		}
	case "choice":
		ch := p.Choice
		switch c.Command {
		case "select_option":
			if ch == nil || ent < 0 || ent >= len(ch.Items) {
				s.reject(L.t("没有这个选项", "No such option"))
				return
			}
			s.answer("string", ch.Items[ent].Key)
		case "submit_text":
			want := strings.ToLower(strings.TrimSpace(c.Text))
			for _, it := range ch.Items {
				if strings.ToLower(it.Text) == want || strings.ToLower(it.Key) == want {
					s.answer("string", it.Key)
					return
				}
			}
			s.reject(L.t("没有叫这个名字的牌", "No card has that name"))
		case "cancel":
			if ch != nil && !ch.Required {
				s.answer("bool", false)
			}
		}
	case "pile":
		if c.Command == "select_option" && (ent == 0 || ent == 1) {
			s.answer("bool", ent == 0)
		}
	case "amount":
		if c.Command == "submit_number" && c.Value != nil {
			v := *c.Value
			if v < p.Min || v > p.Max {
				s.reject(fmt.Sprintf(L.t("要在 %d–%d 之间", "Must be between %d and %d"), p.Min, p.Max))
				return
			}
			s.answer("int", v)
		}
	case "multi":
		switch c.Command {
		case "submit_allocation":
			vals := make([]int, len(p.Multi))
			for i, a := range p.Multi {
				vals[i] = a.Default
			}
			for _, a := range c.Allocations {
				if a.ID >= 0 && a.ID < len(vals) {
					vals[a.ID] = a.Amount
				}
			}
			parts := []string{}
			for _, v := range vals {
				parts = append(parts, strconv.Itoa(v))
			}
			s.answer("string", strings.Join(parts, " "))
		case "skip_allocation", "cancel":
			if _, ok := p.Options["canCancel"]; ok {
				s.answer("bool", false)
			}
		}
	default:
		if c.Command == "select_option" {
			s.answer("bool", ent == 0)
		}
	}
}

// autopayNext：替玩家点下一个还没点过的法术力来源。没有可点的返回 false。
func (s *Session) autopayNext() bool {
	if s.view == nil {
		return false
	}
	// 先点战场上的（地、法术力生物），手上的、坟场的放后面；同一类按编号排，结果稳定。
	best, bestRank := "", 99
	for id := range s.view.Playable {
		if s.autopay[id] {
			continue
		}
		rank := 2
		for _, p := range s.view.Players {
			for _, c := range p.Battlefield {
				if c.ID == id {
					rank = 0
					if !c.Land {
						rank = 1
					}
				}
			}
		}
		if rank < bestRank || (rank == bestRank && s.ids.of(id) < s.ids.of(best)) {
			best, bestRank = id, rank
		}
	}
	if best == "" {
		return false
	}
	s.autopay[best] = true
	s.answer("uuid", best)
	return true
}

func (s *Session) onAttack(c Command, ent int, entStr string) {
	p := s.cur
	L := s.lab
	switch c.Command {
	case "select_card":
		if contains(p.optionIDs("POSSIBLE_ATTACKERS"), entStr) || s.isAttacking(entStr) {
			s.pendingDefender = s.defender
			s.answer("uuid", entStr)
			return
		}
		if s.validDefender(entStr) { // 点的是鹏洛客／战役：之后的进攻者打它
			s.defender = entStr
			s.emit()
			return
		}
		s.reject(L.t("不能进攻", "Can't attack"))
	case "select_player":
		if id, ok := s.playerOfSeat(ent); ok && s.validDefender(id) {
			s.defender = id
		}
		s.emit()
	case "alpha_strike":
		s.pendingDefender = s.defender
		s.answer("string", "special")
	case "clear_attackers":
		// XMage 再点一次已进攻的生物就是撤回：排好一只只撤
		atk := []string{}
		for _, g := range s.view.Combat {
			atk = append(atk, g.Attackers...)
		}
		if len(atk) == 0 {
			s.emit()
			return
		}
		for _, a := range atk[1:] {
			s.queue = append(s.queue, queued{kind: "attack", op: "uuid", v: a})
		}
		s.answer("uuid", atk[0])
	case "ok", "pass":
		s.answer("bool", true)
	}
}

func (s *Session) isAttacking(id string) bool {
	for _, g := range s.view.Combat {
		if contains(g.Attackers, id) {
			return true
		}
	}
	return false
}

// blocking：这个阻挡者现在挡的是哪个进攻者（没挡返回空）。
func (s *Session) blocking(blocker string) string {
	for _, g := range s.view.Combat {
		if contains(g.Blockers, blocker) && len(g.Attackers) > 0 {
			return g.Attackers[0]
		}
	}
	return ""
}

func (s *Session) onBlock(c Command, entStr string) {
	p := s.cur
	L := s.lab
	switch c.Command {
	case "assign_block":
		if c.TargetID == nil {
			return
		}
		att, _ := s.ids.back(*c.TargetID)
		if !contains(p.optionIDs("POSSIBLE_BLOCKERS"), entStr) || !s.isAttacking(att) {
			s.reject(L.t("挡不了这个进攻者", "Can't block that attacker"))
			return
		}
		now := s.blocking(entStr)
		switch {
		case now == att: // 再点同一对＝撤回
			s.answer("uuid", entStr)
		case now != "": // 改挡：先撤再挡
			s.queue = append(s.queue, queued{kind: "block", op: "uuid", v: entStr})
			s.pendingBlock = att
			s.answer("uuid", entStr)
		default:
			s.pendingBlock = att
			s.answer("uuid", entStr)
		}
	case "select_card": // 点阻挡者本身＝撤回它的阻挡
		if s.blocking(entStr) != "" {
			s.answer("uuid", entStr)
			return
		}
		s.emit()
	case "block_all":
		s.emit() // XMage 没有「全部阻挡」，不替玩家乱挡
	case "ok", "pass":
		s.answer("bool", true)
	}
}
