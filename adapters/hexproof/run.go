// SPDX-License-Identifier: GPL-3.0-or-later

package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"tablenet-adapter-hexproof/bridge"
	"tablenet-adapter-hexproof/hub"
)

// arcaneProtocolVersion 是独弈联机握手的版本号（独弈 mtg-engine-sidecar 的 net::PROTOCOL_VERSION）。
const arcaneProtocolVersion = 3

// bridgeRun 是一位桥接玩家：一头是 Tablenet 对局通道（JSON Lines，讲 arcane.v0），一头是他在 hub 上的会话。
type bridgeRun struct {
	a      *Adapter
	ch     uint32
	name   string
	info   seatInfo
	in     chan []byte
	buf    bytes.Buffer
	done   chan struct{}
	once   sync.Once
	sendMu sync.Mutex
	// needReady：交了牌组但座位还没坐满，等坐满再准备。
	needReady bool
}

func newBridgeRun(a *Adapter, ch uint32, name string, info seatInfo) *bridgeRun {
	return &bridgeRun{a: a, ch: ch, name: name, info: info, in: make(chan []byte, 256), done: make(chan struct{})}
}

// feed 收一帧通道数据（可能是半行、也可能是好几行）。
func (b *bridgeRun) feed(p []byte) {
	select {
	case b.in <- append([]byte(nil), p...):
	case <-b.done:
	}
}

func (b *bridgeRun) stop() { b.once.Do(func() { close(b.done) }) }

// send 往客人那边写一行。
func (b *bridgeRun) send(frame map[string]any) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	line := append(mustJSON(frame), '\n')
	_ = b.a.tn.SendFrame(context.Background(), b.ch, line)
}

