// SPDX-License-Identifier: MIT

// Package xmage 把别人已经在运营的 XMage 服务器（例如 xmage.de）接进 Tablenet 节点。
// 一个适配器可以同时接好几台；它在每台上挂一个大厅机器人账号，不需要、也不自带 XMage 服务器：
//
//   - 大厅镜像：定时取每台服务器主房间里等人的桌子，发布到节点的房间表；
//   - 交接：玩家要去 XMage 客户端打时，给服务器地址、桌名（和密码），玩家用自己的 XMage 账号进；
//   - 桥接：玩家在独弈里打 XMage 上的局，适配器替他开一个 XMage 会话、坐下、交牌组，在对局通道里冒充独弈主机。
//     只有允许不注册登录的服务器才能桥接（适配器不经手玩家的 XMage 密码）；要注册的服务器只做交接；
//   - 排队开桌：排队配到 XMage 时，大厅机器人在选定的那台上建一张带密码的桌（不坐下），坐满了替大家开打。
//
// XMage 只能用 Java 接（JBoss Remoting＋Java 序列化），所以连 XMage 的那一小段是 driver/ 下的 Java 驱动，
// 本包起它当子进程、用标准输入输出讲 JSON Lines。XMage 登录要求客户端和服务器同一版，所以每台服务器
// 可以指向各自版本的 XMage jar（同一版本的共用一个驱动进程）。设计见 spec/07 与 spec/08。
package xmage

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"regexp"
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
	Version    = "0.2.0"
)

// ServerConfig 是要接入的一台 XMage 服务器。
type ServerConfig struct {
	ID       string `json:"id"`       // 短名，进房间编号，[a-z0-9]，例如 "de"
	Name     string `json:"name"`     // 给玩家看的名字，例如 "xmage.de"
	Address  string `json:"address"`  // 适配器连它用的 host:port
	Public   string `json:"public"`   // 交接时告诉玩家的 host:port，默认同 Address
	User     string `json:"user"`     // 大厅机器人的用户名（3–14 位小写字母数字下划线），默认 tablenet
	Password string `json:"password"` // 机器人的密码；服务器要注册时必填
	Lib      string `json:"lib"`      // 和这台同一版本的 XMage lib 目录；空＝用 Config.Driver.XMageLib
	// Accounts：这台服务器要不要注册账号。"optional"（默认，玩家可以不注册直接登录，能桥接）
	// 或 "required"（只能用注册过的账号，这台只做交接）。
	Accounts string `json:"accounts"`
	// Queue：排队配到 XMage 时在这台开桌。只该有一台设它；都没设时用第一台。
	Queue bool `json:"queue"`
}

type Config struct {
	NodeURL  string         // Tablenet 节点，ws://…/tablenet/1
	Servers  []ServerConfig // 要接入的 XMage 服务器
	Homepage string         // 平台主页，必须 https
	// PollEvery：多久取一次各台的桌子表。别人的服务器，别取得太勤，默认 10 秒。
	PollEvery time.Duration
	Driver    DriverConfig // Java 驱动；WorkDir 下每个 XMage 版本一个子目录放本地牌库

	// 只接一台时的简写（老配置）：Servers 为空时用它们拼一台，ID 为 "x"。
	Server, PublicServer, LobbyUser string
}

var serverIDRE = regexp.MustCompile(`^[a-z0-9]{1,12}$`)

