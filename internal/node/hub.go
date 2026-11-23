package node

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"tablenet/pkg/proto"
)

// Hub 是节点的全部状态。所有处理函数都在 mu 下运行、不做 IO，往连接里发东西只是往它的发送队列里塞。
type Hub struct {
	mu  sync.Mutex
	cfg Config
	key ed25519.PrivateKey
	// NodeID 是节点公钥编号。
	NodeID string
	log    *slog.Logger
	st     *store
	now    func() time.Time

	nextConn    uint64
	nextChannel uint32
	nextReq     uint64

	conns     map[connID]*conn
	byPlayer  map[string]connID // 在线玩家 → 连接
	perIP     map[string]int
	platforms map[string]connID // 在线适配器
	offline   map[string]time.Time

	rooms    map[string]*room
	channels map[uint32]*channel
	pendings map[string]*pending

	tickets   map[string]*ticket // playerId → 排队单
	proposals map[string]*proposal
	strikes   map[string]int
	cooldown  map[string]time.Time

	results map[string]*pendingResult

	bans       map[string]string
	adapterKey map[string]string

	startedAt time.Time
	daily     dailyStats
	draining  bool // 排空中：不接新局，等现有对局打完（spec/09#节点自身要补的）
	lastQueue time.Time
}

type connID uint64

type outMsg struct {
	binary bool
	data   []byte
}

type conn struct {
	id     connID
	out    chan outMsg
	done   chan struct{} // 节点不要这条连接了：写协程把队列里剩下的发完就关
	reason string
	ip     string
	nonce  []byte
	authed bool
	dead   bool

	role        string // player | spectator-only | adapter
	playerID    string
	displayName string
	locale      string
	venues      []string
	codecs      []string
	features    []string
	client      json.RawMessage
	watching    bool
	platform    *platform

	tokens   float64
	lastFill time.Time

	nodeChallenge []byte
}

type dailyStats struct {
	day     string
	players map[string]bool
}

const sendQueue = 512

// NewHub 读 data_dir 里的黑名单和适配器公钥，准备好节点状态。
func NewHub(cfg Config, key ed25519.PrivateKey, log *slog.Logger) (*Hub, error) {
	st, err := newStore(cfg.Node.DataDir)
	if err != nil {
		return nil, err
	}
	h := &Hub{
		cfg: cfg, key: key, NodeID: proto.IDOfKey(key), log: log, st: st, now: time.Now,
		conns: map[connID]*conn{}, byPlayer: map[string]connID{}, perIP: map[string]int{},
		platforms: map[string]connID{}, offline: map[string]time.Time{},
		rooms: map[string]*room{}, channels: map[uint32]*channel{}, pendings: map[string]*pending{},
		tickets: map[string]*ticket{}, proposals: map[string]*proposal{}, strikes: map[string]int{}, cooldown: map[string]time.Time{},
		results: map[string]*pendingResult{},
		bans:    map[string]string{}, adapterKey: map[string]string{},
	}
	h.startedAt = h.now()
	if err := st.readJSON("bans.json", &h.bans); err != nil {
		return nil, fmt.Errorf("读黑名单：%w", err)
	}
	if err := st.readJSON("adapters.json", &h.adapterKey); err != nil {
		return nil, fmt.Errorf("读适配器公钥：%w", err)
	}
	return h, nil
}

func validPlatform(id string) bool { return proto.ValidPlatformID(id) }

func randomBytes(n int) []byte { return proto.RandomBytes(n) }

// ---- 连接 ----

// Open 登记一条新连接并发 node.hello。返回 nil 表示超限，调用方直接断开。
func (h *Hub) Open(ip string) (*conn, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.conns) >= h.cfg.Limits.MaxConnections {
		return nil, "节点连接数已满"
	}
	if h.perIP[ip] >= h.cfg.Limits.MaxConnectionsPerIP {
		return nil, "同一地址连接太多"
	}
	h.nextConn++
	c := &conn{id: connID(h.nextConn), out: make(chan outMsg, sendQueue), done: make(chan struct{}), ip: ip, nonce: proto.RandomBytes(24),
		tokens: float64(h.burst()), lastFill: h.now()}
	h.conns[c.id] = c
	h.perIP[ip]++
	h.send(c, proto.New("node.hello", "", "", map[string]any{
		"protocols": []string{proto.Protocol},
		"nodeId":    h.NodeID,
		"name":      h.cfg.Node.Name,
		"software":  map[string]string{"name": "tablenet-node", "version": Version},
		"nonce":     proto.B64(c.nonce),
		"features":  h.features(),
		"limits": map[string]int{
			"maxRoomsPerPlayer": h.cfg.Limits.MaxRoomsPerPlayer,
			"maxMessageBytes":   proto.MaxMessageBytes,
			"messagesPerSecond": h.cfg.Limits.MessagesPerSecond,
		},
		"motd": h.cfg.Node.Motd,
	}))
	return c, ""
}

