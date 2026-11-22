// SPDX-License-Identifier: GPL-3.0-or-later

package adapter

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"

	"tablenet-adapter-hexproof/hub"
)

// Hexproof 把「牌桌类型」（format：modern／duel／edh）和「赛制」（deckFormat）分开记；
// Tablenet 只有一个 format，再加一个局型 gameType。

// tablenetFormat：Hexproof 的房间 → Tablenet 的赛制和局型。认不出的返回空串，这种房不发布。
func tablenetFormat(r hub.RoomListEntry) (format, gameType string) {
	switch r.DeckFormat {
	case "standard", "pioneer", "modern", "legacy", "vintage", "pauper":
		format = r.DeckFormat
	case "commander":
		format = "commander"
	case "duel":
		format = "duel-commander"
	case "custom":
		format = "casual"
		if r.Format == "edh" {
			format = "casual-commander"
		}
	case "limited":
		format = "sealed"
	case "cube":
		format = "cube"
	default:
		return "", ""
	}
	gameType = "duel"
	if r.Format == "edh" && r.MaxSeats > 2 {
		gameType = "ffa"
	}
	return format, gameType
}

// hexproofFormat：Tablenet 的赛制 → Hexproof 开房用的 (format, deckFormat)。开不了的返回空串。
func hexproofFormat(format string) (table, deck string) {
	switch format {
	case "standard", "pioneer", "modern", "legacy", "vintage", "pauper":
		return "modern", format
	case "commander":
		return "edh", "commander"
	case "duel-commander":
		return "duel", "duel"
	case "casual":
		return "modern", "custom"
	case "casual-commander":
		return "edh", "custom"
	}
	return "", ""
}

// Formats 是握手时报给节点的、本平台能开的赛制。
var Formats = []string{"standard", "pioneer", "modern", "legacy", "vintage", "pauper", "commander", "duel-commander", "casual", "casual-commander"}

// missingPrintings 列出缺系列或收藏编号的牌（最多 5 张），都齐了返回空串。
func missingPrintings(boards ...[]hub.DeckCard) string {
	names := []string{}
	for _, board := range boards {
		for _, c := range board {
			if c.SetCode == "" || c.CollectorNumber == "" {
				names = append(names, c.Name)
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	more := ""
	if len(names) > 5 {
		more = fmt.Sprintf(" 等 %d 张", len(names))
		names = names[:5]
	}
	return strings.Join(names, "、") + more
}

// parseDeck 读独弈的 .dck 牌组文本：[Main]、[Sideboard]、[Commander] 几节，每行「张数 牌名|系列|收藏号」。
func parseDeck(text string) (main, side []hub.DeckCard, commanders []string) {
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
		card := hub.DeckCard{Name: strings.TrimSpace(parts[0]), Count: count}
		if len(parts) > 1 {
			card.SetCode = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			card.CollectorNumber = strings.TrimSpace(parts[2])
		}
		if card.Name == "" || count <= 0 {
			continue
		}
		switch section {
		case "main":
			main = append(main, card)
		case "sideboard":
			side = append(side, card)
		case "commander":
			commanders = append(commanders, card.Name)
			main = append(main, card)
		}
	}
	if main == nil {
		main = []hub.DeckCard{}
	}
	if side == nil {
		side = []hub.DeckCard{}
	}
	return main, side, commanders
}