func (c *Config) defaults() error {
	if len(c.Servers) == 0 {
		if c.Server == "" {
			c.Server = "127.0.0.1:17171"
		}
		c.Servers = []ServerConfig{{ID: "x", Name: c.Server, Address: c.Server, Public: c.PublicServer, User: c.LobbyUser}}
	}
	seen := map[string]bool{}
	for i := range c.Servers {
		s := &c.Servers[i]
		if !serverIDRE.MatchString(s.ID) || seen[s.ID] {
			return fmt.Errorf("XMage 服务器的 id %q 要是 1–12 位小写字母数字、而且不重复", s.ID)
		}
		seen[s.ID] = true
		if _, _, err := net.SplitHostPort(s.Address); err != nil {
			return fmt.Errorf("XMage 服务器 %s 的地址要写成 host:port：%w", s.ID, err)
		}
		if s.Public == "" {
			s.Public = s.Address
		}
		if s.Name == "" {
			s.Name = s.Address
		}
		if s.User == "" {
			s.User = "tablenet"
		}
		if s.Lib == "" {
			s.Lib = c.Driver.XMageLib
		}
		if s.Accounts == "" {
			s.Accounts = "optional"
		}
		if s.Accounts == "required" && s.Password == "" {
			return fmt.Errorf("XMage 服务器 %s 要注册账号，要给机器人配 password", s.ID)
		}
	}
	if c.Homepage == "" {
		c.Homepage = "https://xmage.today"
	}
	if c.PollEvery == 0 {
		c.PollEvery = 10 * time.Second
	}
	return nil
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

// server 是接入的一台 XMage 服务器的运行状态。
type server struct {
	cfg  ServerConfig
	drv  *Driver
	host string
	port int
	// bridgeOK：玩家能不能不注册登录（能才桥接）。配置说 optional，但实际登录被拒时会改成 false。
	mu       sync.Mutex
	bridgeOK bool
	online   bool // 大厅机器人现在登着
}

func (s *server) setOnline(v bool) {
	s.mu.Lock()
	s.online = v
	s.mu.Unlock()
}

func (s *server) isOnline() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.online
}

func (s *server) lobby() string { return s.cfg.ID + ".lobby" }

func (s *server) canBridge() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridgeOK
}

func (s *server) noBridge() {
	s.mu.Lock()
	s.bridgeOK = false
	s.mu.Unlock()
}

// localID：节点上的房间编号 = <服务器 id>.<XMage 桌子编号>。
func localID(srv, table string) string { return srv + "." + table }

type published struct {
	srv   *server
	table Table
	join  string
}

type Adapter struct {
	cfg     Config
	log     *slog.Logger
	tn      *client.Client
	servers []*server

	mu        sync.Mutex
	published map[string]published // localId → 发布出去的样子
	waiting   map[string]seatInfo  // playerId → 等 channel.opened 的桥接入座
	bridges   map[uint32]*bridgeRun
	matches   map[string]*matchTable // 排队开的桌：localId（服务器.桌子）→ 这一局
}

type seatInfo struct {
	localID  string // 节点上的房间 localId
	srv      *server
	table    Table
	password string
	locale   string
	match    *matchTable
}

// matchTable：排队配对后机器人建的桌。
type matchTable struct {
	matchID   string
	localID   string // 节点上的房间 localId（m-<matchId>）
	srv       *server
	tableID   string
	tableName string
	password  string
	structure string
	players   []string // playerId，按配对顺序
	started   bool
}