// nextLine 等客人发来的下一整行 JSON。
func (b *bridgeRun) nextLine(ctx context.Context) (map[string]any, error) {
	for {
		if i := bytes.IndexByte(b.buf.Bytes(), '\n'); i >= 0 {
			line := append([]byte(nil), b.buf.Next(i+1)...)
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var m map[string]any
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			return m, nil
		}
		select {
		case p := <-b.in:
			b.buf.Write(p)
		case <-b.done:
			return nil, errors.New("通道关了")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (b *bridgeRun) reject(code, msg string) {
	b.send(map[string]any{"type": "join_rejected", "code": code, "message": msg, "protocolVersion": arcaneProtocolVersion})
}

func (b *bridgeRun) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-b.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	// 1. 客人的第一行必须是 join（和连独弈主机一样）。
	jctx, jcancel := context.WithTimeout(ctx, 30*time.Second)
	join, err := b.nextLine(jctx)
	jcancel()
	if err != nil {
		return err
	}
	if join["type"] != "join" {
		b.reject("invalid_join", "第一条消息必须是 join")
		return errors.New("第一行不是 join")
	}
	if v, _ := join["protocolVersion"].(float64); int(v) != arcaneProtocolVersion {
		b.reject("version_mismatch", fmt.Sprintf("双方版本不一致：主机 %d，客机 %v", arcaneProtocolVersion, join["protocolVersion"]))
		return errors.New("arcane 版本不一致")
	}
	// 独弈 0.4 起另附 printedDeck：每行「张数 牌名|系列|收藏编号」，收藏编号是客户端按自己的卡牌库查好的。
	// deck 是 .dck 原文，那里「牌名|系列|N」的 N 是 Forge 的画稿序号，当收藏编号用会交错牌；只在老客户端没附时退回它。
	deckText, _ := join["printedDeck"].(string)
	if strings.TrimSpace(deckText) == "" {
		deckText, _ = join["deck"].(string)
	}
	main, side, commanders := parseDeck(deckText)
	if len(main) == 0 {
		b.reject("missing_deck", "客机没有提交套牌")
		return errors.New("没有牌组")
	}
	// Hexproof 要每张牌都带系列和收藏编号（基本地也一样，2026-11 实测 3.4.0），缺了只回一句 invalid_deck。
	// 适配器没有牌库补不了，在这里先拦下、说清是哪几张。
	if missing := missingPrintings(main, side); len(missing) > 0 {
		b.reject("deck_not_legal", "Hexproof 要每张牌都指定印次（系列和收藏编号），这几张没有："+missing)
		return errors.New("牌组缺印次")
	}
	if name, _ := join["name"].(string); name != "" {
		b.name = name
	}

	// 2. 在 hub 上坐下、交牌组、准备。
	hctx, hcancel := context.WithTimeout(ctx, 20*time.Second)
	defer hcancel()
	hc, seat := b.info.pre, b.info.preSeat
	if hc == nil {
		hc, err = hub.Dial(hctx, b.a.cfg.HubURL, b.name, Version)
		if err != nil {
			b.reject("host_unreachable", "连不上 Hexproof："+err.Error())
			return err
		}
		jr, err := hc.Request(hctx, "room.join", map[string]any{"roomId": b.info.room.RoomID, "asSpectator": false, "password": b.info.password})
		if err != nil {
			hc.Close()
			b.reject(joinCode(err), "Hexproof 拒绝入座："+err.Error())
			return err
		}
		var j struct {
			Seat int `json:"seat"`
		}
		_ = jr.Decode(&j)
		seat = j.Seat
	}
	defer hc.Close()
	if _, err := hc.Request(hctx, "deck.select", hub.DeckSelect{Name: b.name, Format: b.info.room.Format, DeckFormat: b.info.room.DeckFormat,
		Commanders: commanders, Mainboard: main, Sideboard: side}); err != nil {
		b.reject("deck_not_legal", "Hexproof 不收这副牌："+err.Error())
		return err
	}
	// 座位没坐满之前 hub 不让准备（seats_not_filled，2026-11 实测 3.4.0）：排队开房时建房的人最先到，
	// 这时先记着，等房间快照显示坐满了再准备（见 onHub 的 room.snapshot）。
	b.needReady = false
	if _, err := hc.Request(hctx, "player.ready", map[string]any{"ready": true}); err != nil {
		var he *hub.HubError
		if !errors.As(err, &he) || he.Code != "seats_not_filled" {
			b.reject("deck_not_legal", "Hexproof 不让准备："+err.Error())
			return err
		}
		b.needReady = true
	}
	hcancel()

	gameType := "constructed"
	if b.info.room.Format == "edh" || b.info.room.Format == "duel" {
		gameType = "commander"
	}
	mode := "duel"
	if b.info.room.MaxSeats > 2 {
		mode = "freeForAll"
	}
	b.send(map[string]any{"type": "joined", "protocolVersion": arcaneProtocolVersion, "room": b.info.roomID,
		"gameType": gameType, "mode": mode, "seat": seat, "humanSeats": max(b.info.room.MaxSeats, 2),
		"deckWarning": nil, "sessionToken": "", "variants": []string{}, "reconnectWindowMs": 180000})

	var sess *bridge.Session
	sess = bridge.NewSession(seat, b.info.locale, bridge.Hooks{
		Send: b.send,
		Respond: func(r hub.Respond) error {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			_, err := hc.Request(rctx, "rules.respond", r)
			return err
		},
		Concede: func() error {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			// 自己认输时，hub 把 game.conceded 当作这条请求的回话发回来（带 id），不进推送流，要自己转给 onHub。
			e, err := hc.Request(rctx, "game.concede", nil)
			if err == nil {
				b.onHub(ctx, hc, sess, e)
			}
			return err
		},
		Degraded: func(kind, why string) { b.a.log.Info("bridge.degraded", "kind", kind, "why", why) },
	})

	// 3. 两头一起转：hub 推送 → 会话；客人指令 → 会话。会话不是并发安全的，统一在这一条协程里处理。
	hubEv := make(chan hub.Envelope, 64)
	go func() {
		for {
			e, err := hc.Next(ctx)
			if err != nil {
				close(hubEv)
				return
			}
			hubEv <- e
		}
	}()
	guestEv := make(chan map[string]any, 64)
	go func() {
		for {
			m, err := b.nextLine(ctx)
			if err != nil {
				close(guestEv)
				return
			}
			guestEv <- m
		}
	}()
	ping := time.NewTicker(5 * time.Second) // 独弈客机见过主机心跳后，20 秒没字节就当掉线
	defer ping.Stop()
	for {
		select {
		case e, ok := <-hubEv:
			if !ok {
				return hc.Err()
			}
			b.a.log.Debug("bridge.hub", "type", e.Type, "bytes", len(e.Payload))
			b.onHub(ctx, hc, sess, e)
		case m, ok := <-guestEv:
			if !ok {
				return errors.New("客人走了")
			}
			if m["type"] == "command" {
				var c bridge.Command
				if json.Unmarshal(mustJSON(m), &c) == nil {
					sess.OnCommand(c)
				}
			}
		case <-ping.C:
			b.send(map[string]any{"type": "ping"})
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func joinCode(err error) string {
	var he *hub.HubError
	if errors.As(err, &he) {
		switch he.Code {
		case "ErrWrongPassword", "wrong_password":
			return "bad_password"
		case "ErrRoomFull", "room_full":
			return "busy"
		}
	}
	return "join_failed"
}

func (b *bridgeRun) onHub(ctx context.Context, hc *hub.Conn, sess *bridge.Session, e hub.Envelope) {
	switch e.Type {
	case "room.snapshot":
		if !b.needReady {
			return
		}
		var rs struct {
			Seats []struct {
				Occupied bool `json:"occupied"`
			} `json:"seats"`
		}
		_ = e.Decode(&rs)
		full := len(rs.Seats) > 0
		for _, s := range rs.Seats {
			full = full && s.Occupied
		}
		if full {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := hc.Request(rctx, "player.ready", map[string]any{"ready": true})
			cancel()
			if err == nil {
				b.needReady = false
			} else {
				b.a.log.Warn("bridge.ready", "err", err)
			}
		}
	case "match.load_required":
		var p struct {
			LoadID int64 `json:"loadId"`
		}
		_ = e.Decode(&p)
		// 必须带 id（2026-11 实测 3.4.0：不带 id 回 invalid_message，对局就开不了）。
		// 回话要等 Forge 开局，可能好几秒：放到另一条协程里等，主循环照常发心跳。
		// 开局失败（比如某张牌的印次 Forge 不认）时，最后一个加载完的人是在这条回话里收到 rules_unavailable 的，
		// 要转告独弈玩家，不然他会一直干等。
		go func() {
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if _, err := hc.Request(rctx, "client.load_complete", map[string]any{"loadId": p.LoadID}); err != nil {
				b.a.log.Warn("bridge.load_complete", "err", err)
				b.send(map[string]any{"type": "error", "code": "remote_start_failed", "message": "Hexproof 开不了这一局：" + err.Error()})
			}
		}()
	case "match.started":
		b.send(map[string]any{"type": "match_started", "networkRole": "guest", "matchStructure": b.info.room.MatchMode})
	case "rules.snapshot":
		var s hub.Snapshot
		if e.Decode(&s) == nil {
			sess.OnSnapshot(s)
		}
	case "rules.prompt":
		var p hub.Prompt
		if e.Decode(&p) == nil {
			sess.OnPrompt(p)
		}
	case "game.conceded":
		var p struct {
			MatchFinished bool `json:"matchFinished"`
		}
		_ = e.Decode(&p)
		if p.MatchFinished {
			b.send(map[string]any{"type": "match_over", "winner": nil, "gamesPlayed": 1})
		}
	case "error":
		var p hub.ErrorPayload
		_ = e.Decode(&p)
		b.send(map[string]any{"type": "error", "code": "remote_" + p.Code, "message": p.Message})
	}
}
