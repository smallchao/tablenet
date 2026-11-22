// SPDX-License-Identifier: GPL-3.0-or-later

// Package bridge 在对局通道上冒充独弈主机：把 Hexproof 规则房的局面和提问翻成 arcane.v0，
// 把独弈客人前端发来的指令翻回 rules.respond。
//
// 独弈的客人前端是瘦客户端：每点一下发一条指令，由主机记住选了什么、再发一帧更新后的提问。
// Hexproof 的提问则是一次交一整组。所以这里替每个提问记住当前的选择，玩家按确定时才整组交给 Hexproof。
// 对照表见 spec/08-桥接模式.md#hexproof-规则房。
package bridge

import (
	"fmt"
	"strings"

	"tablenet-adapter-hexproof/hub"
)

// Hooks 是会话对外的三个出口：发一帧给客人、交一个回答给 Hexproof、认输。
type Hooks struct {
	Send    func(frame map[string]any)
	Respond func(r hub.Respond) error
	Concede func() error
	// Degraded 在提问退化成通用选项时调用，适配器记日志用。
	Degraded func(kind, why string)
}

type Session struct {
	seat int
	lab  labels
	ids  *ids
	h    Hooks

	snap        *hub.Snapshot
	cur         *hub.Prompt
	pid         int64 // 当前发给客人的提问编号
	promptCards []map[string]any

	// 当前提问里玩家已经点了的东西
	picked    []string          // 牌、目标、选项、牌名：Hexproof 的 id／responseId
	attack    map[string]string // 进攻者 responseId → 防御对象 responseId
	attackOrd []string
	defender  string      // 下一只进攻者默认打谁（防御对象 responseId）
	blocks    [][2]string // (阻挡者 responseId, 进攻者 responseId)
	abilityOf string      // chooseAction 里点了一张有多种动作的牌，正在选动作
}

func NewSession(seat int, locale string, h Hooks) *Session {
	if h.Degraded == nil {
		h.Degraded = func(string, string) {}
	}
	return &Session{seat: seat, lab: labels{zh: strings.HasPrefix(locale, "zh")}, ids: newIDs(), h: h}
}

// OnSnapshot：Hexproof 发来新局面。
func (s *Session) OnSnapshot(snap hub.Snapshot) {
	s.snap = &snap
	s.h.Send(s.stateJSON(s.snap))
	if snap.GameOver {
		winner := any(nil)
		if snap.WinnerSeat != nil {
			for _, p := range snap.Players {
				if p.Seat == *snap.WinnerSeat {
					winner = p.Name
				}
			}
		}
		s.h.Send(map[string]any{"type": "game_over", "winner": winner, "gameNumber": 1, "matchOver": false,
			"winCondition": "AllOpponentsLost", "winSpellEffect": nil, "outcomes": []string{}})
	}
}

// OnPrompt：Hexproof 问这位玩家一个问题。
func (s *Session) OnPrompt(p hub.Prompt) {
	if !p.Pending {
		return
	}
	s.cur = &p
	s.pid++
	s.picked, s.attack, s.attackOrd, s.blocks, s.abilityOf = nil, map[string]string{}, nil, nil, ""
	s.defender = ""
	if p.Kind == "chooseAttackers" && len(p.CombatTargets) > 0 {
		s.defender = p.CombatTargets[0].ResponseID
	}
	for _, c := range p.Cards {
		if c.Selected {
			s.picked = append(s.picked, c.ID)
		}
	}
	// 选牌、展示时这些牌未必在场上：放进 state 的 revealedCards，界面才画得出来。
	s.promptCards = nil
	if p.Kind == "chooseCards" || p.Kind == "mulliganPutBack" || p.Kind == "revealCards" || p.Kind == "scry" {
		for _, c := range p.Cards {
			s.promptCards = append(s.promptCards, s.promptCard(c))
		}
		if s.snap != nil {
			s.h.Send(s.stateJSON(s.snap))
		}
	}
	switch p.Kind {
	case "acknowledge", "diceRolled":
		// 不是提问：显示一条信息，替玩家回「已看」。
		s.h.Send(map[string]any{"type": "message", "title": p.Title, "message": firstNonEmpty(p.Detail, p.Title)})
		s.submit(hub.Respond{ResponseID: "$ack"})
		return
	case "chooseCards", "mulliganPutBack", "chooseBoardTargets":
		// 只能选一个、而且只有一个可选：照样发问，让玩家自己点（不替玩家决定）。
	}
	s.emit()
}

func firstNonEmpty(a ...string) string {
	for _, x := range a {
		if x != "" {
			return x
		}
	}
	return ""
}