func (h *Hub) features() []string {
	return []string{"queue", "results", "node-proof"}
}

// nodeProof：客户端给了 nodeChallenge 才回，否则是 null。
func (h *Hub) nodeProof(c *conn) any {
	if c.nodeChallenge == nil {
		return nil
	}
	return proto.SignNodeProof(h.key, c.nodeChallenge, c.playerID)
}

func (h *Hub) burst() int { return 2 * h.cfg.Limits.MessagesPerSecond }

// Close 是连接断开后的收尾：房间挂起、通道关闭、排队单转离线。
func (h *Hub) Close(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropConn(c, proto.CodePeerLeft)
}

func (h *Hub) dropConn(c *conn, why string) {
	if c.dead {
		return
	}
	c.dead = true
	close(c.done)
	delete(h.conns, c.id)
	if h.perIP[c.ip]--; h.perIP[c.ip] <= 0 {
		delete(h.perIP, c.ip)
	}
	for id, ch := range h.channels {
		if ch.host == c.id || ch.guest == c.id {
			h.closeChannel(id, why, c.id)
		}
	}
	if !c.authed {
		return
	}
	switch c.role {
	case "adapter":
		if h.platforms[c.platform.ID] == c.id {
			delete(h.platforms, c.platform.ID)
			h.offline[c.platform.ID] = h.now()
			h.log.Info("adapter.offline", "platform", c.platform.ID)
		}
		for id, p := range h.pendings {
			if p.adapter == c.id {
				h.failPending(id, p, proto.CodeVenueUnavailable, "外部平台的适配器掉线了")
			}
		}
	default:
		if h.byPlayer[c.playerID] == c.id {
			delete(h.byPlayer, c.playerID)
		}
		h.suspendPlayer(c.playerID)
	}
}

// send 往连接里塞一条消息。发送队列满了说明对方读得太慢，直接断开，免得拖住整个节点。
func (h *Hub) send(c *conn, e proto.Envelope) {
	h.push(c, outMsg{data: e.Bytes()})
}

func (h *Hub) push(c *conn, m outMsg) {
	if c == nil || c.dead {
		return
	}
	select {
	case c.out <- m:
	default:
		h.log.Warn("conn.slow", "conn", c.id)
		h.kick(c, "发送队列已满")
	}
}

func (h *Hub) kick(c *conn, reason string) {
	if c.reason == "" {
		c.reason = reason
	}
	h.dropConn(c, proto.CodePeerLeft)
}

func (h *Hub) sendTo(id connID, e proto.Envelope) {
	h.send(h.conns[id], e)
}

func (h *Hub) playerConn(playerID string) *conn {
	if id, ok := h.byPlayer[playerID]; ok {
		return h.conns[id]
	}
	return nil
}

// ---- 收消息 ----

// HandleText 处理一条文本消息。
func (h *Hub) HandleText(c *conn, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.dead {
		return
	}
	env, err := proto.Parse(data)
	if err != nil {
		h.send(c, proto.Error("", proto.CodeBadRequest, err.Error()))
		return
	}
	if !h.allow(c) {
		metricRateLimited.Add(1)
		h.send(c, proto.ErrorRetry(env.ID, proto.CodeRateLimited, "发得太快了", 1000))
		return
	}
	switch {
	case env.Type == "ping":
		h.send(c, proto.New("pong", "", env.ID, nil))
		return
	case env.Type == "pong":
		return
	case !c.authed:
		if env.Type != "hello" {
			h.send(c, proto.Error(env.ID, proto.CodeNotAuthenticated, "先握手"))
			return
		}
		h.onHello(c, env)
		return
	}
	if c.role == "adapter" {
		h.dispatchAdapter(c, env)
	} else {
		h.dispatchPlayer(c, env)
	}
}

// 对局帧最多占到发送队列的这么多格：留出余量给文本消息（ping、通道通知），免得它们因为队列被大帧塞满而把人踢掉。
const binaryQueue = sendQueue - 64

