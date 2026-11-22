// SPDX-License-Identifier: GPL-3.0-or-later

// Package adapter 把一个 Hexproof hub 接进 Tablenet 节点：
//
//   - 大厅镜像：定时取 hub 的公开房间，发布到节点的房间表；
//   - 交接：玩家要去 Hexproof 客户端打时，给 hub 地址和房号；
//   - 桥接：玩家在独弈里打 Hexproof 规则房，本适配器替他在 hub 上坐下，在对局通道里冒充独弈主机。
//
// 设计见 tablenet 仓库的 spec/07-首批适配器.md 与 spec/08-桥接模式.md。
package adapter

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"tablenet/pkg/client"
	"tablenet/pkg/proto"

	"tablenet-adapter-hexproof/hub"
)

const (
	PlatformID = "hexproof"
	Codec      = "arcane.v0"
	Version    = "0.1.0"
)

type Config struct {
	NodeURL      string        // Tablenet 节点，ws://…/tablenet/1
	HubURL       string        // 适配器自己连 hub 用的地址，ws://127.0.0.1:57320/ws
	PublicServer string        // 交接时告诉玩家的 hub 地址（玩家的 Hexproof 客户端要填它）
	Homepage     string        // 平台主页，必须 https
	LobbyName    string        // 大厅镜像连接在 hub 上的展示名
	PollEvery    time.Duration // 多久取一次 hub 的房间表
}

func (c *Config) defaults() {
	if c.PublicServer == "" {
		c.PublicServer = c.HubURL
	}
	if c.Homepage == "" {
		c.Homepage = "https://github.com/ClayStan404/hexproof"
	}
	if c.LobbyName == "" {
		c.LobbyName = "Tablenet"
	}
	if c.PollEvery == 0 {
		c.PollEvery = 5 * time.Second
	}
}

// Hello 是连 Tablenet 节点时的握手正文。
func Hello(cfg Config) map[string]any {
	return map[string]any{
		"role": "adapter",
		"platform": map[string]any{
			"platformId": PlatformID, "name": "Hexproof", "homepage": cfg.Homepage,
			"joinKinds": []string{"handoff", "bridge"}, "formats": Formats,
			"gameTypes": []string{"duel", "ffa"}, "structures": []string{"bo1", "bo3"}, "codecs": []string{Codec},
			"accountLink": "none", "terms": cfg.Homepage,
		},
		"client": map[string]string{"name": "tablenet-adapter-hexproof", "version": Version},
	}
}

type Adapter struct {
	cfg Config
	log *slog.Logger
	tn  *client.Client

	mu        sync.Mutex
	published map[string]hub.RoomListEntry
	waiting   map[string]seatInfo // playerId → 等 channel.opened 的桥接入座
	bridges   map[uint32]*bridgeRun
}

type seatInfo struct {
	roomID, password string
	room             hub.RoomListEntry
	locale           string
	pre              *hub.Conn // 排队开房时已替这位玩家建好房、坐下的 hub 连接
	preSeat          int
}

// Run 连节点、连 hub 大厅，一直跑到 ctx 结束或某条连接断开。
func Run(ctx context.Context, cfg Config, key ed25519.PrivateKey, log *slog.Logger) error {
	cfg.defaults()
	tn, err := client.Dial(ctx, cfg.NodeURL, key, Hello(cfg))
	if err != nil {
		return err
	}
	defer tn.Close()
	lobby, err := hub.Dial(ctx, cfg.HubURL, cfg.LobbyName, Version)
	if err != nil {
		return err
	}
	defer lobby.Close()
	a := &Adapter{cfg: cfg, log: log, tn: tn, published: map[string]hub.RoomListEntry{},
		waiting: map[string]seatInfo{}, bridges: map[uint32]*bridgeRun{}}
	log.Info("hexproof.online", "hub", cfg.HubURL, "hubVersion", lobby.Welcome.ServerVersion, "forgeRules", lobby.Welcome.ForgeRulesAvailable)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		a.mirrorLobby(ctx, lobby)
	}()
	go func() {
		<-lobby.Done()
		cancel()
	}()
	for {
		m, err := tn.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return lobby.Err()
			}
			return err
		}
		if m.IsFrame() {
			a.mu.Lock()
			b := a.bridges[m.Channel]
			a.mu.Unlock()
			if b != nil {
				b.feed(m.Payload)
			}
			continue
		}
		a.onNode(ctx, m.Env)
	}
}

// ---- 大厅镜像 ----

func (a *Adapter) mirrorLobby(ctx context.Context, lobby *hub.Conn) {
	t := time.NewTicker(a.cfg.PollEvery)
	defer t.Stop()
	for {
		if err := a.syncRooms(ctx, lobby); err != nil {
			a.log.Warn("hexproof.list", "err", err)
			if errors.Is(err, context.Canceled) {
				return
			}
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		case <-lobby.Done():
			return
		}
	}
}