func (s *Session) promptCard(c hub.PromptCard) map[string]any {
	return map[string]any{"id": s.ids.of(c.ID), "name": c.Name, "zhName": "", "mana": "", "type": "",
		"setCode": c.SetCode, "zone": "Reveal", "token": c.Token}
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
	if s.snap != nil {
		for _, p := range s.snap.Players {
			if p.Seat == s.seat {
				return p.Name
			}
		}
	}
	return nil
}

func (s *Session) targetSource() any {
	if s.cur == nil || len(s.cur.ContextCards) == 0 {
		return nil
	}
	c := s.cur.ContextCards[0]
	return map[string]any{"cardId": s.ids.of(c.ID), "name": c.Name, "zhName": "", "text": s.cur.ContextText}
}

func (s *Session) message() string {
	p := s.cur
	if p.Detail != "" && p.Title != "" && p.Detail != p.Title {
		return p.Title + "：" + p.Detail
	}
	return firstNonEmpty(p.Title, p.Detail)
}

// emit 按当前提问和已选的东西发一组帧：prompt、具体的提问帧、buttons。
func (s *Session) emit() {
	p := s.cur
	if p == nil {
		return
	}
	L := s.lab
	if !p.Supported {
		s.h.Degraded(p.Kind, "Hexproof 自己也答不了")
		s.h.Send(s.promptFrame(L.t("对方平台不支持这一步（"+p.Kind+"），只能等超时或认输", "The other platform can't handle this step ("+p.Kind+"); wait for the timeout or concede")))
		s.h.Send(s.base("locked"))
		s.h.Send(s.buttons("", "", false, false))
		return
	}
	switch p.Kind {
	case "chooseAction":
		if s.abilityOf != "" {
			s.emitAbilityPick()
			return
		}
		m := s.base("action")
		legal, actions := []int{}, []map[string]any{}
		seen := map[int]bool{}
		for _, o := range p.Options {
			if o.CardID == "" || o.Kind == "pass" {
				continue
			}
			id := s.ids.of(o.CardID)
			actions = append(actions, map[string]any{"cardId": id, "label": o.Label})
			if !seen[id] {
				seen[id] = true
				legal = append(legal, id)
			}
		}
		m["legalCardIds"], m["playableCardIds"], m["cardActions"] = legal, legal, actions
		m["hasAvailableActions"] = len(legal) > 0
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("让过", "Pass"), "", true, false))
	case "payManaCost":
		m := s.base("action")
		legal := []int{}
		for _, o := range p.Options {
			if o.CardID != "" {
				legal = append(legal, s.ids.of(o.CardID))
			}
		}
		m["legalCardIds"] = legal
		m["manaPayment"] = map[string]any{"payerPlayerId": s.seat, "remainingCost": strings.TrimPrefix(p.Title, "Pay Mana Cost: "),
			"lifeAlreadyChosen": 0, "canPayLife": false, "sourceCardId": nil, "sourceName": p.Detail, "sourceZhName": "",
			"duringResolution": false, "autoTapCardIds": []int{}}
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("自动支付", "Auto-pay"), L.t("取消", "Cancel"), s.hasOption("$auto-pay") || s.hasOption("$pay"), s.hasOption("$cancel")))
	case "mulligan":
		m := s.base("mulligan")
		m["mulliganMode"] = "keep_or_mulligan"
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("保留", "Keep"), L.t("调度", "Mulligan"), true, true))
	case "mulliganPutBack":
		m := s.base("mulligan")
		m["mulliganMode"], m["cardsToBottom"] = "bottom_cards", p.RequiredSelections
		m["legalCardIds"], m["selectedCardIds"] = s.cardIDs(p.Cards), s.intsOf(s.picked)
		m["min"], m["max"] = p.RequiredSelections, p.RequiredSelections
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("确定", "OK"), "", len(s.picked) == p.RequiredSelections, false))
	case "chooseCards":
		m := s.base("select")
		m["legalCardIds"], m["selectedCardIds"] = s.cardIDs(p.Cards), s.intsOf(s.picked)
		m["min"], m["max"], m["pickFromList"] = p.MinCardSelections, p.MaxCardSelections, true
		m["targetSource"] = s.targetSource()
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("确定", "OK"), L.t("取消", "Cancel"), len(s.picked) >= p.MinCardSelections, p.Cancellable))
	case "chooseBoardTargets":
		m := s.base("select")
		cards, players := []int{}, []int{}
		selCards, selPlayers := []int{}, []int{}
		for _, t := range p.Targets {
			chosen := contains(s.picked, t.ResponseID)
			if t.Seat != nil && t.ObjectID == "" {
				players = append(players, *t.Seat)
				if chosen {
					selPlayers = append(selPlayers, *t.Seat)
				}
			} else {
				id := s.ids.of(t.ObjectID)
				cards = append(cards, id)
				if chosen {
					selCards = append(selCards, id)
				}
			}
		}
		m["legalCardIds"], m["candidatePlayerIds"], m["selectedCardIds"], m["selectedPlayerIds"] = cards, players, selCards, selPlayers
		m["min"], m["max"], m["targetSource"] = p.MinSelections, p.MaxSelections, s.targetSource()
		m["declinable"] = p.Cancellable
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("确定", "OK"), L.t("取消", "Cancel"), len(s.picked) >= p.MinSelections, p.Cancellable))
	case "chooseAttackers":
		m := s.base("attack")
		cands, declared, pw := []int{}, []int{}, []int{}
		for _, src := range p.CombatSources {
			cands = append(cands, s.ids.of(src.ObjectID))
		}
		for _, a := range s.attackOrd {
			if src := s.combatSource(a); src != nil {
				declared = append(declared, s.ids.of(src.ObjectID))
			}
		}
		players := []int{}
		for _, t := range p.CombatTargets {
			if t.Seat != nil && t.ObjectID == "" {
				players = append(players, *t.Seat)
			} else if t.ObjectID != "" {
				pw = append(pw, s.ids.of(t.ObjectID))
			}
		}
		m["attackerCandidateIds"], m["legalCardIds"], m["declaredAttackerIds"] = cands, cands, declared
		m["attackDefenderCardIds"], m["candidatePlayerIds"] = pw, players
		if t := s.combatTarget(s.defender); t != nil {
			if t.Seat != nil && t.ObjectID == "" {
				m["currentDefenderPlayerId"] = *t.Seat
			} else {
				m["currentDefenderCardId"] = s.ids.of(t.ObjectID)
			}
		}
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("确定", "OK"), "", true, false))
	case "chooseBlockers":
		m := s.base("block")
		cands := []map[string]any{}
		for _, src := range p.CombatSources {
			as := []int{}
			for _, tid := range src.ValidTargetIDs {
				if t := s.combatTarget(tid); t != nil {
					as = append(as, s.ids.of(t.ObjectID))
				}
			}
			if len(as) > 0 {
				cands = append(cands, map[string]any{"blockerId": s.ids.of(src.ObjectID), "attackerIds": as})
			}
		}
		m["blockerCandidates"] = cands
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(L.t("确定", "OK"), "", true, false))
	case "chooseBoolean":
		m := s.base("confirm")
		ok, no := L.t("是", "Yes"), L.t("否", "No")
		if len(p.Choices) >= 2 {
			ok, no = p.Choices[0].Label, p.Choices[1].Label
		}
		m["targetSource"] = s.targetSource()
		s.h.Send(s.promptFrame(s.message()))
		s.h.Send(m)
		s.h.Send(s.buttons(ok, no, true, len(p.Choices) >= 2))
	case "chooseColor", "chooseFromSelection":
		opts, selected := []map[string]any{}, []int{}
		for i, c := range p.Choices {
			opts = append(opts, map[string]any{"id": i, "label": c.Label})
			if contains(s.picked, c.ResponseID) {
				selected = append(selected, i)
			}
		}
		s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "choices", "title": p.Title,
			"message": p.Detail, "min": p.MinChoiceTotal, "max": p.MaxChoiceTotal, "cancelEnabled": false,
			"okEnabled": len(s.picked) >= p.MinChoiceTotal, "options": opts, "revealedCards": []any{}, "revealTitle": "",
			"selectedOptionIds": selected})
	case "chooseNumber":
		s.h.Send(map[string]any{"type": "number_request", "promptId": s.pid, "title": p.Title, "message": p.Detail,
			"min": p.MinNumber, "max": p.MaxNumber, "value": p.MinNumber, "cancelEnabled": false, "allowedValues": []int{}})
	case "chooseCardName":
		s.h.Send(map[string]any{"type": "text_request", "promptId": s.pid, "title": p.Title, "message": p.Detail,
			"text": "", "cancelEnabled": p.Cancellable, "maxLength": 256, "suggestions": []any{}})
	case "reorder", "chooseDamageAssignmentOrder":
		items := []map[string]any{}
		if p.Kind == "reorder" {
			for _, it := range p.OrderItems {
				items = append(items, map[string]any{"id": s.ids.of("order:" + it.ResponseID), "label": it.Name, "card": nil})
			}
		} else {
			for _, t := range p.DamageTargets {
				items = append(items, map[string]any{"id": s.ids.of("order:" + t.ResponseID), "label": t.Label, "card": nil})
			}
		}
		s.h.Send(map[string]any{"type": "order_request", "promptId": s.pid, "title": p.Title, "top": L.t("先", "First"),
			"remainingMin": 0, "remainingMax": 0, "rememberVisible": false, "sourceItems": items, "orderedItems": []any{}})
	case "chooseCombatDamageAssignment":
		targets := []map[string]any{}
		for _, t := range p.DamageTargets {
			kind := "card"
			if t.Kind == "player" {
				kind = "player"
			}
			var entity any
			if t.ObjectID != "" {
				entity = s.ids.of(t.ObjectID)
			}
			targets = append(targets, map[string]any{"id": s.ids.of("dmg:" + t.ResponseID), "label": t.Label, "kind": kind,
				"entityId": entity, "min": 0, "max": p.TotalDamage, "amount": 0, "lethal": t.LethalDamage,
				"requiresPreviousLethal": p.DamageAssignmentMode == "ordered"})
		}
		s.h.Send(map[string]any{"type": "allocation_request", "promptId": s.pid, "kind": "combat", "title": p.Title,
			"message": p.Detail, "total": p.TotalDamage, "allowSkip": false, "targets": targets})
	case "revealCards":
		items := []map[string]any{}
		for i, c := range p.Cards {
			items = append(items, map[string]any{"id": i, "label": c.Name, "card": s.promptCard(c), "cardName": c.Name})
		}
		s.h.Send(map[string]any{"type": "reveal_request", "promptId": s.pid, "title": p.Title, "items": items})
	case "scry":
		// 独弈的牌库面板只认「顶／底」两处；Hexproof 的去处可能更多（坟场、放逐、手牌），这里退成逐张选去处。
		s.emitScry()
	default:
		s.degradedOptions()
	}
}

