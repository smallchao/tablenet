// SPDX-License-Identifier: GPL-3.0-or-later

package bridge

import (
	"strconv"

	"tablenet-adapter-hexproof/hub"
)

// 把 Hexproof 的 rules.snapshot 翻成 arcane.v0 的 state 帧。
//
// Hexproof 只给牌的身份（牌名、系列、收藏编号），不给费用、类型、规则文字；
// 这些空着，由独弈按牌名从本地牌库补（见 spec/08-桥接模式.md#牌怎么认）。

func (s *Session) cardJSON(c hub.CardState, zone string) map[string]any {
	id := s.ids.of(c.ID)
	if !c.Visible || c.Identity == nil {
		return hiddenJSON(id, zone, c.OwnerSeat, c.ControllerSeat)
	}
	counters := []map[string]any{}
	for _, k := range c.Counters {
		counters = append(counters, map[string]any{"type": k.Name, "label": k.Name, "count": k.Value})
	}
	m := map[string]any{
		"id": id, "name": c.Identity.Name, "zhName": "", "mana": "", "type": "", "rulesText": "",
		"setCode": c.Identity.SetCode, "ownerPlayerId": c.OwnerSeat, "controllerPlayerId": c.ControllerSeat,
		"zone": zone, "tapped": c.Tapped, "faceDown": c.FaceDown, "summoningSick": c.SummoningSick,
		"token": c.Identity.Token, "counters": counters, "damage": c.Damage,
		"power": atoiOrNil(c.Power), "toughness": atoiOrNil(c.Toughness),
	}
	if c.AttachedTo != "" {
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

var arcaneZone = map[string]string{
	"hand": "Hand", "battlefield": "Battlefield", "graveyard": "Graveyard", "exile": "Exile",
	"library": "Library", "command": "Command", "sideboard": "Sideboard", "stack": "Stack",
}

func (s *Session) stateJSON(snap *hub.Snapshot) map[string]any {
	players := []map[string]any{}
	byZone := map[[2]any][]map[string]any{}
	counts := map[[2]any]int{}
	combat := []map[string]any{}
	for _, z := range snap.Zones {
		key := [2]any{z.OwnerSeat, z.Zone}
		counts[key] = z.Count
		for _, c := range z.Cards {
			byZone[key] = append(byZone[key], s.cardJSON(c, arcaneZone[z.Zone]))
			if z.Zone == "battlefield" && c.Attacking {
				def := map[string]any{"kind": "player", "id": 0}
				if c.AttackingPlayer != nil {
					def["id"] = *c.AttackingPlayer
				} else if c.AttackingTarget != "" {
					def = map[string]any{"kind": "card", "id": s.ids.of(c.AttackingTarget)}
				}
				combat = append(combat, map[string]any{"attackerId": s.ids.of(c.ID), "defender": def,
					"blockerIds": []int{}, "plannedBlockerIds": []int{}})
			}
		}
	}
	// 阻挡关系挂在阻挡者身上（blocking = 它挡的进攻者），倒过来填进每个进攻者。
	for _, z := range snap.Zones {
		for _, c := range z.Cards {
			for _, a := range c.Blocking {
				aid := s.ids.of(a)
				for _, row := range combat {
					if row["attackerId"] == aid {
						row["blockerIds"] = append(row["blockerIds"].([]int), s.ids.of(c.ID))
					}
				}
			}
		}
	}
	zone := func(seat int, name string) []map[string]any {
		if v := byZone[[2]any{seat, name}]; v != nil {
			return v
		}
		return []map[string]any{}
	}
	for _, p := range snap.Players {
		hand := zone(p.Seat, "hand")
		// 对手的手牌 Hexproof 只给张数时，补成那么多张牌背。
		if n := counts[[2]any{p.Seat, "hand"}]; len(hand) < n {
			for i := len(hand); i < n; i++ {
				hand = append(hand, hiddenJSON(s.ids.of(handSlot(p.Seat, i)), "Hand", p.Seat, p.Seat))
			}
		}
		pool := map[string]int{"W": 0, "U": 0, "B": 0, "R": 0, "G": 0, "C": 0}
		for _, m := range p.Mana {
			if _, ok := pool[m.Name]; ok {
				pool[m.Name] = m.Value
			}
		}
		counters := []map[string]any{}
		for _, k := range p.Counter {
			counters = append(counters, map[string]any{"type": k.Name, "label": k.Name, "count": k.Value})
		}
		players = append(players, map[string]any{
			"id": p.Seat, "name": p.Name, "isYou": p.Seat == s.seat, "isControlledLocally": p.Seat == s.seat,
			"team": p.Seat, "hasPriority": p.Seat == snap.PrioritySeat, "hasLost": p.Status == "lost" || p.Status == "eliminated",
			"isExtraTurn": false, "extraTurnCount": 0, "life": p.Life,
			"handCount": len(hand), "maxHandSize": 7, "unlimitedHandSize": false,
			"libraryCount": counts[[2]any{p.Seat, "library"}], "library": []any{},
			"sideboardCount": 0, "sideboard": []any{},
			"landsPlayedThisTurn": 0, "maxLandPlays": 1, "unlimitedLandPlays": false, "cardsDrawnThisTurn": 0,
			"manaShards": 0, "currentPlaneName": nil, "preventNextDamage": 0, "additionalVote": 0,
			"optionalAdditionalVote": 0, "controlVote": false, "additionalVillainousChoices": 0,
			"delirium": false, "cityBlessing": false, "speed": 0, "ringTemptations": 0,
			"diceRollsThisTurn": []int{}, "unlockedDoors": []string{},
			"graveyardCount": counts[[2]any{p.Seat, "graveyard"}], "graveyard": zone(p.Seat, "graveyard"),
			"exileCount": counts[[2]any{p.Seat, "exile"}], "exile": zone(p.Seat, "exile"),
			"commandZone": zone(p.Seat, "command"), "activeEffects": []any{}, "counters": counters,
			"commanders": []any{}, "completedDungeons": []any{}, "mindSlaveMasterId": nil,
			"manaPool": pool, "battlefield": zone(p.Seat, "battlefield"), "hand": hand,
		})
	}
	stack := []map[string]any{}
	for i := len(snap.Stack) - 1; i >= 0; i-- { // arcane.v0 的堆叠顶在前
		o := snap.Stack[i]
		tc, tp := []int{}, []int{}
		for _, t := range o.Targets {
			if t.Seat != nil {
				tp = append(tp, *t.Seat)
			} else if t.ObjectID != "" {
				tc = append(tc, s.ids.of(t.ObjectID))
			}
		}
		var src any
		if o.SourceID != "" {
			src = s.ids.of(o.SourceID)
		}
		stack = append(stack, map[string]any{
			"id": s.ids.of(o.ID), "sourceCardId": src, "sourceName": o.Identity.Name, "sourceZhName": "",
			"text": o.Text, "activatingPlayerId": o.ControllerSeat, "targetCardIds": tc, "targetPlayerIds": tp,
			"targetStackItemIds": []int{}, "isAbility": o.SourceID != "" && o.SourceID != o.ID, "isTrigger": false,
			"isOptionalTrigger": false, "autoYield": false, "triggerDecision": "ask", "triggerConfigurable": false,
		})
	}
	var combatState any
	if len(combat) > 0 {
		combatState = map[string]any{"attackers": combat}
	}
	revealed := s.promptCards
	if revealed == nil {
		revealed = []map[string]any{}
	}
	return map[string]any{
		"type": "state", "turnNumber": snap.Turn, "phase": phase(snap.Step),
		"activePlayerId": snap.ActiveSeat, "viewingPlayerId": s.seat, "dayTime": nil,
		"poisonCountersToLose": 10, "stormCount": 0, "monarchPlayerId": nil, "initiativePlayerId": nil,
		"planarPlayerId": nil, "stackSize": len(stack), "stack": stack, "pendingCasts": []any{},
		"combat": combatState, "revealedCards": revealed, "visibleZones": []any{}, "phaseStops": []any{},
		"autoPassMode": "off", "yieldMarker": nil,
		"yieldSettings": map[string]bool{"abilityScope": false, "autoYieldsDisabled": true, "autoTriggersDisabled": true},
		"gameLog":       []any{}, "dependencies": []any{}, "delayedTriggers": []any{}, "players": players,
	}
}

func handSlot(seat, i int) string { return "hand-slot:" + strconv.Itoa(seat) + ":" + strconv.Itoa(i) }
