package node

import (
	"encoding/json"
	"strings"
	"time"

	"tablenet/pkg/proto"
)

type room struct {
	id    string // <public_host>/<房号> 或 <public_host>/<平台>-<localId>
	code  string
	venue string // native 或 platformId

	ownerPlayer   string // 原生房的房主
	ownerPlatform string // 外部平台的房
	localID       string
	hostName      string

	title, format, gameType, structure, visibility, status, codec string
	seatsTotal, seatsTaken, seatsAI                               int
	hasPassword, spectateAllowed, seeHands                        bool
	joinKind                                                      string // native | handoff | bridge
	extra                                                         map[string]any

	createdAt      time.Time
	prevStatus     string
	suspendedUntil time.Time

	matchID string
	// codecInfo：按这间房的编码由客户端自己解释的大厅信息（独弈的座位摆法、变体、预算……），节点原样存、原样给
	codecInfo json.RawMessage
	allowed   map[string]bool // 私密房只收这些人
}

var gameTypes = map[string]bool{"duel": true, "2hg": true, "ffa": true, "teams": true, "draft-pod": true}

func (h *Hub) roomJSON(r *room, admin bool) map[string]any {
	m := map[string]any{
		"roomId": r.id, "venue": r.venue, "title": r.title, "format": r.format, "gameType": r.gameType,
		"seats":     map[string]int{"total": r.seatsTotal, "taken": r.seatsTaken, "ai": r.seatsAI},
		"structure": r.structure, "hasPassword": r.hasPassword, "visibility": r.visibility,
		"spectate": map[string]bool{"allowed": r.spectateAllowed, "seeHands": r.seeHands},
		"status":   r.status, "createdAt": r.createdAt.UTC().Format(time.RFC3339),
		"join": map[string]string{"kind": r.joinKind},
	}
	if r.codec != "" {
		m["codec"] = r.codec
	}
	if len(r.codecInfo) > 0 {
		m["codecInfo"] = r.codecInfo
	}
	if r.ownerPlayer != "" {
		m["host"] = map[string]string{"playerId": r.ownerPlayer, "displayName": r.hostName}
	} else if r.hostName != "" {
		m["host"] = map[string]any{"playerId": nil, "displayName": r.hostName}
	} else {
		m["host"] = nil
	}
	for k, v := range r.extra {
		m[k] = v
	}
	if admin && r.matchID != "" {
		m["matchId"] = r.matchID
	}
	return m
}

// listed：进大厅的房。
func (r *room) listed() bool { return r.visibility == "public" && r.status != "suspended" }

func (h *Hub) notifyRoom(r *room, wasListed bool) {
	nowListed := r.listed()
	if !wasListed && !nowListed {
		return
	}
	var e proto.Envelope
	if nowListed {
		e = proto.New("room.updated", "", "", map[string]any{"room": h.roomJSON(r, false)})
	} else {
		e = proto.New("room.removed", "", "", map[string]any{"roomId": r.id})
	}
	for _, c := range h.conns {
		if c.watching && c.authed {
			h.send(c, e)
		}
	}
}

func (h *Hub) removeRoom(id, why string) {
	r := h.rooms[id]
	if r == nil {
		return
	}
	was := r.listed()
	delete(h.rooms, id)
	// 外部平台的房间被撤，只表示「大厅里不再列它」：桥接通道的另一端是适配器，由适配器自己决定什么时候关
	// （适配器掉线时 dropConn 会一起关）。不然对方平台一开局、房间从大厅撤下，正在打的桥接局就被节点掐断了。
	if r.venue == proto.VenueNative {
		for chID, ch := range h.channels {
			if ch.roomID == id {
				h.closeChannel(chID, why, 0)
			}
		}
	}
	if was {
		e := proto.New("room.removed", "", "", map[string]any{"roomId": id})
		for _, c := range h.conns {
			if c.watching && c.authed {
				h.send(c, e)
			}
		}
	}
}

// ---- room.list ----

type listFilter struct {
	Venue    string `json:"venue"`
	Format   string `json:"format"`
	GameType string `json:"gameType"`
	Status   string `json:"status"`
}