// Run 起驱动、连各台 XMage 和节点，一直跑到 ctx 结束或节点连接断开。
// 某一台 XMage 连不上或断了只影响那一台：记日志、30 秒后重连，别的照常。
func Run(ctx context.Context, cfg Config, key ed25519.PrivateKey, log *slog.Logger) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	a := &Adapter{cfg: cfg, log: log, published: map[string]published{},
		waiting: map[string]seatInfo{}, bridges: map[uint32]*bridgeRun{}, matches: map[string]*matchTable{}}

	// 同一个 XMage 版本（同一个 lib 目录）共用一个驱动进程；牌库目录按版本分开（H2 一个目录只能一个进程开）。
	drivers := map[string]*Driver{}
	for _, sc := range cfg.Servers {
		drv := drivers[sc.Lib]
		if drv == nil {
			dc := cfg.Driver
			dc.XMageLib = sc.Lib
			if dc.WorkDir != "" && len(drivers) > 0 {
				dc.WorkDir = filepath.Join(dc.WorkDir, "v"+strconv.Itoa(len(drivers)+1))
			}
			d, err := StartDriver(dc)
			if err != nil {
				return err
			}
			defer d.Close()
			cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			hello, err := d.Call(cctx, "hello", nil)
			if err == nil {
				// 本地牌库（按牌名补系列和收藏编号用）：第一次要从 XMage 的 jar 建库，几十秒。
				_, err = d.Call(cctx, "cards", nil)
			}
			cancel()
			if err != nil {
				return fmt.Errorf("Java 驱动（%s）起不来：%w", sc.Lib, err)
			}
			log.Info("xmage.driver", "lib", sc.Lib, "clientVersion", hello["clientVersion"])
			drivers[sc.Lib], drv = d, d
		}
		host, portStr, _ := net.SplitHostPort(sc.Address)
		port, _ := strconv.Atoi(portStr)
		a.servers = append(a.servers, &server{cfg: sc, drv: drv, host: host, port: port, bridgeOK: sc.Accounts != "required"})
	}

	tn, err := client.Dial(ctx, cfg.NodeURL, key, Hello(cfg))
	if err != nil {
		return err
	}
	defer tn.Close()
	a.tn = tn

	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	for _, s := range a.servers {
		go a.keepServer(ctx, s)
	}
	for _, d := range drivers {
		go func(d *Driver) {
			select {
			case <-d.Done():
				log.Error("xmage.driver_exit", "err", d.Err())
				cancelAll()
			case <-ctx.Done():
			}
		}(d)
	}
	for {
		m, err := tn.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				for _, d := range drivers {
					if e := d.Err(); e != nil {
						return e
					}
				}
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

// keepServer：登录一台 XMage、镜像它的大厅；断了 30 秒后重来。
func (a *Adapter) keepServer(ctx context.Context, s *server) {
	for ctx.Err() == nil {
		ev := s.drv.Subscribe(s.lobby())
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		body := map[string]any{"s": s.lobby(), "host": s.host, "port": s.port, "user": s.cfg.User}
		if s.cfg.Password != "" {
			body["password"] = s.cfg.Password
		}
		conn, err := s.drv.Call(lctx, "connect", body)
		cancel()
		if err != nil {
			// 客户端版本和服务器不一样时 XMage 不让登录，错误文字里会写明两边的版本。
			a.log.Error("xmage.connect", "server", s.cfg.ID, "address", s.cfg.Address, "err", err)
		} else {
			a.probeAnonymous(ctx, s)
			s.setOnline(true)
			a.log.Info("xmage.online", "server", s.cfg.ID, "address", s.cfg.Address, "serverVersion", conn["serverVersion"], "bridge", s.canBridge())
			a.mirrorLobby(ctx, s, ev)
			s.setOnline(false)
			a.dropServer(ctx, s)
		}
		s.drv.Unsubscribe(s.lobby())
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
		}
	}
}

// probeAnonymous：配置说这台不用注册时，用一个临时名字试登一次（马上退出）。被拒就改成只做交接，
// 免得发布成桥接房、等第一位玩家坐下时才碰壁。
func (a *Adapter) probeAnonymous(ctx context.Context, s *server) {
	if !s.canBridge() {
		return
	}
	name := s.cfg.ID + ".probe"
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := s.drv.Call(pctx, "connect", map[string]any{"s": name, "host": s.host, "port": s.port, "user": xmageUser("tablenet")})
	_, _ = s.drv.Call(pctx, "disconnect", map[string]any{"s": name})
	if isAuthError(err) {
		s.noBridge()
		a.log.Warn("xmage.accounts_required", "server", s.cfg.ID, "err", err)
	}
}

// dropServer：一台服务器断了，撤下它的全部房间。
func (a *Adapter) dropServer(ctx context.Context, s *server) {
	a.mu.Lock()
	gone := []string{}
	for id, p := range a.published {
		if p.srv == s {
			gone = append(gone, id)
			delete(a.published, id)
		}
	}
	a.mu.Unlock()
	for _, id := range gone {
		_ = a.tn.Notify(ctx, "venue.room.remove", map[string]string{"localId": id})
	}
}

