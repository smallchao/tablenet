// SPDX-License-Identifier: MIT

package xmage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"tablenet/adapters/xmage/bridge"
	"tablenet/pkg/proto"
)

// arcaneProtocolVersion 是独弈联机握手的版本号（独弈 mtg-engine-sidecar 的 net::PROTOCOL_VERSION）。
const arcaneProtocolVersion = 3

// bridgeRun 是一位桥接玩家：一头是 Tablenet 对局通道（JSON Lines，讲 arcane.v0），一头是他在 XMage 上的会话。
type bridgeRun struct {
	a        *Adapter
	ch       uint32
	playerID string
	name     string
	info     seatInfo
	sess     string // 驱动里的会话名
	in       chan []byte
	buf      bytes.Buffer
	done     chan struct{}
	once     sync.Once
	sendMu   sync.Mutex

	game  string // 当前这一局的 XMage 游戏编号
	games int
	deck  deckList
}

func newBridgeRun(a *Adapter, ch uint32, playerID, name string, info seatInfo) *bridgeRun {
	return &bridgeRun{a: a, ch: ch, playerID: playerID, name: name, info: info, sess: fmt.Sprintf("%s.p%d", info.srv.cfg.ID, ch),
		in: make(chan []byte, 256), done: make(chan struct{})}
}

func (b *bridgeRun) feed(p []byte) {
	select {
	case b.in <- append([]byte(nil), p...):
	case <-b.done:
	}
}

func (b *bridgeRun) stop() { b.once.Do(func() { close(b.done) }) }

func (b *bridgeRun) send(frame map[string]any) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	_ = b.a.tn.SendFrame(context.Background(), b.ch, append(mustJSON(frame), '\n'))
}

func (b *bridgeRun) nextLine(ctx context.Context) (map[string]any, error) {
	for {
		if i := bytes.IndexByte(b.buf.Bytes(), '\n'); i >= 0 {
			line := bytes.TrimSpace(append([]byte(nil), b.buf.Next(i+1)...))
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

// xmageUser：XMage 的用户名只许小写字母、数字、下划线，3–14 位，而且整台服务器不能重名。
// 取展示名里能用的字符，再接一段随机后缀。
func xmageUser(display string) string {
	keep := []rune{}
	for _, r := range strings.ToLower(display) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			keep = append(keep, r)
		}
		if len(keep) == 8 {
			break
		}
	}
	prefix := string(keep)
	if prefix == "" {
		prefix = "tn"
	}
	suffix := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(proto.B64(proto.RandomBytes(6))[4:]))
	u := prefix + "_" + suffix
	if len(u) > 14 {
		u = u[:14]
	}
	return u
}