func (h *Hub) onRoomList(c *conn, env proto.Envelope) {
	var f listFilter
	_ = env.Decode(&f)
	out := []map[string]any{}
	for _, r := range h.sortedRooms() {
		if !r.listed() || (f.Venue != "" && r.venue != f.Venue) || (f.Format != "" && r.format != f.Format) ||
			(f.GameType != "" && r.gameType != f.GameType) || (f.Status != "" && r.status != f.Status) {
			continue
		}
		out = append(out, h.roomJSON(r, false))
	}
	h.send(c, proto.New("room.listing", "", env.ID, map[string]any{"rooms": out}))
}

// ---- room.open / update / close ----

type seatsBody struct {
	Total *int `json:"total"`
	Taken *int `json:"taken"`
	AI    *int `json:"ai"`
}

type spectateBody struct {
	Allowed  *bool `json:"allowed"`
	SeeHands *bool `json:"seeHands"`
}

type roomOpenBody struct {
	Title       string          `json:"title"`
	Format      string          `json:"format"`
	GameType    string          `json:"gameType"`
	Seats       seatsBody       `json:"seats"`
	Structure   string          `json:"structure"`
	HasPassword bool            `json:"hasPassword"`
	Visibility  string          `json:"visibility"`
	Spectate    spectateBody    `json:"spectate"`
	Codec       string          `json:"codec"`
	Status      string          `json:"status"`
	CodecInfo   json.RawMessage `json:"codecInfo"`
	Code        string          `json:"code"` // 指定房号（只有原生房的 room.open 认）
}

func (h *Hub) ownedRooms(playerID string) int {
	n := 0
	for _, r := range h.rooms {
		if r.ownerPlayer == playerID {
			n++
		}
	}
	return n
}

func (h *Hub) newCode() string {
	for {
		code := proto.NewRoomCode()
		if h.rooms[h.cfg.Node.PublicHost+"/"+code] == nil {
			return code
		}
	}
}

func (h *Hub) onRoomOpen(c *conn, env proto.Envelope) {
	if h.refuseIfDraining(c, env) {
		return
	}
	var b roomOpenBody
	if err := env.Decode(&b); err != nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, err.Error()))
		return
	}
	if !contains(c.venues, proto.VenueNative) {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "握手时没声明能当主机（venues 里没有 native）"))
		return
	}
	r, msg := h.buildRoom(b)
	if r == nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, msg))
		return
	}
	if r.codec == "" || !contains(c.codecs, r.codec) {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "原生房必须带 codec，且是握手时声明过的"))
		return
	}
	// 指定房号：再来一局按原号重开、排队配上后接手节点替你开好的房、轮抓里各桌按对阵表约好的号。
	// 这个号已经是自己的房就接着用（换上新参数，留着邀请名单和配对编号），是别人的就拒。
	if b.Code != "" {
		code := strings.ToUpper(strings.TrimSpace(b.Code))
		if !validRoomCode(code) {
			h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "code 要 4–12 位字母或数字"))
			return
		}
		if old := h.rooms[h.cfg.Node.PublicHost+"/"+code]; old != nil {
			if old.ownerPlayer != c.playerID || old.venue != proto.VenueNative {
				h.send(c, proto.Error(env.ID, proto.CodeRoomTaken, "这个房号有人在用"))
				return
			}
			was := old.listed()
			r.id, r.code, r.venue, r.joinKind = old.id, old.code, old.venue, old.joinKind
			r.ownerPlayer, r.hostName, r.createdAt = c.playerID, c.displayName, old.createdAt
			r.allowed, r.matchID = old.allowed, old.matchID
			if old.visibility == "private" {
				r.visibility = "private" // 节点替你开的私密房（排队配上的）不因重开变公开
			}
			h.rooms[r.id] = r
			h.send(c, proto.New("room.opened", "", env.ID, map[string]any{"room": h.roomJSON(r, false)}))
			h.notifyRoom(r, was)
			h.log.Info("room.reopen", "room", r.id, "player", short(c.playerID))
			return
		}
	}
	if h.ownedRooms(c.playerID) >= h.cfg.Limits.MaxRoomsPerPlayer {
		h.send(c, proto.Error(env.ID, proto.CodeLimitReached, "开的房间太多了"))
		return
	}
	if b.Code != "" {
		r.code = strings.ToUpper(strings.TrimSpace(b.Code))
	} else {
		r.code = h.newCode()
	}
	r.id = h.cfg.Node.PublicHost + "/" + r.code
	r.venue, r.joinKind = proto.VenueNative, "native"
	r.ownerPlayer, r.hostName = c.playerID, c.displayName
	r.createdAt = h.now()
	h.rooms[r.id] = r
	h.send(c, proto.New("room.opened", "", env.ID, map[string]any{"room": h.roomJSON(r, false)}))
	h.notifyRoom(r, false)
	h.log.Info("room.open", "room", r.id, "format", r.format, "player", short(c.playerID))
}

