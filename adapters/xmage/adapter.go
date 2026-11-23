// SPDX-License-Identifier: MIT

// Package xmage 把一台 XMage 服务器接进 Tablenet 节点：
//
//   - 大厅镜像：定时取 XMage 主房间里等人的桌子，发布到节点的房间表；
//   - 交接：玩家要去 XMage 客户端打时，给服务器地址、桌名（和密码）；
//   - 桥接：玩家在独弈里打 XMage 上的局，适配器替他开一个 XMage 会话、坐下、交牌组，在对局通道里冒充独弈主机；
//   - 排队开桌：排队配到 XMage 时，大厅机器人建一张带密码的桌（不坐下），坐满了替大家开打。
//
// XMage 只能用 Java 接（JBoss Remoting＋Java 序列化），所以连 XMage 的那一小段是 driver/ 下的 Java 驱动，
// 本包起它当子进程、用标准输入输出讲 JSON Lines。设计见 spec/07-首批适配器.md 与 spec/08-桥接模式.md。
package xmage

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"tablenet/pkg/client"
	"tablenet/pkg/proto"
)

const (
	PlatformID = "xmage"
	Codec      = "arcane.v0"
	Version    = "0.1.0"
)

type Config struct {
	NodeURL      string        // Tablenet 节点，ws://…/tablenet/1
	Server       string        // 适配器连 XMage 用的地址，host:port
	PublicServer string        // 交接时告诉玩家的地址（玩家的 XMage 客户端要填它），默认同 Server
	Homepage     string        // 平台主页，必须 https
	LobbyUser    string        // 大厅机器人在 XMage 上的用户名
	PollEvery    time.Duration // 多久取一次桌子表
	Driver       DriverConfig
}

func (c *Config) defaults() {
	if c.Server == "" {
		c.Server = "127.0.0.1:17171"
	}
	if c.PublicServer == "" {
		c.PublicServer = c.Server
	}
	if c.Homepage == "" {
		c.Homepage = "https://xmage.today"
	}
	if c.LobbyUser == "" {
		c.LobbyUser = "tablenet"
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
			"platformId": PlatformID, "name": "XMage", "homepage": cfg.Homepage,
			"joinKinds": []string{"handoff", "bridge"}, "formats": Formats(),
			"gameTypes": []string{"duel", "ffa"}, "structures": []string{"bo1", "bo3"}, "codecs": []string{Codec},
			"accountLink": "none", "terms": cfg.Homepage,
		},
		"client": map[string]string{"name": "tablenet-adapter-xmage", "version": Version},
	}
}

// Table 是驱动给的一张 XMage 桌子。
type Table struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Controller string `json:"controller"`
	GameType   string `json:"gameType"`
	DeckType   string `json:"deckType"`
	State      string `json:"state"`
	Passworded bool   `json:"passworded"`
	Spectators bool   `json:"spectators"`
	Tournament bool   `json:"tournament"`
	Limited    bool   `json:"limited"`
	Seats      []struct {
		Player string `json:"player"`
		Name   string `json:"name"`
		Type   string `json:"type"`
	} `json:"seats"`
}

func (t Table) taken() (players, ai int) {
	for _, s := range t.Seats {
		if s.Name == "" {
			continue
		}
		if strings.HasPrefix(s.Type, "Computer") {
			ai++
		} else {
			players++
		}
	}
	return
}

type Adapter struct {
	cfg  Config
	log  *slog.Logger
	tn   *client.Client
	drv  *Driver
	host string
	port int

	mu        sync.Mutex
	published map[string]Table   // XMage 桌子编号 → 发布出去的样子
	waiting   map[string]seatInfo // playerId → 等 channel.opened 的桥接入座
	bridges   map[uint32]*bridgeRun
	matches   map[string]*matchTable // 排队开的桌：XMage 桌子编号 → 这一局
}

type seatInfo struct {
	localID  string // 节点上的房间 localId
	table    Table
	password string
	locale   string
	match    *matchTable
}

// matchTable：排队配对后机器人建的桌。
type matchTable struct {
	id        string
	matchID   string
	localID   string
	tableID   string
	tableName string
	password  string
	structure string
	players   []string // playerId，按配对顺序
	started   bool
}

