// SPDX-License-Identifier: GPL-3.0-or-later

package bridge

import (
	"strconv"
	"strings"
)

// ids 把 Hexproof 的字符串编号（对象、提问里的 responseId）换成 arcane.v0 要的整数，整局稳定。
type ids struct {
	toInt map[string]int
	toStr map[int]string
	next  int
}

func newIDs() *ids { return &ids{toInt: map[string]int{}, toStr: map[int]string{}, next: 1} }

func (m *ids) of(s string) int {
	if s == "" {
		return 0
	}
	if n, ok := m.toInt[s]; ok {
		return n
	}
	n := m.next
	m.next++
	m.toInt[s], m.toStr[n] = n, s
	return n
}

func (m *ids) back(n int) (string, bool) {
	s, ok := m.toStr[n]
	return s, ok
}

// atoiOrNil：Hexproof 的攻防是字符串（可能是 "*"），能读成整数才给。
func atoiOrNil(s string) any {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return n
}

// phase 把阶段名统一成 arcane.v0 的写法。Hexproof 规则房底下是 Forge，步骤名本来就和独弈一样（MAIN1、
// COMBAT_DECLARE_ATTACKERS……）；手动房用的是 main_1 这类小写名，也一并认。
func phase(step string) string {
	switch strings.ToLower(step) {
	case "untap":
		return "UNTAP"
	case "upkeep":
		return "UPKEEP"
	case "draw":
		return "DRAW"
	case "main_1", "main1":
		return "MAIN1"
	case "begin_combat", "combat_begin":
		return "COMBAT_BEGIN"
	case "declare_attackers", "combat_declare_attackers":
		return "COMBAT_DECLARE_ATTACKERS"
	case "declare_blockers", "combat_declare_blockers":
		return "COMBAT_DECLARE_BLOCKERS"
	case "combat_first_strike_damage":
		return "COMBAT_FIRST_STRIKE_DAMAGE"
	case "combat_damage":
		return "COMBAT_DAMAGE"
	case "end_combat", "combat_end":
		return "COMBAT_END"
	case "main_2", "main2":
		return "MAIN2"
	case "end", "end_of_turn":
		return "END_OF_TURN"
	case "cleanup":
		return "CLEANUP"
	}
	return strings.ToUpper(step)
}

// labels：界面上的几个按钮字。玩家 locale 是中文就出中文。
type labels struct{ zh bool }

func (l labels) t(zh, en string) string {
	if l.zh {
		return zh
	}
	return en
}
