// SPDX-License-Identifier: MIT

package xmage

import (
	"bufio"
	"strconv"
	"strings"
)

// XMage 的一张桌子有「游戏类型」（Two Player Duel、Commander Free For All……，决定人数和统帅规则）
// 和「牌组类型」（Constructed - Modern、Variant Magic - Commander……，决定赛制）。
// Tablenet 只有一个 format，再加局型 gameType（duel／ffa）。

type xmageFormat struct {
	format   string // Tablenet 赛制名
	deckType string // XMage 牌组类型
	duel     string // 一对一用的游戏类型
	ffa      string // 多人乱斗用的游戏类型（空＝不支持）
}

var xmageFormats = []xmageFormat{
	{"standard", "Constructed - Standard", "Two Player Duel", "Free For All"},
	{"pioneer", "Constructed - Pioneer", "Two Player Duel", "Free For All"},
	{"modern", "Constructed - Modern", "Two Player Duel", "Free For All"},
	{"premodern", "Constructed - Premodern", "Two Player Duel", "Free For All"},
	{"legacy", "Constructed - Legacy", "Two Player Duel", "Free For All"},
	{"vintage", "Constructed - Vintage", "Two Player Duel", "Free For All"},
	{"pauper", "Constructed - Pauper", "Two Player Duel", "Free For All"},
	{"historic", "Constructed - Historic", "Two Player Duel", "Free For All"},
	{"casual", "Constructed - Freeform", "Two Player Duel", "Free For All"},
	{"commander", "Variant Magic - Commander", "Commander Two Player Duel", "Commander Free For All"},
	{"duel-commander", "Variant Magic - Duel Commander", "Commander Two Player Duel", ""},
	{"brawl", "Variant Magic - Brawl", "Brawl Two Player Duel", "Brawl Free For All"},
	{"oathbreaker", "Variant Magic - Oathbreaker", "Oathbreaker Two Player Duel", "Oathbreaker Free For All"},
	{"casual-commander", "Variant Magic - Freeform Commander", "Freeform Commander Two Player Duel", "Freeform Commander Free For All"},
}

// Formats 是握手时声明的赛制。
func Formats() []string {
	out := []string{}
	for _, f := range xmageFormats {
		out = append(out, f.format)
	}
	return out
}

// tablenetFormat：XMage 桌子的 (游戏类型, 牌组类型) → Tablenet 的 (format, gameType)。认不出返回空串，这种桌不发布。
func tablenetFormat(gameType, deckType string) (format, tnGameType string) {
	for _, f := range xmageFormats {
		if f.deckType != deckType {
			continue
		}
		switch gameType {
		case f.duel:
			return f.format, "duel"
		case f.ffa:
			if f.ffa != "" {
				return f.format, "ffa"
			}
		}
	}
	return "", ""
}

// xmageTable：Tablenet 的 (format, gameType) → XMage 开桌用的 (游戏类型, 牌组类型)。开不了返回空串。
func xmageTable(format, gameType string) (xGameType, deckType string) {
	for _, f := range xmageFormats {
		if f.format != format {
			continue
		}
		switch gameType {
		case "duel", "":
			return f.duel, f.deckType
		case "ffa":
			if f.ffa != "" {
				return f.ffa, f.deckType
			}
		}
	}
	return "", ""
}

// isCommanderType：这种游戏类型在独弈那边算 commander 局（统帅区、40 血）。
func isCommanderType(xGameType string) bool {
	l := strings.ToLower(xGameType)
	return strings.Contains(l, "commander") || strings.Contains(l, "brawl") || strings.Contains(l, "oathbreaker") || strings.Contains(l, "leaders")
}

// deckCard 是交给驱动的一张牌：名字，可能带系列和收藏编号。
type deckCard struct {
	Name   string `json:"name"`
	Set    string `json:"set,omitempty"`
	Number string `json:"number,omitempty"`
	Count  int    `json:"count"`
}

type deckList struct {
	Name string     `json:"name"`
	Main []deckCard `json:"main"`
	Side []deckCard `json:"side"`
}

// parseDeck 读独弈 join 里的牌表（[main]／[sideboard]／[commander] 分节，每行「数量 牌名|系列|编号」）。
// XMage 把统帅放在备牌里（Variant Magic 的牌组类型从备牌认统帅），所以 [commander] 归进 Side。
func parseDeck(text string) deckList {
	d := deckList{Name: "Tablenet", Main: []deckCard{}, Side: []deckCard{}}
	section := "main"
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		if section != "main" && section != "sideboard" && section != "commander" {
			continue
		}
		count := 1
		if sp := strings.IndexByte(line, ' '); sp > 0 {
			if n, err := strconv.Atoi(line[:sp]); err == nil {
				count, line = n, strings.TrimSpace(line[sp+1:])
			}
		}
		parts := strings.Split(line, "|")
		c := deckCard{Name: strings.TrimSpace(parts[0]), Count: count}
		if len(parts) > 1 {
			c.Set = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			c.Number = strings.TrimSpace(parts[2])
		}
		if c.Name == "" || count <= 0 {
			continue
		}
		if section == "main" {
			d.Main = append(d.Main, c)
		} else {
			d.Side = append(d.Side, c)
		}
	}
	return d
}