func (s *Session) hasOption(id string) bool {
	for _, o := range s.cur.Options {
		if o.ResponseID == id {
			return true
		}
	}
	return false
}

func (s *Session) cardIDs(cards []hub.PromptCard) []int {
	out := []int{}
	for _, c := range cards {
		if !c.ReadOnly {
			out = append(out, s.ids.of(c.ID))
		}
	}
	return out
}

func (s *Session) intsOf(xs []string) []int {
	out := []int{}
	for _, x := range xs {
		out = append(out, s.ids.of(x))
	}
	return out
}

func (s *Session) combatSource(resp string) *hub.CombatSource {
	for i := range s.cur.CombatSources {
		if s.cur.CombatSources[i].ResponseID == resp {
			return &s.cur.CombatSources[i]
		}
	}
	return nil
}

func (s *Session) combatTarget(resp string) *hub.CombatTarget {
	for i := range s.cur.CombatTargets {
		if s.cur.CombatTargets[i].ResponseID == resp {
			return &s.cur.CombatTargets[i]
		}
	}
	return nil
}

// emitAbilityPick：一张牌在 chooseAction 里有不止一种动作（施放、起动不同异能），让玩家挑。
func (s *Session) emitAbilityPick() {
	opts := []map[string]any{}
	for i, o := range s.cur.Options {
		if o.CardID == s.abilityOf {
			opts = append(opts, map[string]any{"id": i, "label": o.Label})
		}
	}
	s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "ability", "title": s.lab.t("选一种用法", "Choose an action"),
		"message": "", "min": 1, "max": 1, "cancelEnabled": true, "okEnabled": false, "options": opts,
		"revealedCards": []any{}, "revealTitle": "", "selectedOptionIds": []int{}})
}