// Run 起驱动、连 XMage 和节点，一直跑到 ctx 结束或某条连接断开。
func Run(ctx context.Context, cfg Config, key ed25519.PrivateKey, log *slog.Logger) error {
	cfg.defaults()
	host, portStr, err := net.SplitHostPort(cfg.Server)
	if err != nil {
		return fmt.Errorf("XMage 地址要写成 host:port：%w", err)
	}
	port, _ := strconv.Atoi(portStr)
	drv, err := StartDriver(cfg.Driver)
	if err != nil {
		return err
	}
	defer drv.Close()
	a := &Adapter{cfg: cfg, log: log, drv: drv, host: host, port: port, published: map[string]Table{},
		waiting: map[string]seatInfo{}, bridges: map[uint32]*bridgeRun{}, matches: map[string]*matchTable{}}

	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	hello, err := drv.Call(hctx, "hello", nil)
	cancel()
	if err != nil {
		return fmt.Errorf("Java 驱动没应：%w", err)
	}
	// 本地牌库（按牌名补系列和收藏编号用）：第一次要从 XMage 的 jar 建库，几十秒。
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	cards, err := drv.Call(cctx, "cards", nil)
	cancel()
	if err != nil {
		return fmt.Errorf("建不了本地牌库：%w", err)
	}
	lobbyEv := drv.Subscribe("lobby")
	lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := drv.Call(lctx, "connect", map[string]any{"s": "lobby", "host": host, "port": port, "user": cfg.LobbyUser})
	cancel()
	if err != nil {
		return err
	}
	// 客户端版本必须和服务器一样，不然 XMage 根本不让登录；能登进来就是对得上。
	log.Info("xmage.online", "server", cfg.Server, "serverVersion", conn["serverVersion"], "clientVersion", hello["clientVersion"], "cards", cards["cards"])

	tn, err := client.Dial(ctx, cfg.NodeURL, key, Hello(cfg))
	if err != nil {
		return err
	}
	defer tn.Close()
	a.tn = tn

	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	go func() {
		defer cancelAll()
		a.mirrorLobby(ctx, lobbyEv)
	}()
	go func() {
		select {
		case <-drv.Done():
			cancelAll()
		case <-ctx.Done():
		}
	}()
	for {
		m, err := tn.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				if e := drv.Err(); e != nil {
					return e
				}
				return errors.New("连 XMage 的大厅会话断了")
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

func (a *Adapter) mirrorLobby(ctx context.Context, lobbyEv <-chan Event) {
	t := time.NewTicker(a.cfg.PollEvery)
	defer t.Stop()
	for {
		if err := a.syncTables(ctx); err != nil {
			a.log.Warn("xmage.tables", "err", err)
			if errors.Is(err, context.Canceled) {
				return
			}
		}
		select {
		case <-t.C:
		case ev := <-lobbyEv:
			if ev.M == "lost" {
				a.log.Error("xmage.lobby_lost")
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (a *Adapter) syncTables(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := a.drv.Call(rctx, "tables", map[string]any{"s": "lobby"})
	if err != nil {
		return err
	}
	var tables []Table
	if err := decodeAs(res, "tables", &tables); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, t := range tables {
		a.watchMatch(ctx, t)
		a.mu.Lock()
		_, listed := a.published[t.ID]
		_, ours := a.matches[t.ID]
		a.mu.Unlock()
		// 新桌只发布等人的；已经发布过的桌开打了不撤，改成「对局中」挂着，XMage 上整张没了才撤
		// （开打就撤会让节点以为房没了，把桥接通道一起关掉）。
		waiting := t.State == "WAITING"
		if ours || t.Tournament || t.Limited || (!waiting && !listed) {
			continue
		}
		format, gameType := tablenetFormat(t.GameType, t.DeckType)
		if format == "" {
			continue
		}
		seen[t.ID] = true
		a.mu.Lock()
		old, had := a.published[t.ID]
		a.published[t.ID] = t
		a.mu.Unlock()
		if had && sameTable(old, t) {
			continue
		}
		players, ai := t.taken()
		up := map[string]any{
			"localId": t.ID, "title": t.Name, "format": format, "gameType": gameType,
			"seats":     map[string]int{"total": max(len(t.Seats), 2), "taken": players + ai, "ai": ai},
			"structure": "bo1", "hasPassword": t.Passworded, "visibility": "public",
			"spectate": map[string]bool{"allowed": t.Spectators, "seeHands": false},
			"join":     "bridge", "codec": Codec, // 解不了 arcane.v0 的玩家，节点会自动退成交接
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

func sameTable(a, b Table) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// watchMatch：排队开的桌坐满了，由机器人（桌主）开打。XMage 只认桌主按开始，而且要所有座位都坐满。
func (a *Adapter) watchMatch(ctx context.Context, t Table) {
	a.mu.Lock()
	m := a.matches[t.ID]
	a.mu.Unlock()
	if m == nil || m.started {
		return
	}
	if t.State != "READY_TO_START" {
		return
	}
	m.started = true
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := a.drv.Call(rctx, "start_match", map[string]any{"s": "lobby", "table": t.ID}); err != nil {
			a.log.Warn("xmage.start_match", "table", t.ID, "err", err)
			m.started = false
		}
	}()
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

func (a *Adapter) handoffNote(passworded bool) string {
	note := "打开 XMage 客户端，连这台服务器，在主房间（Main Room）找这张桌子"
	if passworded {
		note += "（这张桌要密码）"
	}
	return note
}

func (a *Adapter) onNode(ctx context.Context, e proto.Envelope) {
	switch e.Type {
	case "venue.seat.request":
		var b seatRequest
		_ = e.Decode(&b)
		a.mu.Lock()
		t, ok := a.published[b.LocalID]
		a.mu.Unlock()
		if !ok {
			_ = a.tn.SendEnv(ctx, proto.Error(e.ID, proto.CodeNoRoom, "XMage 上没有这张桌子了"))
			return
		}
		if b.Bridge && b.As == "player" {
			a.mu.Lock()
			a.waiting[b.Player.PlayerID] = seatInfo{localID: b.LocalID, table: t, password: b.Password, locale: b.Player.Locale}
			a.mu.Unlock()
			_ = a.tn.Reply(ctx, e.ID, "venue.seat.bridged", map[string]any{})
			return
		}
		connect := map[string]any{"client": "xmage", "server": a.cfg.PublicServer, "room": t.Name}
		_ = a.tn.Reply(ctx, e.ID, "venue.seat.granted", map[string]any{"connect": connect, "code": t.Name, "note": a.handoffNote(t.Passworded)})
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
		if !ok || info.localID != b.LocalID {
			_ = a.tn.Notify(ctx, "channel.close", map[string]any{"channel": b.Channel, "code": "no_seat"})
			return
		}
		br := newBridgeRun(a, b.Channel, b.Player.PlayerID, b.Player.DisplayName, info)
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

// createForMatch：排队配到 XMage。XMage 建桌的人不必坐下（和 Hexproof 不同），所以由大厅机器人建一张
// 带密码的桌，桥接的玩家由适配器替他坐下，其余玩家拿服务器地址、桌名和密码自己去 XMage 客户端里坐。
// 坐满之后机器人开打（见 watchMatch）。
func (a *Adapter) createForMatch(ctx context.Context, e proto.Envelope) {
	var b struct {
		MatchID   string `json:"matchId"`
		Format    string `json:"format"`
		GameType  string `json:"gameType"`
		Structure string `json:"structure"`
		Players   []struct {
			PlayerID    string `json:"playerId"`
			DisplayName string `json:"displayName"`
			Locale      string `json:"locale"`
			Bridge      bool   `json:"bridge"`
		} `json:"players"`
	}
	_ = e.Decode(&b)
	fail := func(msg string) { _ = a.tn.SendEnv(ctx, proto.Error(e.ID, proto.CodeVenueUnavailable, msg)) }
	gt, deckType := xmageTable(b.Format, b.GameType)
	if gt == "" {
		fail("XMage 开不了这个赛制：" + b.Format)
		return
	}
	wins := 1
	if b.Structure == "bo3" {
		wins = 2
	}
	password := strings.ToLower(proto.B64(proto.RandomBytes(6))[4:])
	name := "Tablenet " + b.MatchID
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := a.drv.Call(rctx, "create_table", map[string]any{"s": "lobby", "name": name, "gameType": gt, "deckType": deckType,
		"seats": len(b.Players), "winsNeeded": wins, "password": password, "spectators": false})
	if err != nil {
		fail("XMage 建桌失败：" + err.Error())
		return
	}
	var t Table
	_ = decodeAs(res, "table", &t)
	m := &matchTable{matchID: b.MatchID, localID: "m-" + b.MatchID, tableID: t.ID, tableName: name, password: password, structure: b.Structure}
	seats := []map[string]any{}
	a.mu.Lock()
	a.matches[t.ID] = m
	for _, p := range b.Players {
		m.players = append(m.players, p.PlayerID)
		if p.Bridge {
			a.waiting[p.PlayerID] = seatInfo{localID: m.localID, table: t, password: password, locale: p.Locale, match: m}
			seats = append(seats, map[string]any{"playerId": p.PlayerID, "bridge": true})
		} else {
			seats = append(seats, map[string]any{"playerId": p.PlayerID, "connect": map[string]any{
				"client": "xmage", "server": a.cfg.PublicServer, "room": name, "password": password,
			}, "note": "打开 XMage 客户端，连这台服务器，在主房间找这张桌子，输入密码入座"})
		}
	}
	a.mu.Unlock()
	_ = a.tn.Reply(ctx, e.ID, "venue.room.created", map[string]any{"localId": m.localID, "seats": seats})
	// 10 分钟还没坐满就撤桌（机器人一直挂着桌主，桌子不会自己消失）。
	go func() {
		select {
		case <-time.After(10 * time.Minute):
		case <-ctx.Done():
			return
		}
		if !m.started {
			dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = a.drv.Call(dctx, "remove_table", map[string]any{"s": "lobby", "table": t.ID})
			a.mu.Lock()
			delete(a.matches, t.ID)
			a.mu.Unlock()
		}
	}()
}

// report：桥接玩家这一盘打完，以平台的名义报战绩（spec/02「结果回报」）。
func (a *Adapter) report(m *matchTable, localID, me string, wins, loses int) {
	if a.tn == nil {
		return
	}
	players := []map[string]any{{"playerId": me, "games": wins}}
	if m != nil && len(m.players) == 2 {
		for _, p := range m.players {
			if p != me {
				players = append(players, map[string]any{"playerId": p, "games": loses})
			}
		}
	}
	body := map[string]any{"localId": localID, "players": players, "reason": "finished"}
	if m != nil {
		body["matchId"] = m.matchID
	}
	_ = a.tn.Notify(context.Background(), "venue.result", body)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