func (a *Adapter) syncRooms(ctx context.Context, lobby *hub.Conn) error {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	e, err := lobby.Request(rctx, "room.list", nil)
	if err != nil {
		return err
	}
	var body struct {
		Rooms []hub.RoomListEntry `json:"rooms"`
	}
	_ = e.Decode(&body)
	seen := map[string]bool{}
	for _, r := range body.Rooms {
		// 新房只发布等人的；已经发布过的房开局了也不撤，改成「对局中」挂着，hub 上整间没了才撤。
		// 开局就撤会让节点以为房没了（2026-11 实测：桥接局刚开就被掐断，见 spec/07）。
		a.mu.Lock()
		_, listed := a.published[r.RoomID]
		a.mu.Unlock()
		waiting := r.PlayerJoinable && r.Phase == "waiting"
		if r.HostingMode == "player" || (!waiting && !listed) {
			continue
		}
		format, gameType := tablenetFormat(r)
		if format == "" {
			continue
		}
		seen[r.RoomID] = true
		a.mu.Lock()
		old, had := a.published[r.RoomID]
		a.published[r.RoomID] = r
		a.mu.Unlock()
		if had && old == r {
			continue
		}
		join := "handoff"
		if r.RulesMode == "forge" {
			join = "bridge" // 解不了 arcane.v0 的玩家，节点会自动退成交接
		}
		up := map[string]any{
			"localId": r.RoomID, "title": r.Name, "format": format, "gameType": gameType,
			"seats":     map[string]int{"total": max(r.MaxSeats, 2), "taken": r.PlayerCount},
			"structure": r.MatchMode, "hasPassword": r.HasPassword, "visibility": "public",
			"spectate": map[string]bool{"allowed": r.AllowSpectators, "seeHands": r.SpectatorsSeeHand},
			"join":     join, "extra": map[string]any{"x-hexproof-rules": r.RulesMode},
		}
		if join == "bridge" {
			up["codec"] = Codec
		}
		if !waiting {
			up["status"] = "playing"
		}
		if _, err := a.tn.Request(ctx, "venue.room.upsert", up); err != nil {
			return err
		}
	}
	a.mu.Lock()
	gone := []string{}
	for id := range a.published {
		if !seen[id] {
			gone = append(gone, id)
			delete(a.published, id)
		}
	}
	a.mu.Unlock()
	for _, id := range gone {
		_ = a.tn.Notify(ctx, "venue.room.remove", map[string]string{"localId": id})
	}
	return nil
}

// ---- 节点发来的事 ----

type seatRequest struct {
	LocalID  string `json:"localId"`
	As       string `json:"as"`
	Password string `json:"password"`
	Bridge   bool   `json:"bridge"`
	Player   struct {
		PlayerID    string `json:"playerId"`
		DisplayName string `json:"displayName"`
		Locale      string `json:"locale"`
	} `json:"player"`
}

func (a *Adapter) onNode(ctx context.Context, e proto.Envelope) {
	switch e.Type {
	case "venue.seat.request":
		var b seatRequest
		_ = e.Decode(&b)
		a.mu.Lock()
		room, ok := a.published[b.LocalID]
		a.mu.Unlock()
		if !ok {
			_ = a.tn.SendEnv(ctx, proto.Error(e.ID, proto.CodeNoRoom, "Hexproof 上没有这个房间了"))
			return
		}
		if b.Bridge && b.As == "player" {
			a.mu.Lock()
			a.waiting[b.Player.PlayerID] = seatInfo{roomID: b.LocalID, password: b.Password, room: room, locale: b.Player.Locale}
			a.mu.Unlock()
			_ = a.tn.Reply(ctx, e.ID, "venue.seat.bridged", map[string]any{})
			return
		}
		note := "打开 Hexproof 客户端，连这台服务器，按房号进房"
		if room.HasPassword {
			note += "（这间房要密码，填房主给你的密码）"
		}
		_ = a.tn.Reply(ctx, e.ID, "venue.seat.granted", map[string]any{
			"connect": map[string]any{"client": "hexproof", "server": a.cfg.PublicServer, "room": b.LocalID, "protocol": hub.Protocol},
			"code":    b.LocalID, "note": note,
		})
	case "venue.room.create":
		go a.createForMatch(ctx, e)
	case "channel.opened":
		var b struct {
			Channel uint32 `json:"channel"`
			LocalID string `json:"localId"`
			As      string `json:"as"`
			Player  struct {
				PlayerID    string `json:"playerId"`
				DisplayName string `json:"displayName"`
			} `json:"player"`
		}
		_ = e.Decode(&b)
		a.mu.Lock()
		info, ok := a.waiting[b.Player.PlayerID]
		delete(a.waiting, b.Player.PlayerID)
		a.mu.Unlock()
		if !ok || info.roomID != b.LocalID {
			_ = a.tn.Notify(ctx, "channel.close", map[string]any{"channel": b.Channel, "code": "no_seat"})
			return
		}
		br := newBridgeRun(a, b.Channel, b.Player.DisplayName, info)
		a.mu.Lock()
		a.bridges[b.Channel] = br
		a.mu.Unlock()
		go func() {
			err := br.run(ctx)
			a.log.Info("bridge.end", "channel", b.Channel, "err", err)
			a.mu.Lock()
			delete(a.bridges, b.Channel)
			a.mu.Unlock()
			_ = a.tn.Notify(context.Background(), "channel.close", map[string]any{"channel": b.Channel, "code": "bridge_ended"})
		}()
	case "channel.closed":
		var b struct {
			Channel uint32 `json:"channel"`
		}
		_ = e.Decode(&b)
		a.mu.Lock()
		br := a.bridges[b.Channel]
		a.mu.Unlock()
		if br != nil {
			br.stop()
		}
	}
}