// emitScry：每张牌一个选项组，「牌名 → 去处」。
func (s *Session) emitScry() {
	opts := []map[string]any{}
	n := 0
	for _, c := range s.cur.Cards {
		for _, d := range s.cur.ScryDestinations {
			opts = append(opts, map[string]any{"id": n, "label": c.Name + " → " + s.destLabel(d), "group": c.Name})
			n++
		}
	}
	s.h.Degraded("scry", "退成逐张选去处")
	s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "options", "title": s.cur.Title,
		"message": s.lab.t("每张牌选一个去处，最上面的先放", "Choose a destination for each card"), "min": len(s.cur.Cards),
		"max": len(s.cur.Cards), "cancelEnabled": false, "okEnabled": len(s.picked) == len(s.cur.Cards), "options": opts,
		"revealedCards": []any{}, "revealTitle": "", "selectedOptionIds": s.scrySelected()})
}

func (s *Session) destLabel(d string) string {
	switch d {
	case "libraryTop":
		return s.lab.t("牌库顶", "top of library")
	case "libraryBottom":
		return s.lab.t("牌库底", "bottom of library")
	case "graveyard":
		return s.lab.t("坟场", "graveyard")
	case "exile":
		return s.lab.t("放逐", "exile")
	case "hand":
		return s.lab.t("手牌", "hand")
	}
	return d
}