// ---- 大厅镜像 ----

func (a *Adapter) mirrorLobby(ctx context.Context, s *server, lobbyEv <-chan Event) {
	t := time.NewTicker(a.cfg.PollEvery)
	defer t.Stop()
	for {
		if err := a.syncTables(ctx, s); err != nil {
			a.log.Warn("xmage.tables", "server", s.cfg.ID, "err", err)
			if errors.Is(err, context.Canceled) {
				return
			}
		}
		select {
		case <-t.C:
		case ev := <-lobbyEv:
			if ev.M == "lost" {
				a.log.Error("xmage.lobby_lost", "server", s.cfg.ID)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (a *Adapter) syncTables(ctx context.Context, s *server) error {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := s.drv.Call(rctx, "tables", map[string]any{"s": s.lobby()})
	if err != nil {
		return err
	}
	var tables []Table
	if err := decodeAs(res, "tables", &tables); err != nil {
		return err
	}
	join := "handoff"
	if s.canBridge() {
		join = "bridge" // 解不了 arcane.v0 的玩家，节点会自动退成交接
	}
	seen := map[string]bool{}
	for _, t := range tables {
		id := localID(s.cfg.ID, t.ID)
		a.watchMatch(ctx, s, id, t)
		a.mu.Lock()
		_, listed := a.published[id]
		_, ours := a.matches[id]
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
		seen[id] = true
		a.mu.Lock()
		old, had := a.published[id]
		a.published[id] = published{srv: s, table: t, join: join}
		a.mu.Unlock()
		if had && old.join == join && sameTable(old.table, t) {
			continue
		}
		players, ai := t.taken()
		up := map[string]any{
			"localId": id, "title": t.Name, "format": format, "gameType": gameType,
			"seats":     map[string]int{"total": max(len(t.Seats), 2), "taken": players + ai, "ai": ai},
			"structure": "bo1", "hasPassword": t.Passworded, "visibility": "public",
			"spectate": map[string]bool{"allowed": t.Spectators, "seeHands": false},
			// 房主名不转发：那是别人服务器上的玩家（spec/02：不发布对方平台的账号名）
			"join": join, "extra": map[string]any{"x-xmage-server": s.cfg.Name},
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
	for id, p := range a.published {
		if p.srv == s && !seen[id] {
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
func (a *Adapter) watchMatch(ctx context.Context, s *server, id string, t Table) {
	a.mu.Lock()
	m := a.matches[id]
	if m == nil || m.started || t.State != "READY_TO_START" {
		a.mu.Unlock()
		return
	}
	m.started = true
	a.mu.Unlock()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := s.drv.Call(rctx, "start_match", map[string]any{"s": s.lobby(), "table": t.ID}); err != nil {
			a.log.Warn("xmage.start_match", "server", s.cfg.ID, "table", t.ID, "err", err)
			a.mu.Lock()
			m.started = false
			a.mu.Unlock()
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

func handoffNote(s *server, passworded bool) string {
	note := "打开 XMage 客户端，连 " + s.cfg.Name + "，在主房间（Main Room）找这张桌子"
	if s.cfg.Accounts == "required" {
		note += "；这台服务器要用注册过的 XMage 账号登录"
	}
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
		p, ok := a.published[b.LocalID]
		a.mu.Unlock()
		if !ok {
			_ = a.tn.SendEnv(ctx, proto.Error(e.ID, proto.CodeNoRoom, "XMage 上没有这张桌子了"))
			return
		}
		if b.Bridge && b.As == "player" && p.srv.canBridge() {
			a.mu.Lock()
			a.waiting[b.Player.PlayerID] = seatInfo{localID: b.LocalID, srv: p.srv, table: p.table, password: b.Password, locale: b.Player.Locale}
			a.mu.Unlock()
			_ = a.tn.Reply(ctx, e.ID, "venue.seat.bridged", map[string]any{})
			return
		}
		connect := map[string]any{"client": "xmage", "server": p.srv.cfg.Public, "room": p.table.Name}
		_ = a.tn.Reply(ctx, e.ID, "venue.seat.granted", map[string]any{"connect": connect, "code": p.table.Name, "note": handoffNote(p.srv, p.table.Passworded)})
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
			a.log.Info("bridge.end", "server", info.srv.cfg.ID, "channel", b.Channel, "err", err)
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

// queueServer：排队配到 XMage 时在哪台开桌（设了 queue 的那台，没设就第一台）。
func (a *Adapter) queueServer() *server {
	for _, s := range a.servers {
		if s.cfg.Queue {
			return s
		}
	}
	return a.servers[0]
}

// tableServer：真去开桌的那台。首选的那台刚启动、机器人还没登上时等它一会儿（最多 30 秒）；
// 它断着就换一台在线的。都不在线返回 nil。
func (a *Adapter) tableServer(ctx context.Context) *server {
	first := a.queueServer()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if first.isOnline() {
			return first
		}
		for _, s := range a.servers {
			if s.isOnline() && time.Now().After(deadline) {
				return s
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
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
	fail := func(msg string) {
		a.log.Warn("xmage.queue_table", "match", b.MatchID, "err", msg)
		_ = a.tn.SendEnv(ctx, proto.Error(e.ID, proto.CodeVenueUnavailable, msg))
	}
	s := a.tableServer(ctx)
	if s == nil {
		fail("没有在线的 XMage 服务器")
		return
	}
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
	res, err := s.drv.Call(rctx, "create_table", map[string]any{"s": s.lobby(), "name": name, "gameType": gt, "deckType": deckType,
		"seats": len(b.Players), "winsNeeded": wins, "password": password, "spectators": false})
	if err != nil {
		fail("XMage（" + s.cfg.Name + "）建桌失败：" + err.Error())
		return
	}
	var t Table
	_ = decodeAs(res, "table", &t)
	m := &matchTable{matchID: b.MatchID, localID: "m-" + b.MatchID, srv: s, tableID: t.ID, tableName: name, password: password, structure: b.Structure}
	seats := []map[string]any{}
	bridge := s.canBridge()
	a.mu.Lock()
	a.matches[localID(s.cfg.ID, t.ID)] = m
	for _, p := range b.Players {
		m.players = append(m.players, p.PlayerID)
		if p.Bridge && bridge {
			a.waiting[p.PlayerID] = seatInfo{localID: m.localID, srv: s, table: t, password: password, locale: p.Locale, match: m}
			seats = append(seats, map[string]any{"playerId": p.PlayerID, "bridge": true})
		} else {
			seats = append(seats, map[string]any{"playerId": p.PlayerID, "connect": map[string]any{
				"client": "xmage", "server": s.cfg.Public, "room": name, "password": password,
			}, "note": handoffNote(s, true)})
		}
	}
	a.mu.Unlock()
	a.log.Info("xmage.queue_table", "server", s.cfg.ID, "match", b.MatchID, "table", t.ID)
	_ = a.tn.Reply(ctx, e.ID, "venue.room.created", map[string]any{"localId": m.localID, "seats": seats})
	// 10 分钟还没坐满就撤桌（机器人一直挂着桌主，桌子不会自己消失；别在别人的服务器上留空桌）。
	go func() {
		select {
		case <-time.After(10 * time.Minute):
		case <-ctx.Done():
			return
		}
		a.mu.Lock()
		started := m.started
		a.mu.Unlock()
		if !started {
			dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = s.drv.Call(dctx, "remove_table", map[string]any{"s": s.lobby(), "table": t.ID})
			a.mu.Lock()
			delete(a.matches, localID(s.cfg.ID, t.ID))
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

// isAuthError：XMage 拒绝不注册的用户名登录（服务器开了注册）。
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	l := strings.ToLower(err.Error())
	return strings.Contains(l, "password") || strings.Contains(l, "regist") || strings.Contains(l, "auth")
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