// createForMatch：排队配到 Hexproof。Hexproof 建房的人自己就占一个座位，所以必须由其中一位桥接玩家来建；
// 两个人都不能桥接时，适配器没法替他们建房（建了自己就占座），只能回错，让节点把他们放回排队。
func (a *Adapter) createForMatch(ctx context.Context, e proto.Envelope) {
	var b struct {
		MatchID   string `json:"matchId"`
		Format    string `json:"format"`
		Structure string `json:"structure"`
		Players   []struct {
			PlayerID    string `json:"playerId"`
			DisplayName string `json:"displayName"`
			Bridge      bool   `json:"bridge"`
		} `json:"players"`
	}
	_ = e.Decode(&b)
	fail := func(msg string) { _ = a.tn.SendEnv(ctx, proto.Error(e.ID, proto.CodeVenueUnavailable, msg)) }
	table, deck := hexproofFormat(b.Format)
	if table == "" {
		fail("Hexproof 开不了这个赛制：" + b.Format)
		return
	}
	creator := -1
	for i, p := range b.Players {
		if p.Bridge {
			creator = i
			break
		}
	}
	if creator < 0 {
		fail("双方都不能桥接：Hexproof 建房的人自己占座，适配器没法替两位玩家建房")
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hc, err := hub.Dial(rctx, a.cfg.HubURL, b.Players[creator].DisplayName, Version)
	if err != nil {
		fail(err.Error())
		return
	}
	password := proto.B64(proto.RandomBytes(9))[4:]
	created, err := hc.Request(rctx, "room.create", map[string]any{
		"name": "Tablenet " + b.MatchID, "format": table, "deckFormat": deck, "maxSeats": len(b.Players),
		"allowSpectators": false, "spectatorsSeeHands": false, "matchMode": b.Structure,
		"rulesMode": "forge", "hostingMode": "server", "password": password,
	})
	if err != nil {
		hc.Close()
		fail("Hexproof 建房失败：" + err.Error())
		return
	}
	var cr struct {
		RoomID   string `json:"roomId"`
		HostSeat int    `json:"hostSeat"`
	}
	_ = created.Decode(&cr)
	room := hub.RoomListEntry{RoomID: cr.RoomID, Format: table, DeckFormat: deck, MatchMode: b.Structure, MaxSeats: len(b.Players), RulesMode: "forge"}
	seats := []map[string]any{}
	a.mu.Lock()
	for i, p := range b.Players {
		switch {
		case i == creator:
			a.waiting[p.PlayerID] = seatInfo{roomID: "m-" + b.MatchID, password: password, room: room, pre: hc, preSeat: cr.HostSeat}
			seats = append(seats, map[string]any{"playerId": p.PlayerID, "bridge": true})
		case p.Bridge:
			a.waiting[p.PlayerID] = seatInfo{roomID: "m-" + b.MatchID, password: password, room: room}
			seats = append(seats, map[string]any{"playerId": p.PlayerID, "bridge": true})
		default:
			seats = append(seats, map[string]any{"playerId": p.PlayerID, "connect": map[string]any{
				"client": "hexproof", "server": a.cfg.PublicServer, "room": cr.RoomID, "password": password, "protocol": hub.Protocol,
			}, "note": "打开 Hexproof 客户端，连这台服务器，输入房号和密码进房"})
		}
	}
	a.mu.Unlock()
	// 桥接玩家进房用的是 hub 的真房号；节点那边的房间名是 m-<matchId>。
	for i, p := range b.Players {
		if i != creator && p.Bridge {
			a.mu.Lock()
			info := a.waiting[p.PlayerID]
			info.room.RoomID = cr.RoomID
			a.waiting[p.PlayerID] = info
			a.mu.Unlock()
		}
	}
	_ = a.tn.Reply(ctx, e.ID, "venue.room.created", map[string]any{"localId": "m-" + b.MatchID, "seats": seats})
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