func (s *Session) scrySelected() []int {
	out := []int{}
	nd := len(s.cur.ScryDestinations)
	for i, c := range s.cur.Cards {
		for j, d := range s.cur.ScryDestinations {
			if contains(s.picked, c.ID+"→"+d) {
				out = append(out, i*nd+j)
			}
		}
	}
	return out
}

// degradedOptions：映射不了的提问，把 Hexproof 给的选项原样列出来，宁可难看也不卡死。
func (s *Session) degradedOptions() {
	p := s.cur
	s.h.Degraded(p.Kind, "没有对照，退成通用选项")
	opts := []map[string]any{}
	for i, o := range p.Options {
		opts = append(opts, map[string]any{"id": i, "label": o.Label})
	}
	s.h.Send(map[string]any{"type": "modal_request", "promptId": s.pid, "kind": "options", "title": firstNonEmpty(p.Title, p.Kind),
		"message": p.Detail, "min": 1, "max": 1, "cancelEnabled": false, "okEnabled": false, "options": opts,
		"revealedCards": []any{}, "revealTitle": "", "selectedOptionIds": []int{}})
}

// ---- 收指令 ----

// Command 是独弈客人前端发来的一条 command（command-v0）。
type Command struct {
	Type         string `json:"type"`
	PromptID     int64  `json:"promptId"`
	Command      string `json:"command"`
	EntityID     *int   `json:"entityId"`
	TargetID     *int   `json:"targetId"`
	Value        *int   `json:"value"`
	Text         string `json:"text"`
	OrderedIDs   []int  `json:"orderedIds"`
	RemainingIDs []int  `json:"remainingIds"`
	Allocations  []struct {
		ID     int `json:"id"`
		Amount int `json:"amount"`
	} `json:"allocations"`
}

func (s *Session) reject(why string) {
	s.h.Send(map[string]any{"type": "error", "code": "rejected", "message": why})
	s.emit()
}

