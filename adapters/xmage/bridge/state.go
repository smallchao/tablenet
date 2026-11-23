// SPDX-License-Identifier: MIT

package bridge

import "strings"

// 把 XMage 的 GameView 翻成 arcane.v0 的 state 帧。
//
// XMage 给的牌带牌名、系列、收藏编号、费用、类别行和英文规则文字；中文和卡图由独弈按牌名从本地牌库补
// （见 spec/08-桥接模式.md#牌怎么认）。

// seatOf：XMage 的牌手 UUID → arcane 的牌手编号（第一次见到的顺序，整局不变）。
func (s *Session) seatOf(playerID string) int {
	if n, ok := s.seats[playerID]; ok {
		return n
	}
	n := len(s.seats)
	s.seats[playerID] = n
	s.seatIDs = append(s.seatIDs, playerID)
	return n
}

func (s *Session) playerOfSeat(seat int) (string, bool) {
	if seat < 0 || seat >= len(s.seatIDs) {
		return "", false
	}
	return s.seatIDs[seat], true
}

func (s *Session) isPlayer(id string) bool {
	if s.view == nil {
		return false
	}
	for _, p := range s.view.Players {
		if p.ID == id {
			return true
		}
	}
	return false
}

func rulesText(rules []string) string {
	out := []string{}
	for _, r := range rules {
		if t := plain(r); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

func counters(cs []Counter) []map[string]any {
	out := []map[string]any{}
	for _, c := range cs {
		out = append(out, map[string]any{"type": c.Name, "label": c.Name, "count": c.Count})
	}
	return out
}

func (s *Session) cardJSON(c Card, zone string, owner, controller int) map[string]any {
	id := s.ids.of(c.ID)
	if c.FaceDown && c.Name == "" {
		return hiddenJSON(id, zone, owner, controller)
	}
	m := map[string]any{
		"id": id, "name": c.Name, "zhName": "", "mana": c.Cost, "type": strings.Join(strings.Fields(c.Types), " "),
		"rulesText": rulesText(c.Rules), "setCode": c.Set, "ownerPlayerId": owner, "controllerPlayerId": controller,
		"zone": zone, "tapped": c.Tapped, "faceDown": c.FaceDown, "summoningSick": c.Sick, "token": c.Token,
		"counters": counters(c.Counters), "damage": c.Damage, "transformed": c.Transformed,
		"power": nil, "toughness": nil, "loyalty": strOrNil(c.Loyalty), "defense": strOrNil(c.Defense),
	}
	if c.Creature {
		m["power"], m["toughness"] = atoiOrNil(c.Power), atoiOrNil(c.Toughness)
	}
	if c.PhasedIn != nil && !*c.PhasedIn {
		m["phasedOut"] = true
	}
	if c.AttachedTo != "" && !s.isPlayer(c.AttachedTo) {
		m["attachedToCardId"] = s.ids.of(c.AttachedTo)
	}
	return m
}

func hiddenJSON(id int, zone string, owner, controller int) map[string]any {
	return map[string]any{
		"id": id, "name": "Face Down Card", "zhName": "牌背", "mana": "", "type": "", "state": "Unknown",
		"zone": zone, "faceDown": true, "hidden": true, "ownerPlayerId": owner, "controllerPlayerId": controller,
		"tapped": false, "power": nil, "toughness": nil,
	}
}

// phase：XMage 的 PhaseStep → arcane.v0 的阶段名。
func phase(step string) string {
	switch step {
	case "UNTAP":
		return "UNTAP"
	case "UPKEEP":
		return "UPKEEP"
	case "DRAW":
		return "DRAW"
	case "PRECOMBAT_MAIN":
		return "MAIN1"
	case "BEGIN_COMBAT":
		return "COMBAT_BEGIN"
	case "DECLARE_ATTACKERS":
		return "COMBAT_DECLARE_ATTACKERS"
	case "DECLARE_BLOCKERS":
		return "COMBAT_DECLARE_BLOCKERS"
	case "FIRST_COMBAT_DAMAGE":
		return "COMBAT_FIRST_STRIKE_DAMAGE"
	case "COMBAT_DAMAGE":
		return "COMBAT_DAMAGE"
	case "END_COMBAT":
		return "COMBAT_END"
	case "POSTCOMBAT_MAIN":
		return "MAIN2"
	case "END_TURN":
		return "END_OF_TURN"
	case "CLEANUP":
		return "CLEANUP"
	}
	return step
}

func (s *Session) stateJSON() map[string]any {
	v := s.view
	players := []map[string]any{}
	// 战场上谁控制哪张：XMage 按牌手分区给战场，owner 只有名字；控制者就是所在的那一区。
	nameSeat := map[string]int{}
	for _, p := range v.Players {
		nameSeat[p.Name] = s.seatOf(p.ID)
	}
	zone := func(cards []Card, z string, owner int) []map[string]any {
		out := []map[string]any{}
		for _, c := range cards {
			out = append(out, s.cardJSON(c, z, owner, owner))
		}
		return out
	}
	for _, p := range v.Players {
		seat := s.seatOf(p.ID)
		var hand []map[string]any
		if p.ID == v.MyPlayerID {
			hand = zone(v.Hand, "Hand", seat)
		} else {
			hand = []map[string]any{}
			for i := 0; i < p.HandCount; i++ {
				hand = append(hand, hiddenJSON(s.ids.of(handSlot(p.ID, i)), "Hand", seat, seat))
			}
		}
		bf := []map[string]any{}
		for _, c := range p.Battlefield {
			owner := seat
			if o, ok := nameSeat[c.OwnerName]; ok {
				owner = o
			}
			bf = append(bf, s.cardJSON(c, "Battlefield", owner, seat))
		}
		command := []map[string]any{}
		for _, c := range p.Command {
			command = append(command, map[string]any{"id": s.ids.of(c.ID), "name": c.Name, "zhName": "", "mana": "", "type": c.Kind,
				"rulesText": rulesText(c.Rules), "setCode": c.Set, "zone": "Command", "ownerPlayerId": seat, "controllerPlayerId": seat,
				"emblem": c.Kind == "EmblemView", "commander": c.Kind == "CommanderView"})
		}
		pool := map[string]int{"W": 0, "U": 0, "B": 0, "R": 0, "G": 0, "C": 0}
		for k, n := range p.ManaPool {
			pool[k] = n
		}
		players = append(players, map[string]any{
			"id": seat, "name": p.Name, "isYou": p.ID == v.MyPlayerID, "isControlledLocally": p.ID == v.MyPlayerID,
			"team": seat, "hasPriority": p.HasPriority, "hasLost": p.HasLeft,
			"isExtraTurn": false, "extraTurnCount": 0, "life": p.Life,
			"handCount": len(hand), "maxHandSize": 7, "unlimitedHandSize": false,
			"libraryCount": p.LibraryCount, "library": []any{},
			"sideboardCount": 0, "sideboard": []any{},
			"landsPlayedThisTurn": 0, "maxLandPlays": 1, "unlimitedLandPlays": false, "cardsDrawnThisTurn": 0,
			"manaShards": 0, "currentPlaneName": nil, "preventNextDamage": 0, "additionalVote": 0,
			"optionalAdditionalVote": 0, "controlVote": false, "additionalVillainousChoices": 0,
			"delirium": false, "cityBlessing": false, "speed": 0, "ringTemptations": 0,
			"diceRollsThisTurn": []int{}, "unlockedDoors": []string{},
			"graveyardCount": len(p.Graveyard), "graveyard": zone(p.Graveyard, "Graveyard", seat),
			"exileCount": len(p.Exile), "exile": zone(p.Exile, "Exile", seat),
			"commandZone": command, "activeEffects": []any{}, "counters": counters(p.Counters),
			"commanders": []any{}, "completedDungeons": []any{}, "mindSlaveMasterId": nil,
			"manaPool": pool, "battlefield": bf, "hand": hand,
		})
	}
	stack := []map[string]any{}
	for _, o := range v.Stack { // XMage 的堆叠顶在前，和 arcane.v0 一样
		tc, tp := []int{}, []int{}
		for _, t := range o.Targets {
			if s.isPlayer(t) {
				tp = append(tp, s.seatOf(t))
			} else {
				tc = append(tc, s.ids.of(t))
			}
		}
		var src any
		name := o.Name
		if o.SourceID != "" {
			src = s.ids.of(o.SourceID)
			if o.SourceName != "" {
				name = o.SourceName
			}
		}
		stack = append(stack, map[string]any{
			"id": s.ids.of(o.ID), "sourceCardId": src, "sourceName": name, "sourceZhName": "",
			"text": rulesText(o.Rules), "activatingPlayerId": nil, "targetCardIds": tc, "targetPlayerIds": tp,
			"targetStackItemIds": []int{}, "isAbility": o.StackAbility, "isTrigger": o.AbilityType == "TRIGGERED",
			"isOptionalTrigger": false, "autoYield": false, "triggerDecision": "ask", "triggerConfigurable": false,
		})
	}
	var combatState any
	if len(v.Combat) > 0 {
		rows := []map[string]any{}
		for _, g := range v.Combat {
			def := map[string]any{"kind": "player", "id": 0}
			if s.isPlayer(g.DefenderID) {
				def["id"] = s.seatOf(g.DefenderID)
			} else if g.DefenderID != "" {
				def = map[string]any{"kind": "card", "id": s.ids.of(g.DefenderID)}
			}
			blockers := []int{}
			for _, b := range g.Blockers {
				blockers = append(blockers, s.ids.of(b))
			}
			for _, a := range g.Attackers {
				rows = append(rows, map[string]any{"attackerId": s.ids.of(a), "defender": def,
					"blockerIds": blockers, "plannedBlockerIds": []int{}})
			}
		}
		if len(rows) > 0 {
			combatState = map[string]any{"attackers": rows}
		}
	}
	revealed := []map[string]any{}
	for _, c := range s.promptCards {
		revealed = append(revealed, s.cardJSON(c, "Reveal", -1, -1))
	}
	active := 0
	if v.ActivePlayerID != "" {
		active = s.seatOf(v.ActivePlayerID)
	}
	return map[string]any{
		"type": "state", "turnNumber": v.Turn, "phase": phase(v.Step),
		"activePlayerId": active, "viewingPlayerId": s.mySeat(), "dayTime": nil,
		"poisonCountersToLose": 10, "stormCount": 0, "monarchPlayerId": s.flagSeat(func(p Player) bool { return p.Monarch }),
		"initiativePlayerId": s.flagSeat(func(p Player) bool { return p.Initiative }),
		"planarPlayerId": nil, "stackSize": len(stack), "stack": stack, "pendingCasts": []any{},
		"combat": combatState, "revealedCards": revealed, "visibleZones": []any{}, "phaseStops": []any{},
		"autoPassMode": "off", "yieldMarker": nil,
		"yieldSettings": map[string]bool{"abilityScope": false, "autoYieldsDisabled": true, "autoTriggersDisabled": true},
		"gameLog":       []any{}, "dependencies": []any{}, "delayedTriggers": []any{}, "players": players,
	}
}

func (s *Session) flagSeat(f func(Player) bool) any {
	for _, p := range s.view.Players {
		if f(p) {
			return s.seatOf(p.ID)
		}
	}
	return nil
}

func (s *Session) mySeat() int {
	if s.view != nil && s.view.MyPlayerID != "" {
		return s.seatOf(s.view.MyPlayerID)
	}
	return 0
}

func handSlot(player string, i int) string { return "hand-slot:" + player + ":" + itoa(i) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