// buildRoom 校验开房参数（原生房和适配器发布的房共用）。
func (h *Hub) buildRoom(b roomOpenBody) (*room, string) {
	r := &room{title: strings.TrimSpace(b.Title), format: b.Format, gameType: b.GameType, structure: b.Structure,
		visibility: b.Visibility, hasPassword: b.HasPassword, codec: b.Codec, status: "waiting", seatsTotal: 2, spectateAllowed: true}
	if len([]rune(r.title)) > 80 {
		return nil, "标题太长"
	}
	if r.format == "" || len(r.format) > 48 {
		return nil, "format 必填"
	}
	if r.gameType == "" {
		r.gameType = "duel"
	}
	if !gameTypes[r.gameType] {
		return nil, "未知的 gameType：" + r.gameType
	}
	if r.structure == "" {
		r.structure = "bo1"
	}
	if r.structure != "bo1" && r.structure != "bo3" {
		return nil, "structure 只能是 bo1 或 bo3"
	}
	if r.visibility == "" {
		r.visibility = "public"
	}
	if r.visibility != "public" && r.visibility != "unlisted" && r.visibility != "private" {
		return nil, "visibility 只能是 public、unlisted、private"
	}
	if b.Seats.Total != nil {
		r.seatsTotal = *b.Seats.Total
	}
	if r.seatsTotal < 2 || r.seatsTotal > 16 {
		return nil, "座位数要在 2–16 之间"
	}
	if b.Seats.Taken != nil {
		r.seatsTaken = clamp(*b.Seats.Taken, 0, r.seatsTotal)
	} else {
		r.seatsTaken = 1
	}
	if b.Seats.AI != nil {
		r.seatsAI = clamp(*b.Seats.AI, 0, r.seatsTotal)
	}
	if b.Spectate.Allowed != nil {
		r.spectateAllowed = *b.Spectate.Allowed
	}
	if b.Spectate.SeeHands != nil {
		r.seeHands = *b.Spectate.SeeHands
	}
	if b.Status != "" {
		if b.Status != "waiting" && b.Status != "playing" {
			return nil, "status 只能是 waiting 或 playing"
		}
		r.status = b.Status
	}
	if msg := setCodecInfo(r, b.CodecInfo); msg != "" {
		return nil, msg
	}
	return r, ""
}

// validRoomCode：指定的房号 4–12 位，大写字母或数字。
func validRoomCode(code string) bool {
	if len(code) < 4 || len(code) > 12 {
		return false
	}
	for _, r := range code {
		if !(r >= 0x41 && r <= 0x5a) && !(r >= 0x30 && r <= 0x39) {
			return false
		}
	}
	return true
}

// MaxCodecInfo 是 codecInfo 序列化后的上限（字节）。
const MaxCodecInfo = 1024