// OnCommand 处理一条指令。
func (s *Session) OnCommand(c Command) {
	if c.Command == "concede" {
		_ = s.h.Concede()
		return
	}
	if s.cur == nil {
		return // 不是我的回合要我答的东西：让过类指令丢掉
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
	switch p.Kind {
	case "chooseAction":
		if s.abilityOf != "" {
			switch c.Command {
			case "select_option":
				if ent >= 0 && ent < len(p.Options) && p.Options[ent].CardID == s.abilityOf {
					s.submit(hub.Respond{ResponseID: p.Options[ent].ResponseID})
					return
				}
			case "cancel":
				s.abilityOf = ""
				s.emit()
				return
			}
			s.reject(L.t("先选一种用法", "Choose an action first"))
			return
		}
		switch c.Command {
		case "pass", "ok":
			s.submit(hub.Respond{ResponseID: "$pass"})
		case "select_card":
			var mine []hub.PromptOption
			for _, o := range p.Options {
				if o.CardID != "" && o.CardID == entStr {
					mine = append(mine, o)
				}
			}
			switch len(mine) {
			case 0:
				s.reject(L.t("这张牌现在不能用", "That card can't be used now"))
			case 1:
				s.submit(hub.Respond{ResponseID: mine[0].ResponseID})
			default:
				s.abilityOf = entStr
				s.pid++
				s.emit()
			}
		default:
			s.reject(L.t("现在不能这样做", "You can't do that now"))
		}
	case "payManaCost":
		switch c.Command {
		case "select_card", "use_mana":
			for _, o := range p.Options {
				if o.CardID != "" && o.CardID == entStr {
					s.submit(hub.Respond{ResponseID: o.ResponseID})
					return
				}
			}
			s.reject(L.t("这张牌付不了这笔费用", "That card can't pay this cost"))
		case "ok":
			// 只发 Hexproof 这一问里真提供了的选项：付不起时它不给 $auto-pay，硬发会被拒（2026-11 真 hub 实测）。
			switch {
			case s.hasOption("$pay"):
				s.submit(hub.Respond{ResponseID: "$pay"})
			case s.hasOption("$auto-pay"):
				s.submit(hub.Respond{ResponseID: "$auto-pay"})
			default:
				s.reject(L.t("现在的法术力付不起这笔费用：点法术力来源，或者取消", "Can't pay this cost automatically: tap a mana source or cancel"))
			}
		case "cancel":
			s.submit(hub.Respond{ResponseID: "$cancel"})
		}
	case "mulligan":
		switch c.Command {
		case "ok":
			s.submit(hub.Respond{ResponseID: "$keep"})
		case "cancel":
			s.submit(hub.Respond{ResponseID: "$mulligan"})
		}
	case "mulliganPutBack":
		switch c.Command {
		case "select_card":
			if s.isPromptCard(entStr) {
				s.picked = toggle(s.picked, entStr, p.RequiredSelections)
			}
			s.emit()
		case "ok":
			if len(s.picked) != p.RequiredSelections {
				s.reject(L.t("张数不对", "Wrong number of cards"))
				return
			}
			s.submit(hub.Respond{ResponseID: "$submit", CardIDs: s.picked})
		}
	case "chooseCards":
		switch c.Command {
		case "select_card":
			if !s.isPromptCard(entStr) {
				s.reject(L.t("不能选这张", "This card can't be chosen"))
				return
			}
			s.picked = toggle(s.picked, entStr, p.MaxCardSelections)
			// 选满上限即交（和独弈主机一样：界面把凑满的那一下压在本地，发来就是要交了）。
			if len(s.picked) == p.MaxCardSelections && len(s.picked) >= p.MinCardSelections {
				s.submit(hub.Respond{ResponseID: "$submit", CardIDs: s.picked})
				return
			}
			s.emit()
		case "ok":
			if len(s.picked) < p.MinCardSelections {
				s.reject(L.t("选得不够", "Choose more"))
				return
			}
			s.submit(hub.Respond{ResponseID: "$submit", CardIDs: s.picked})
		case "cancel":
			if p.Cancellable {
				s.submit(hub.Respond{ResponseID: "$cancel"})
			}
		}
	case "chooseBoardTargets":
		switch c.Command {
		case "select_card", "select_player":
			resp := s.targetResp(c.Command == "select_player", ent, entStr)
			if resp == "" {
				s.reject(L.t("不是可选的目标", "Not a legal target"))
				return
			}
			s.picked = toggle(s.picked, resp, p.MaxSelections)
			if p.MaxSelections == 1 && len(s.picked) == 1 {
				s.submit(hub.Respond{ResponseID: "$submit", TargetIDs: s.picked})
				return
			}
			s.emit()
		case "ok":
			if len(s.picked) < p.MinSelections {
				s.reject(L.t("目标不够", "Choose more targets"))
				return
			}
			s.submit(hub.Respond{ResponseID: "$submit", TargetIDs: s.picked})
		case "cancel":
			if p.Cancellable {
				s.submit(hub.Respond{ResponseID: "$cancel"})
			}
		}
	case "chooseAttackers":
		s.onAttack(c, ent, entStr)
	case "chooseBlockers":
		s.onBlock(c, ent, entStr)
	case "chooseBoolean":
		if len(p.Choices) == 0 {
			return
		}
		switch c.Command {
		case "ok":
			s.submit(hub.Respond{ResponseID: "$submit", ChoiceIDs: []string{p.Choices[0].ResponseID}})
		case "cancel":
			if len(p.Choices) >= 2 {
				s.submit(hub.Respond{ResponseID: "$submit", ChoiceIDs: []string{p.Choices[1].ResponseID}})
			}
		}
	case "chooseColor", "chooseFromSelection":
		switch c.Command {
		case "select_option":
			if ent < 0 || ent >= len(p.Choices) {
				s.reject(L.t("没有这个选项", "No such option"))
				return
			}
			s.picked = toggle(s.picked, p.Choices[ent].ResponseID, p.MaxChoiceTotal)
			if p.MaxChoiceTotal == 1 && p.MinChoiceTotal == 1 && len(s.picked) == 1 {
				s.submit(hub.Respond{ResponseID: "$submit", ChoiceIDs: s.picked})
				return
			}
			s.emit()
		case "ok":
			if len(s.picked) < p.MinChoiceTotal {
				s.reject(L.t("选得不够", "Choose more"))
				return
			}
			s.submit(hub.Respond{ResponseID: "$submit", ChoiceIDs: s.picked})
		}
	case "chooseNumber":
		if c.Command == "submit_number" && c.Value != nil {
			v := *c.Value
			if v < p.MinNumber || v > p.MaxNumber {
				s.reject(fmt.Sprintf(L.t("要在 %d–%d 之间", "Must be between %d and %d"), p.MinNumber, p.MaxNumber))
				return
			}
			s.submit(hub.Respond{ResponseID: "$submit", ChosenNumber: &v})
		}
	case "chooseCardName":
		switch c.Command {
		case "submit_text":
			if strings.TrimSpace(c.Text) != "" {
				s.submit(hub.Respond{ResponseID: "$submit", Name: strings.TrimSpace(c.Text)})
			}
		case "cancel":
			if p.Cancellable {
				s.submit(hub.Respond{ResponseID: "$cancel"})
			}
		}
	case "reorder", "chooseDamageAssignmentOrder":
		if c.Command != "submit_order" {
			return
		}
		order := []string{}
		for _, n := range c.OrderedIDs {
			if str, ok := s.ids.back(n); ok && strings.HasPrefix(str, "order:") {
				order = append(order, strings.TrimPrefix(str, "order:"))
			}
		}
		if p.Kind == "reorder" {
			s.submit(hub.Respond{ResponseID: "$submit", OrderedIDs: order})
		} else {
			s.submit(hub.Respond{ResponseID: "$submit", DamageOrderIDs: order})
		}
	case "chooseCombatDamageAssignment":
		if c.Command != "submit_allocation" {
			return
		}
		out := []hub.DamageAssignment{}
		total := 0
		for _, a := range c.Allocations {
			str, ok := s.ids.back(a.ID)
			if !ok || !strings.HasPrefix(str, "dmg:") || a.Amount < 0 {
				s.reject(L.t("分配对象不对", "Invalid allocation"))
				return
			}
			total += a.Amount
			if a.Amount > 0 {
				out = append(out, hub.DamageAssignment{TargetID: strings.TrimPrefix(str, "dmg:"), Damage: a.Amount})
			}
		}
		if total != p.TotalDamage {
			s.reject(fmt.Sprintf(L.t("要正好分配 %d 点", "Assign exactly %d damage"), p.TotalDamage))
			return
		}
		s.submit(hub.Respond{ResponseID: "$submit", DamageAssignments: out})
	case "revealCards":
		if c.Command == "close_reveal" || c.Command == "ok" {
			s.submit(hub.Respond{ResponseID: "$ack"})
		}
	case "scry":
		s.onScry(c, ent)
	default:
		if c.Command == "select_option" && ent >= 0 && ent < len(p.Options) {
			s.submit(hub.Respond{ResponseID: p.Options[ent].ResponseID})
		}
	}
}

func (s *Session) isPromptCard(id string) bool {
	for _, c := range s.cur.Cards {
		if c.ID == id && !c.ReadOnly {
			return true
		}
	}
	return false
}

func (s *Session) targetResp(player bool, ent int, entStr string) string {
	for _, t := range s.cur.Targets {
		if player && t.Seat != nil && t.ObjectID == "" && *t.Seat == ent {
			return t.ResponseID
		}
		if !player && t.ObjectID != "" && t.ObjectID == entStr {
			return t.ResponseID
		}
	}
	return ""
}

func (s *Session) onAttack(c Command, ent int, entStr string) {
	p := s.cur
	switch c.Command {
	case "select_card":
		for _, src := range p.CombatSources {
			if src.ObjectID == entStr {
				if _, on := s.attack[src.ResponseID]; on {
					delete(s.attack, src.ResponseID)
					s.attackOrd = remove(s.attackOrd, src.ResponseID)
				} else {
					s.attack[src.ResponseID] = s.pickDefender(src)
					s.attackOrd = append(s.attackOrd, src.ResponseID)
				}
				s.emit()
				return
			}
		}
		for _, t := range p.CombatTargets { // 点的是鹏洛客／战役：之后的进攻者打它
			if t.ObjectID != "" && t.ObjectID == entStr {
				s.defender = t.ResponseID
				s.emit()
				return
			}
		}
		s.reject(s.lab.t("不能进攻", "Can't attack"))
	case "select_player":
		for _, t := range p.CombatTargets {
			if t.Seat != nil && t.ObjectID == "" && *t.Seat == ent {
				s.defender = t.ResponseID
			}
		}
		s.emit()
	case "alpha_strike":
		for _, src := range p.CombatSources {
			if _, on := s.attack[src.ResponseID]; !on {
				s.attack[src.ResponseID] = s.pickDefender(src)
				s.attackOrd = append(s.attackOrd, src.ResponseID)
			}
		}
		s.emit()
	case "clear_attackers":
		s.attack, s.attackOrd = map[string]string{}, nil
		s.emit()
	case "ok":
		as := []hub.Assignment{}
		for _, a := range s.attackOrd {
			as = append(as, hub.Assignment{SourceID: a, TargetID: s.attack[a]})
		}
		s.submit(hub.Respond{ResponseID: "$submit", Assignments: as})
	}
}

// pickDefender：默认打当前选定的防御对象；这只生物打不了它就打它能打的第一个。
func (s *Session) pickDefender(src hub.CombatSource) string {
	if contains(src.ValidTargetIDs, s.defender) {
		return s.defender
	}
	if len(src.ValidTargetIDs) > 0 {
		return src.ValidTargetIDs[0]
	}
	return s.defender
}

func (s *Session) onBlock(c Command, ent int, entStr string) {
	p := s.cur
	switch c.Command {
	case "assign_block":
		if c.TargetID == nil {
			return
		}
		attStr, _ := s.ids.back(*c.TargetID)
		var src *hub.CombatSource
		for i := range p.CombatSources {
			if p.CombatSources[i].ObjectID == entStr {
				src = &p.CombatSources[i]
			}
		}
		var tgt *hub.CombatTarget
		for i := range p.CombatTargets {
			if p.CombatTargets[i].ObjectID == attStr {
				tgt = &p.CombatTargets[i]
			}
		}
		if src == nil || tgt == nil || !contains(src.ValidTargetIDs, tgt.ResponseID) {
			s.reject(s.lab.t("挡不了这个进攻者", "Can't block that attacker"))
			return
		}
		pair := [2]string{src.ResponseID, tgt.ResponseID}
		for i, b := range s.blocks {
			if b == pair {
				s.blocks = append(s.blocks[:i], s.blocks[i+1:]...)
				s.emit()
				return
			}
		}
		max := src.MaxAssignments
		if max <= 0 {
			max = 1
		}
		used := 0
		for _, b := range s.blocks {
			if b[0] == src.ResponseID {
				used++
			}
		}
		if used >= max { // 挡满了：顶掉它最早挡的那个
			for i, b := range s.blocks {
				if b[0] == src.ResponseID {
					s.blocks = append(s.blocks[:i], s.blocks[i+1:]...)
					break
				}
			}
		}
		s.blocks = append(s.blocks, pair)
		s.emit()
	case "select_card": // 点阻挡者本身＝撤回它的阻挡
		kept := s.blocks[:0]
		for _, b := range s.blocks {
			if src := s.combatSource(b[0]); src == nil || src.ObjectID != entStr {
				kept = append(kept, b)
			}
		}
		s.blocks = kept
		s.emit()
	case "block_all": // 只挡得了一个进攻者的都去挡它
		for _, src := range p.CombatSources {
			if len(src.ValidTargetIDs) != 1 {
				continue
			}
			busy := false
			for _, b := range s.blocks {
				busy = busy || b[0] == src.ResponseID
			}
			if !busy {
				s.blocks = append(s.blocks, [2]string{src.ResponseID, src.ValidTargetIDs[0]})
			}
		}
		s.emit()
	case "ok":
		as := []hub.Assignment{}
		for _, b := range s.blocks {
			as = append(as, hub.Assignment{SourceID: b[0], TargetID: b[1]})
		}
		s.submit(hub.Respond{ResponseID: "$submit", Assignments: as})
	}
}

func (s *Session) onScry(c Command, ent int) {
	p := s.cur
	nd := len(p.ScryDestinations)
	switch c.Command {
	case "select_option":
		if nd == 0 || ent < 0 || ent >= nd*len(p.Cards) {
			return
		}
		card, dest := p.Cards[ent/nd], p.ScryDestinations[ent%nd]
		kept := []string{}
		for _, x := range s.picked { // 每张牌只留一个去处
			if !strings.HasPrefix(x, card.ID+"→") {
				kept = append(kept, x)
			}
		}
		s.picked = append(kept, card.ID+"→"+dest)
		s.emit()
	case "ok":
		if len(s.picked) != len(p.Cards) {
			s.reject(s.lab.t("每张牌都要选去处", "Choose a destination for every card"))
			return
		}
		piles := map[string][]string{}
		order := []string{}
		for _, x := range s.picked {
			parts := strings.SplitN(x, "→", 2)
			if _, ok := piles[parts[1]]; !ok {
				order = append(order, parts[1])
			}
			piles[parts[1]] = append(piles[parts[1]], parts[0])
		}
		out := []hub.ScryPile{}
		for _, d := range order {
			out = append(out, hub.ScryPile{Destination: d, CardIDs: piles[d]})
		}
		s.submit(hub.Respond{ResponseID: "$submit", ScryPiles: out})
	}
}

func (s *Session) submit(r hub.Respond) {
	asked := *s.cur
	r.PromptID = asked.PromptID
	s.cur = nil
	s.promptCards = nil
	if err := s.h.Respond(r); err != nil {
		s.h.Send(map[string]any{"type": "error", "code": "rejected", "message": err.Error()})
		// Hexproof 拒了这个回答，它那边还在等这一问：把这一问原样再问一遍（已选的清空），
		// 不然独弈这边再也收不到它，整局卡死（2026-11 真 hub 实测）。通知类的不重问，免得来回打转。
		if asked.Kind != "acknowledge" && asked.Kind != "diceRolled" {
			s.OnPrompt(asked)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func remove(xs []string, x string) []string {
	out := xs[:0]
	for _, y := range xs {
		if y != x {
			out = append(out, y)
		}
	}
	return out
}

// toggle 点一下选上、再点一下取消；超过上限就顶掉最早选的。
func toggle(xs []string, x string, max int) []string {
	for i, y := range xs {
		if y == x {
			return append(xs[:i:i], xs[i+1:]...)
		}
	}
	xs = append(xs, x)
	if max > 0 && len(xs) > max {
		xs = xs[1:]
	}
	return xs
}