// 收方一直腾不出地方、发方的帧等了这么久还进不去，才算收方卡死了，踢掉它。
const binaryStall = 60 * time.Second

// HandleBinary 转发一帧对局数据：只认通道号，不看负载。
//
// 收方的队列快满了就在这里等（不拿节点锁）：发方这条连接的读协程停下来，TCP 的流控一路传回发方，
// 发方自然放慢。以前是队列一满就踢收方——卡图、DIY 牌这类几十 MB 的大块，上行快、下行慢的一对
// （跨境线路晚高峰很常见）一传就把收方踢下线。
func (h *Hub) HandleBinary(c *conn, data []byte) {
	h.mu.Lock()
	if c.dead || !c.authed {
		h.mu.Unlock()
		return
	}
	ch, _, ok := proto.DecodeFrame(data)
	if !ok {
		h.mu.Unlock()
		return
	}
	link := h.channels[ch]
	if link == nil {
		h.mu.Unlock()
		return
	}
	var peerID connID
	switch c.id {
	case link.host:
		peerID = link.guest
	case link.guest:
		peerID = link.host
	default:
		h.mu.Unlock()
		return // 不是这条通道的人
	}
	peer := h.conns[peerID]
	metricRelayFrames.Add(1)
	metricRelayBytes.Add(int64(len(data)))
	if peer == nil || peer.dead {
		h.mu.Unlock()
		return
	}
	if len(peer.out) < binaryQueue {
		h.push(peer, outMsg{binary: true, data: data})
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	deadline := time.Now().Add(binaryStall)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-peer.done:
			return
		case <-c.done:
			return
		case <-tick.C:
		}
		h.mu.Lock()
		if peer.dead || c.dead {
			h.mu.Unlock()
			return
		}
		if len(peer.out) < binaryQueue {
			h.push(peer, outMsg{binary: true, data: data})
			h.mu.Unlock()
			return
		}
		if time.Now().After(deadline) {
			h.log.Warn("conn.stalled", "conn", peer.id)
			h.kick(peer, "收得太慢")
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
	}
}