// setCodecInfo 只认 JSON 对象、限长；空的（没给或 null）不动。
func setCodecInfo(r *room, raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if len(raw) > MaxCodecInfo {
		return "codecInfo 太长"
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return "codecInfo 必须是 JSON 对象"
	}
	r.codecInfo = append(json.RawMessage(nil), raw...)
	return ""
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type roomUpdateBody struct {
	RoomID     string          `json:"roomId"`
	Title      *string         `json:"title"`
	Seats      seatsBody       `json:"seats"`
	Visibility string          `json:"visibility"`
	Status     string          `json:"status"`
	Spectate   spectateBody    `json:"spectate"`
	CodecInfo  json.RawMessage `json:"codecInfo"`
}

func (h *Hub) ownRoom(c *conn, env proto.Envelope, roomID string) *room {
	r := h.rooms[roomID]
	if r == nil {
		h.send(c, proto.Error(env.ID, proto.CodeNoRoom, "没有这个房间"))
		return nil
	}
	if r.ownerPlayer != c.playerID {
		h.send(c, proto.Error(env.ID, proto.CodeNotOwner, "只有房主能做这件事"))
		return nil
	}
	return r
}

func (h *Hub) onRoomUpdate(c *conn, env proto.Envelope) {
	var b roomUpdateBody
	if err := env.Decode(&b); err != nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, err.Error()))
		return
	}
	r := h.ownRoom(c, env, b.RoomID)
	if r == nil {
		return
	}
	was := r.listed()
	if b.Title != nil && len([]rune(*b.Title)) <= 80 {
		r.title = strings.TrimSpace(*b.Title)
	}
	if b.Seats.Taken != nil {
		r.seatsTaken = clamp(*b.Seats.Taken, 0, r.seatsTotal)
	}
	if b.Seats.AI != nil {
		r.seatsAI = clamp(*b.Seats.AI, 0, r.seatsTotal)
	}
	if b.Visibility == "public" || b.Visibility == "unlisted" || b.Visibility == "private" {
		r.visibility = b.Visibility
	}
	if b.Status == "waiting" || b.Status == "playing" {
		r.status = b.Status
	}
	if b.Spectate.Allowed != nil {
		r.spectateAllowed = *b.Spectate.Allowed
	}
	if b.Spectate.SeeHands != nil {
		r.seeHands = *b.Spectate.SeeHands
	}
	if msg := setCodecInfo(r, b.CodecInfo); msg != "" {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, msg))
		return
	}
	h.send(c, proto.New("room.updated", "", env.ID, map[string]any{"room": h.roomJSON(r, false)}))
	h.notifyRoom(r, was)
}

func (h *Hub) onRoomClose(c *conn, env proto.Envelope) {
	var b struct {
		RoomID string `json:"roomId"`
	}
	_ = env.Decode(&b)
	if r := h.ownRoom(c, env, b.RoomID); r != nil {
		h.removeRoom(r.id, proto.CodeRoomClosed)
		h.send(c, proto.New("room.closed", "", env.ID, map[string]any{"roomId": b.RoomID}))
	}
}

// ---- room.join ----

type roomJoinBody struct {
	RoomID   string `json:"roomId"`
	Password string `json:"password"`
	As       string `json:"as"`
}

func (h *Hub) onRoomJoin(c *conn, env proto.Envelope) {
	var b roomJoinBody
	if err := env.Decode(&b); err != nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, err.Error()))
		return
	}
	if b.As == "" {
		b.As = "player"
	}
	if c.role == "spectator-only" {
		b.As = "spectator"
	}
	if b.As != "player" && b.As != "spectator" {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "as 只能是 player 或 spectator"))
		return
	}
	// 只给房号（玩家口头报的那 6 位）也认：补上本节点的主机名，大小写不论
	if !strings.Contains(b.RoomID, "/") {
		b.RoomID = h.cfg.Node.PublicHost + "/" + strings.ToUpper(strings.TrimSpace(b.RoomID))
	}
	r := h.rooms[b.RoomID]
	if r == nil || (r.visibility == "private" && !r.allowed[c.playerID]) {
		h.send(c, proto.Error(env.ID, proto.CodeNoRoom, "没有这个房间"))
		return
	}
	if b.As == "spectator" && !r.spectateAllowed {
		h.send(c, proto.Error(env.ID, proto.CodeSpectateDisabled, "这间房不让观战"))
		return
	}
	if b.As == "player" && r.status == "waiting" && h.refuseIfDraining(c, env) {
		return // 排空中：还没开打的房不再收人；已开打的（掉线回来、观战）照常
	}
	if b.As == "player" && r.status == "waiting" && r.seatsTaken+r.seatsAI >= r.seatsTotal {
		h.send(c, proto.Error(env.ID, proto.CodeRoomFull, "坐满了"))
		return
	}
	if r.venue != proto.VenueNative {
		h.joinVenueRoom(c, env, r, b)
		return
	}
	if r.ownerPlayer == c.playerID {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "这是你自己的房间"))
		return
	}
	host := h.playerConn(r.ownerPlayer)
	if r.status == "suspended" || host == nil {
		h.send(c, proto.ErrorRetry(env.ID, proto.CodeHostOffline, "房主暂时不在，稍后再试", 5000))
		return
	}
	if !contains(c.codecs, r.codec) {
		h.send(c, proto.Error(env.ID, proto.CodeCodecMismatch, "你的客户端解不了这间房的对局编码："+r.codec))
		return
	}
	ch := h.openChannel(host.id, c.id, r.id, b.As == "spectator")
	h.send(host, proto.New("channel.opened", "", "", map[string]any{
		"channel": ch, "roomId": r.id, "as": b.As, "password": b.Password,
		"player": map[string]string{"playerId": c.playerID, "displayName": c.displayName},
	}))
	h.send(c, proto.New("room.joined", "", env.ID, map[string]any{"roomId": r.id, "channel": ch, "as": b.As}))
}

