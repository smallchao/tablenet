// Package example 是参考适配器：一个假平台 example，演示适配器协议的三件事——
// 发布房间、交接档回入座链接、桥接档在对局通道里当「主机」（这里只是把收到的东西原样回显）。
//
// 写真适配器（Hexproof、XMage）时照着它的结构来；协议见 spec/02-适配器协议.md。
package example

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"sync"

	"tablenet/pkg/client"
	"tablenet/pkg/proto"
)

const (
	PlatformID = "example"
	Homepage   = "https://play.example.org"
	Codec      = "example.echo"
)

// Rooms 是适配器上线后发布的两间房：一间交接、一间桥接。
var Rooms = []map[string]any{
	{"localId": "lobby-1", "title": "示例：交接房", "format": "modern", "gameType": "duel", "structure": "bo1",
		"seats": map[string]int{"total": 2, "taken": 1}, "join": "handoff", "host": map[string]string{"displayName": "Alice"}},
	{"localId": "echo-1", "title": "示例：桥接回显房", "format": "standard", "gameType": "duel", "structure": "bo1",
		"seats": map[string]int{"total": 2, "taken": 1}, "join": "bridge", "codec": Codec},
}

func Hello() map[string]any {
	return map[string]any{
		"role": "adapter",
		"platform": map[string]any{
			"platformId": PlatformID, "name": "Example Platform", "homepage": Homepage,
			"joinKinds": []string{"handoff", "bridge"}, "formats": []string{"standard", "modern"},
			"gameTypes": []string{"duel"}, "structures": []string{"bo1", "bo3"}, "codecs": []string{Codec},
			"accountLink": "none", "terms": Homepage + "/terms",
		},
		"client": map[string]string{"name": "tablenet-adapter-example", "version": "0.1.0"},
	}
}

// Run 连上节点、发布房间，然后一直应答，直到 ctx 结束或连接断开。
func Run(ctx context.Context, url string, key ed25519.PrivateKey, log *slog.Logger) error {
	c, err := client.Dial(ctx, url, key, Hello())
	if err != nil {
		return err
	}
	defer c.Close()
	for _, r := range Rooms {
		if _, err := c.Request(ctx, "venue.room.upsert", r); err != nil {
			return err
		}
	}
	log.Info("example.online", "rooms", len(Rooms))
	var mu sync.Mutex
	channels := map[uint32]string{} // 通道 → 玩家
	for {
		m, err := c.Next(ctx)
		if err != nil {
			return err
		}
		if m.IsFrame() {
			mu.Lock()
			_, ok := channels[m.Channel]
			mu.Unlock()
			if ok {
				_ = c.SendFrame(ctx, m.Channel, append([]byte("echo: "), m.Payload...))
			}
			continue
		}
		e := m.Env
		switch e.Type {
		case "venue.seat.request":
			var b struct {
				LocalID string `json:"localId"`
				Bridge  bool   `json:"bridge"`
			}
			_ = e.Decode(&b)
			if b.Bridge {
				_ = c.Reply(ctx, e.ID, "venue.seat.bridged", map[string]any{})
				continue
			}
			_ = c.Reply(ctx, e.ID, "venue.seat.granted", map[string]any{
				"url":  fmt.Sprintf("%s/t/%s?k=%s", Homepage, b.LocalID, proto.B64(proto.RandomBytes(6))[4:]),
				"code": b.LocalID, "expiresAt": "", "note": "在示例平台的网页里打开",
			})
		case "venue.room.create":
			var b struct {
				MatchID string `json:"matchId"`
				Players []struct {
					PlayerID string `json:"playerId"`
					Bridge   bool   `json:"bridge"`
				} `json:"players"`
			}
			_ = e.Decode(&b)
			local := "m-" + b.MatchID
			seats := []map[string]any{}
			for _, p := range b.Players {
				if p.Bridge {
					seats = append(seats, map[string]any{"playerId": p.PlayerID, "bridge": true})
				} else {
					seats = append(seats, map[string]any{"playerId": p.PlayerID, "url": Homepage + "/t/" + local})
				}
			}
			_ = c.Reply(ctx, e.ID, "venue.room.created", map[string]any{"localId": local, "seats": seats})
		case "channel.opened":
			var b struct {
				Channel uint32 `json:"channel"`
				Player  struct {
					PlayerID string `json:"playerId"`
				} `json:"player"`
			}
			_ = e.Decode(&b)
			mu.Lock()
			channels[b.Channel] = b.Player.PlayerID
			mu.Unlock()
		case "channel.closed":
			var b struct {
				Channel uint32 `json:"channel"`
			}
			_ = e.Decode(&b)
			mu.Lock()
			delete(channels, b.Channel)
			mu.Unlock()
		}
	}
}