// allow 是每连接的令牌桶，只限文本消息；对局帧不限。
func (h *Hub) allow(c *conn) bool {
	now := h.now()
	rate := float64(h.cfg.Limits.MessagesPerSecond)
	c.tokens += now.Sub(c.lastFill).Seconds() * rate
	if max := float64(h.burst()); c.tokens > max {
		c.tokens = max
	}
	c.lastFill = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

// ---- 握手 ----

type helloBody struct {
	Protocol    string          `json:"protocol"`
	Role        string          `json:"role"`
	PlayerID    string          `json:"playerId"`
	Sig         string          `json:"sig"`
	DisplayName string          `json:"displayName"`
	Client      json.RawMessage `json:"client"`
	Venues      []string        `json:"venues"`
	Codecs      []string        `json:"codecs"`
	Features    []string        `json:"features"`
	Locale      string          `json:"locale"`
	Platform    *platformBody   `json:"platform"`
	// NodeChallenge：客户端给的随机数，节点在 welcome 里用自己的密钥签名回去，证明自己就是这个编号（spec/09）。
	NodeChallenge string `json:"nodeChallenge"`
}

func (h *Hub) onHello(c *conn, env proto.Envelope) {
	var b helloBody
	if err := env.Decode(&b); err != nil {
		h.reject(c, env.ID, proto.CodeBadRequest, "hello 格式不对："+err.Error())
		return
	}
	if b.Protocol != proto.Protocol {
		h.reject(c, env.ID, proto.CodeUnsupportedProtocol, "本节点只讲 "+proto.Protocol)
		return
	}
	if !proto.VerifyHello(b.PlayerID, b.Sig, c.nonce, h.NodeID) {
		h.reject(c, env.ID, proto.CodeBadSignature, "握手签名不对")
		return
	}
	if reason, banned := h.bans[b.PlayerID]; banned {
		h.reject(c, env.ID, proto.CodeBanned, "已被本节点拉黑："+reason)
		return
	}
	c.playerID, c.client, c.locale, c.features = b.PlayerID, b.Client, b.Locale, b.Features
	if b.NodeChallenge != "" {
		ch, ok := proto.UnB64(b.NodeChallenge)
		if !ok || len(ch) < 16 || len(ch) > 64 {
			h.reject(c, env.ID, proto.CodeBadRequest, "nodeChallenge 要 16–64 字节的 b64")
			return
		}
		c.nodeChallenge = ch
	}
	switch b.Role {
	case "adapter":
		h.helloAdapter(c, env, b)
	case "player", "spectator-only":
		h.helloPlayer(c, env, b)
	case "node":
		h.reject(c, env.ID, proto.CodeBadRequest, "本节点没开联邦")
	default:
		h.reject(c, env.ID, proto.CodeBadRequest, "未知的 role："+b.Role)
	}
}

func (h *Hub) reject(c *conn, re, code, msg string) {
	metricHandshakeFail.Add(1)
	h.send(c, proto.Error(re, code, msg))
	h.kick(c, msg)
}

func (h *Hub) helloPlayer(c *conn, env proto.Envelope, b helloBody) {
	name := strings.TrimSpace(b.DisplayName)
	if name == "" || len([]rune(name)) > 64 {
		h.reject(c, env.ID, proto.CodeBadRequest, "displayName 要 1–64 个字")
		return
	}
	c.role, c.displayName = b.Role, name
	c.venues = cleanList(b.Venues, 16)
	c.codecs = cleanList(b.Codecs, 16)
	// 同一个人在别处还连着：顶掉旧的（先让旧连接把房间挂起，再在下面一起接回来）。
	if old := h.playerConn(b.PlayerID); old != nil {
		h.send(old, proto.Error("", proto.CodeReplaced, "这个身份在别处登录了"))
		h.kick(old, "replaced")
	}
	c.authed = true
	h.byPlayer[c.playerID] = c.id
	resumed := h.resumePlayer(c)
	h.countDaily(c.playerID)
	h.send(c, proto.New("welcome", "", env.ID, map[string]any{
		"session":     fmt.Sprintf("s_%d", c.id),
		"resumeToken": proto.B64(proto.RandomBytes(18)),
		"you":         map[string]any{"playerId": c.playerID, "displayName": c.displayName, "banned": false},
		"platforms":   h.platformList(),
		"resumed":     resumed,
		"nodeProof":   h.nodeProof(c),
	}))
	h.log.Info("player.hello", "player", short(c.playerID), "role", c.role)
}

func cleanList(in []string, max int) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || len(s) > 64 {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == max {
			break
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func short(id string) string {
	if len(id) > 20 {
		return id[:20]
	}
	return id
}

func (h *Hub) countDaily(playerID string) {
	day := h.now().UTC().Format("2006-01-02")
	if h.daily.day != day {
		h.daily = dailyStats{day: day, players: map[string]bool{}}
	}
	h.daily.players[playerID] = true
}

// ---- 分发 ----

func (h *Hub) dispatchPlayer(c *conn, env proto.Envelope) {
	spectatorOnly := c.role == "spectator-only"
	switch env.Type {
	case "room.list":
		h.onRoomList(c, env)
	case "room.watch":
		c.watching = true
		h.send(c, proto.New("room.watching", "", env.ID, nil))
	case "room.unwatch":
		c.watching = false
		h.send(c, proto.New("room.unwatched", "", env.ID, nil))
	case "room.join":
		h.onRoomJoin(c, env)
	case "channel.close":
		h.onChannelClose(c, env)
	default:
		if spectatorOnly {
			h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "观战身份不能做这件事"))
			return
		}
		switch env.Type {
		case "room.open":
			h.onRoomOpen(c, env)
		case "room.update":
			h.onRoomUpdate(c, env)
		case "room.close":
			h.onRoomClose(c, env)
		case "queue.enter":
			h.onQueueEnter(c, env)
		case "queue.leave":
			h.onQueueLeave(c, env)
		case "queue.pause", "queue.resume":
			h.onQueuePause(c, env, env.Type == "queue.pause")
		case "queue.accept", "queue.decline":
			h.onQueueAnswer(c, env)
		case "result.report":
			h.onResultReport(c, env)
		case "result.confirm":
			h.onResultConfirm(c, env)
		default:
			h.send(c, proto.Error(env.ID, proto.CodeUnknownType, "不认识的消息："+env.Type))
		}
	}
}

// ---- 定时 ----

// Tick 每秒跑一次：处理各种到期。
func (h *Hub) Tick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.expireProposals(now)
	h.expireRooms(now)
	h.expireTickets(now)
	h.expirePendings(now)
	h.expireAdapterRooms(now)
	h.expireResults(now)
	if now.Sub(h.lastQueue) >= 10*time.Second {
		h.lastQueue = now
		h.broadcastQueueStatus()
	}
	for id, until := range h.cooldown {
		if now.After(until) {
			delete(h.cooldown, id)
		}
	}
}