func (b *bridgeRun) call(ctx context.Context, op string, body map[string]any) (map[string]any, error) {
	if body == nil {
		body = map[string]any{}
	}
	body["s"] = b.sess
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return b.info.srv.drv.Call(rctx, op, body)
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
	deckText, _ := join["deck"].(string)
	b.deck = parseDeck(deckText)
	if len(b.deck.Main) == 0 {
		b.reject("missing_deck", "客机没有提交套牌")
		return errors.New("没有牌组")
	}
	if name, _ := join["name"].(string); name != "" {
		b.name = name
	}

	// 2. 开一个 XMage 会话，坐下、交牌组。
	drv := b.info.srv.drv
	events := drv.Subscribe(b.sess)
	defer drv.Unsubscribe(b.sess)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		if b.game != "" {
			_, _ = drv.Call(dctx, "quit_match", map[string]any{"s": b.sess, "game": b.game})
		}
		_, _ = drv.Call(dctx, "disconnect", map[string]any{"s": b.sess})
	}()
	srv := b.info.srv
	if _, err := b.call(ctx, "connect", map[string]any{"host": srv.host, "port": srv.port, "user": xmageUser(b.name)}); err != nil {
		if isAuthError(err) {
			// 这台服务器其实要注册：以后它的桌子只做交接（适配器不经手玩家的 XMage 密码）
			srv.noBridge()
			b.reject("account_required", srv.cfg.Name+" 要用注册过的 XMage 账号登录，只能在 XMage 客户端里用自己的账号打："+err.Error())
			return err
		}
		b.reject("host_unreachable", "连不上 "+srv.cfg.Name+"："+err.Error())
		return err
	}
	if _, err := b.call(ctx, "join_table", map[string]any{"table": b.info.table.ID, "name": xmageUser(b.name),
		"deck": b.deck, "password": b.info.password}); err != nil {
		code := "deck_not_legal"
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "password") {
			code = "bad_password"
		}
		b.reject(code, msg)
		return err
	}
	seats := max(len(b.info.table.Seats), 2)
	gameType := "constructed"
	if isCommanderType(b.info.table.GameType) {
		gameType = "commander"
	}
	mode := "duel"
	if seats > 2 {
		mode = "freeForAll"
	}
	b.send(map[string]any{"type": "joined", "protocolVersion": arcaneProtocolVersion, "room": b.info.localID,
		"gameType": gameType, "mode": mode, "seat": 0, "humanSeats": seats,
		"deckWarning": nil, "sessionToken": "", "variants": []string{}, "reconnectWindowMs": 180000})

	sess := bridge.NewSession(b.info.locale, bridge.Hooks{
		Send: b.send,
		Answer: func(op string, v any) error {
			if b.game == "" {
				return errors.New("对局还没开始")
			}
			_, err := b.call(ctx, op, map[string]any{"game": b.game, "v": v})
			return err
		},
		Degraded: func(kind, why string) { b.a.log.Info("bridge.degraded", "kind", kind, "why", why) },
	})

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
		case ev := <-events:
			if ev.M == "lost" {
				b.send(map[string]any{"type": "error", "code": "remote_lost", "message": "和 XMage 服务器断开了"})
				return errors.New("XMage 会话断了")
			}
			if over := b.onXMage(ctx, sess, ev); over {
				return nil
			}
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

// onXMage 处理一个 XMage 回调。返回 true 表示这一盘打完了。
func (b *bridgeRun) onXMage(ctx context.Context, sess *bridge.Session, ev Event) bool {
	switch ev.M {
	case "START_GAME":
		var d struct {
			Game string `json:"game"`
		}
		_ = json.Unmarshal(ev.D, &d)
		first := b.game == ""
		b.game = d.Game
		b.games++
		if _, err := b.call(ctx, "join_game", map[string]any{"game": d.Game}); err != nil {
			b.a.log.Warn("bridge.join_game", "err", err)
		}
		if first {
			structure := "bo1"
			if b.info.match != nil && b.info.match.structure != "" {
				structure = b.info.match.structure
			}
			b.send(map[string]any{"type": "match_started", "networkRole": "guest", "matchStructure": structure})
		}
	case "END_GAME_INFO":
		var d struct {
			Wins int `json:"wins"`
		}
		_ = json.Unmarshal(ev.D, &d)
		if sess.GameOver(ev.D, b.games) {
			b.send(map[string]any{"type": "match_over", "winner": nil, "gamesPlayed": b.games})
			// 一对一时对手赢的局数＝打了几局减去我赢的（不计平局）
			b.a.report(b.info.match, b.info.localID, b.playerID, d.Wins, b.games-d.Wins)
			return true
		}
	case "SIDEBOARD":
		// 三局两胜的局间换备：先原样交回同一副牌（独弈的换备牌墙还没接到桥接上）。
		var d struct {
			Table string `json:"table"`
		}
		_ = json.Unmarshal(ev.D, &d)
		if _, err := b.call(ctx, "submit_deck", map[string]any{"table": d.Table, "deck": b.deck}); err != nil {
			b.a.log.Warn("bridge.sideboard", "err", err)
		}
	case "driver_error":
		b.a.log.Warn("bridge.driver_error", "data", string(ev.D))
	default:
		sess.OnCallback(ev.M, ev.D)
	}
	return false
}