// ---- 掉线挂起、续接 ----

func (h *Hub) suspendPlayer(playerID string) {
	now := h.now()
	for _, r := range h.rooms {
		if r.ownerPlayer != playerID || r.status == "suspended" {
			continue
		}
		was := r.listed()
		hold := time.Duration(h.cfg.Limits.ResumeSeconds) * time.Second
		if r.status == "playing" {
			hold = time.Duration(h.cfg.Limits.SuspendMinutes) * time.Minute
		}
		r.prevStatus, r.status, r.suspendedUntil = r.status, "suspended", now.Add(hold)
		h.notifyRoom(r, was)
	}
	if t := h.tickets[playerID]; t != nil {
		t.offlineSince = now
	}
}

func (h *Hub) resumePlayer(c *conn) map[string]any {
	rooms := []string{}
	for _, r := range h.sortedRooms() {
		if r.ownerPlayer == c.playerID && r.status == "suspended" {
			r.status, r.suspendedUntil = r.prevStatus, time.Time{}
			rooms = append(rooms, r.id)
			h.notifyRoom(r, false)
		}
	}
	queued := false
	if t := h.tickets[c.playerID]; t != nil {
		t.conn, t.offlineSince, queued = c.id, time.Time{}, true
	}
	for _, p := range h.proposals {
		for _, m := range p.members {
			if m.playerID == c.playerID {
				m.conn = c.id
			}
		}
	}
	return map[string]any{"rooms": rooms, "queued": queued}
}

func (h *Hub) expireRooms(now time.Time) {
	for id, r := range h.rooms {
		if r.status == "suspended" && now.After(r.suspendedUntil) {
			h.log.Info("room.expired", "room", id)
			h.removeRoom(id, proto.CodeHostOffline)
		}
	}
}

// ---- 通道 ----

type channel struct {
	host, guest connID
	roomID      string
	spectator   bool
}

func (h *Hub) openChannel(host, guest connID, roomID string, spectator bool) uint32 {
	for {
		h.nextChannel++
		if h.nextChannel == 0 {
			h.nextChannel = 1
		}
		if h.channels[h.nextChannel] == nil {
			break
		}
	}
	h.channels[h.nextChannel] = &channel{host: host, guest: guest, roomID: roomID, spectator: spectator}
	return h.nextChannel
}

// closeChannel 关一条通道，通知还在的一端（by 是主动关的那一端，0 表示节点关的，两端都通知）。
func (h *Hub) closeChannel(id uint32, code string, by connID) {
	ch := h.channels[id]
	if ch == nil {
		return
	}
	delete(h.channels, id)
	e := proto.New("channel.closed", "", "", map[string]any{"channel": id, "code": code})
	if ch.host != by {
		h.sendTo(ch.host, e)
	}
	if ch.guest != by {
		h.sendTo(ch.guest, e)
	}
}

func (h *Hub) onChannelClose(c *conn, env proto.Envelope) {
	var b struct {
		Channel uint32 `json:"channel"`
		Code    string `json:"code"`
	}
	_ = env.Decode(&b)
	ch := h.channels[b.Channel]
	if ch == nil || (ch.host != c.id && ch.guest != c.id) {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "没有这条通道"))
		return
	}
	code := b.Code
	if code == "" || len(code) > 48 {
		code = proto.CodePeerLeft
	}
	h.closeChannel(b.Channel, code, c.id)
	if env.ID != "" {
		h.send(c, proto.New("channel.closed", "", env.ID, map[string]any{"channel": b.Channel, "code": code}))
	}
}