// ---- 自组网 ----

// cardState 给名片用：当前负载、挂了哪些平台、是否在排空。
func (h *Hub) cardState() (proto.NodeLoad, []proto.VenueInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	players := 0
	for _, c := range h.conns {
		if c.authed && c.role != "adapter" {
			players++
		}
	}
	venues := []proto.VenueInfo{}
	for id, cid := range h.platforms {
		if c := h.conns[cid]; c != nil && c.platform != nil {
			venues = append(venues, proto.VenueInfo{PlatformID: id, Bridge: contains(c.platform.JoinKinds, "bridge")})
		}
	}
	sort.Slice(venues, func(i, j int) bool { return venues[i].PlatformID < venues[j].PlatformID })
	return proto.NodeLoad{Players: players, Rooms: len(h.rooms), Channels: len(h.channels)}, venues, h.draining
}

// SetDraining 进入／退出排空。
func (h *Hub) SetDraining(on bool) {
	h.mu.Lock()
	h.draining = on
	h.mu.Unlock()
}

// ActiveChannels：排空时看还剩几局在打。
func (h *Hub) ActiveChannels() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.channels)
}

func (h *Hub) refuseIfDraining(c *conn, env proto.Envelope) bool {
	if h.draining {
		h.send(c, proto.Error(env.ID, proto.CodeDraining, "这台节点正在停机维护，不接新局；请换一台节点"))
	}
	return h.draining
}

// ---- 给 HTTP 和管理接口看的 ----

func (h *Hub) Snapshot() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	players := 0
	for _, c := range h.conns {
		if c.authed && c.role != "adapter" {
			players++
		}
	}
	return map[string]any{
		"name": h.cfg.Node.Name, "nodeId": h.NodeID, "startedAt": h.startedAt.UTC().Format(time.RFC3339),
		"connections": len(h.conns), "players": players, "rooms": len(h.rooms), "channels": len(h.channels),
		"queued": len(h.tickets), "matches": len(h.proposals), "platforms": h.platformList(),
		"today":    map[string]any{"date": h.daily.day, "players": len(h.daily.players)},
		"draining": h.draining,
	}
}

func (h *Hub) NodeInfo() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{
		"name": h.cfg.Node.Name, "nodeId": h.NodeID, "protocols": []string{proto.Protocol},
		"publicHost": h.cfg.Node.PublicHost, "features": h.features(), "platforms": h.platformList(),
		"software": map[string]string{"name": "tablenet-node", "version": Version},
	}
}

func (h *Hub) RoomsAdmin() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []map[string]any{}
	for _, r := range h.sortedRooms() {
		out = append(out, h.roomJSON(r, true))
	}
	return out
}

func (h *Hub) sortedRooms() []*room {
	list := make([]*room, 0, len(h.rooms))
	for _, r := range h.rooms {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].createdAt.Before(list[j].createdAt) })
	return list
}

// AdminCloseRoom 强制关房。
func (h *Hub) AdminCloseRoom(roomID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[roomID] == nil {
		return false
	}
	h.removeRoom(roomID, proto.CodeRoomClosed)
	return true
}

func (h *Hub) AdminBan(playerID, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if proto.KeyOf(playerID) == nil {
		return fmt.Errorf("不是合法的玩家编号")
	}
	h.bans[playerID] = reason
	if c := h.playerConn(playerID); c != nil {
		h.send(c, proto.Error("", proto.CodeBanned, "已被本节点拉黑："+reason))
		h.kick(c, "banned")
	}
	return h.st.writeJSON("bans.json", h.bans)
}

func (h *Hub) AdminUnban(playerID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.bans, playerID)
	return h.st.writeJSON("bans.json", h.bans)
}

func (h *Hub) AdminSetAdapterKey(platformID, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !validPlatform(platformID) || proto.KeyOf(key) == nil {
		return fmt.Errorf("平台编号或公钥不合规")
	}
	h.adapterKey[platformID] = key
	return h.st.writeJSON("adapters.json", h.adapterKey)
}

func (h *Hub) Bans() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{}
	for k, v := range h.bans {
		out[k] = v
	}
	return out
}
