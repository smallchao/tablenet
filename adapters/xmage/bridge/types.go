// SPDX-License-Identifier: MIT

package bridge

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// 下面这些是 Java 驱动（driver/src/tablenet/xmage/Views.java）压平后的 XMage 对象。

type Card struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	FullName    string    `json:"fullName"`
	Set         string    `json:"set"`
	Number      string    `json:"number"`
	Cost        string    `json:"cost"`
	MV          int       `json:"mv"`
	Types       string    `json:"types"`
	Rules       []string  `json:"rules"`
	Power       string    `json:"power"`
	Toughness   string    `json:"toughness"`
	Loyalty     string    `json:"loyalty"`
	Defense     string    `json:"defense"`
	FaceDown    bool      `json:"faceDown"`
	Token       bool      `json:"token"`
	Ability     bool      `json:"ability"`
	Transformed bool      `json:"transformed"`
	Creature    bool      `json:"creature"`
	Land        bool      `json:"land"`
	Counters    []Counter `json:"counters"`
	Targets     []string  `json:"targets"`
	ParentID    string    `json:"parentId"`
	// 战场上的永久物才有
	Tapped     bool   `json:"tapped"`
	Damage     int    `json:"damage"`
	Sick       bool   `json:"sick"`
	AttachedTo string `json:"attachedTo"`
	OwnerName  string `json:"ownerName"`
	PhasedIn   *bool  `json:"phasedIn"`
	// 堆叠上的异能
	StackAbility bool   `json:"stackAbility"`
	SourceID     string `json:"sourceId"`
	SourceName   string `json:"sourceName"`
	AbilityType  string `json:"abilityType"`
}

type Counter struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type Player struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Life         int            `json:"life"`
	LibraryCount int            `json:"libraryCount"`
	HandCount    int            `json:"handCount"`
	Wins         int            `json:"wins"`
	WinsNeeded   int            `json:"winsNeeded"`
	Active       bool           `json:"active"`
	HasPriority  bool           `json:"hasPriority"`
	HasLeft      bool           `json:"hasLeft"`
	Monarch      bool           `json:"monarch"`
	Initiative   bool           `json:"initiative"`
	Counters     []Counter      `json:"counters"`
	ManaPool     map[string]int `json:"manaPool"`
	Graveyard    []Card         `json:"graveyard"`
	Exile        []Card         `json:"exile"`
	Battlefield  []Card         `json:"battlefield"`
	Command      []struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		Set   string   `json:"set"`
		Rules []string `json:"rules"`
		Kind  string   `json:"kind"`
	} `json:"command"`
}

type CombatGroup struct {
	DefenderID   string   `json:"defenderId"`
	DefenderName string   `json:"defenderName"`
	Blocked      bool     `json:"blocked"`
	Attackers    []string `json:"attackers"`
	Blockers     []string `json:"blockers"`
}

type Playable struct {
	AbilityIDs []string `json:"abilityIds"`
	Names      []string `json:"names"`
}

type View struct {
	Turn               int      `json:"turn"`
	Phase              string   `json:"phase"`
	Step               string   `json:"step"`
	ActivePlayerID     string   `json:"activePlayerId"`
	PriorityPlayerName string   `json:"priorityPlayerName"`
	Players            []Player `json:"players"`
	MyPlayerID         string   `json:"myPlayerId"`
	Hand               []Card   `json:"hand"`
	Stack              []Card   `json:"stack"`
	Exile              []struct {
		Cards []Card `json:"cards"`
	} `json:"exile"`
	Revealed []struct {
		Name  string `json:"name"`
		Cards []Card `json:"cards"`
	} `json:"revealed"`
	Combat   []CombatGroup       `json:"combat"`
	Playable map[string]Playable `json:"playable"`
}

type ChoiceItem struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

type Choice struct {
	Message    string       `json:"message"`
	SubMessage string       `json:"subMessage"`
	Required   bool         `json:"required"`
	Search     bool         `json:"search"`
	ManaColor  bool         `json:"manaColor"`
	KeyChoice  bool         `json:"keyChoice"`
	Items      []ChoiceItem `json:"items"`
	Special    *string      `json:"special"`
}

type MultiAmount struct {
	Message string `json:"message"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Default int    `json:"default"`
}

// Prompt 是一次 XMage 提问（GAME_SELECT、GAME_TARGET……）的内容。
type Prompt struct {
	Method  string         `json:"-"`
	Message string         `json:"message"`
	Flag    bool           `json:"flag"` // GAME_TARGET：required
	Min     int            `json:"min"`
	Max     int            `json:"max"`
	Targets []string       `json:"targets"`
	Options map[string]any `json:"options"`
	Cards1  []Card         `json:"cards1"`
	Cards2  []Card         `json:"cards2"`
	Choice  *Choice        `json:"choice"`
	Multi   []MultiAmount  `json:"multi"`
	View    *View          `json:"view"`
	// GAME_CHOOSE_ABILITY（AbilityPickerView）
	Choices []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"choices"`
}

func (p *Prompt) option(k string) any {
	if p.Options == nil {
		return nil
	}
	return p.Options[k]
}

func (p *Prompt) optionIDs(k string) []string {
	var out []string
	if l, ok := p.option(k).([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func (p *Prompt) optionText(k string) string {
	s, _ := p.option(k).(string)
	return s
}

func decode(raw json.RawMessage, v any) bool { return len(raw) > 0 && json.Unmarshal(raw, v) == nil }

var tagRE = regexp.MustCompile(`<[^>]*>`)

// plain 去掉 XMage 提示文字里的 HTML 标签（它用 <font color> 上色）。
func plain(s string) string {
	s = tagRE.ReplaceAllString(s, "")
	s = strings.NewReplacer("&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`).Replace(s)
	return strings.TrimSpace(s)
}

func atoiOrNil(s string) any {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return n
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ids 把 XMage 的 UUID 换成 arcane.v0 要的整数，整局稳定。
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

type labels struct{ zh bool }

func (l labels) t(zh, en string) string {
	if l.zh {
		return zh
	}
	return en
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
