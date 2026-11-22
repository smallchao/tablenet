package node

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"tablenet/pkg/proto"
)

type platform struct {
	ID          string   `json:"platformId"`
	Name        string   `json:"name"`
	Homepage    string   `json:"homepage"`
	JoinKinds   []string `json:"joinKinds"`
	Formats     []string `json:"formats"`
	GameTypes   []string `json:"gameTypes"`
	Structures  []string `json:"structures"`
	Codecs      []string `json:"codecs"`
	AccountLink string   `json:"accountLink"`
	Terms       string   `json:"terms"`
	homeHost    string
}

type platformBody = platform

func (h *Hub) helloAdapter(c *conn, env proto.Envelope, b helloBody) {
	p := b.Platform
	if p == nil || !validPlatform(p.ID) {
		h.reject(c, env.ID, proto.CodeBadRequest, "适配器要带 platform，且 platformId 合规")
		return
	}
	conf, allowed := h.cfg.Adapters[p.ID]
	if !allowed {
		h.reject(c, env.ID, proto.CodePlatformNotAllowed, "本节点没放行平台 "+p.ID)
		return
	}
	home, err := url.Parse(p.Homepage)
	if err != nil || home.Scheme != "https" || home.Hostname() == "" {
		h.reject(c, env.ID, proto.CodeBadRequest, "platform.homepage 必须是 https 地址")
		return
	}
	p.homeHost = strings.ToLower(home.Hostname())
	want := conf.Key
	if want == "" {
		want = h.adapterKey[p.ID]
	}
	if want != "" && want != b.PlayerID {
		h.reject(c, env.ID, proto.CodePlatformConflict, "平台 "+p.ID+" 登记的是另一把公钥")
		return
	}
	if old, online := h.platforms[p.ID]; online && h.conns[old] != nil {
		h.reject(c, env.ID, proto.CodePlatformConflict, "平台 "+p.ID+" 已经有适配器在线")
		return
	}
	if want == "" { // 第一次见到：记下这把公钥（先到先得）
		h.adapterKey[p.ID] = b.PlayerID
		if err := h.st.writeJSON("adapters.json", h.adapterKey); err != nil {
			h.log.Error("store.adapters", "err", err)
		}
	}
	p.JoinKinds = cleanList(p.JoinKinds, 4)
	if len(p.JoinKinds) == 0 {
		p.JoinKinds = []string{"handoff"}
	}
	p.Formats, p.GameTypes, p.Structures, p.Codecs =
		cleanList(p.Formats, 64), cleanList(p.GameTypes, 8), cleanList(p.Structures, 4), cleanList(p.Codecs, 8)
	if strings.TrimSpace(p.Name) == "" {
		p.Name = p.ID
	}
	c.role, c.platform, c.authed = "adapter", p, true
	c.displayName = p.Name
	h.platforms[p.ID] = c.id
	delete(h.offline, p.ID)
	h.send(c, proto.New("welcome", "", env.ID, map[string]any{
		"session": fmt.Sprintf("s_%d", c.id), "resumeToken": proto.B64(proto.RandomBytes(18)),
		"you":       map[string]any{"playerId": c.playerID, "displayName": p.Name, "banned": false},
		"platforms": h.platformList(),
		"nodeProof": h.nodeProof(c),
	}))
	h.log.Info("adapter.online", "platform", p.ID, "joinKinds", strings.Join(p.JoinKinds, ","))
}

func (h *Hub) platformList() []map[string]any {
	out := []map[string]any{}
	for id, cid := range h.platforms {
		c := h.conns[cid]
		if c == nil || c.platform == nil {
			continue
		}
		p := c.platform
		out = append(out, map[string]any{
			"platformId": id, "name": p.Name, "homepage": p.Homepage, "joinKinds": p.JoinKinds,
			"codecs": p.Codecs, "terms": p.Terms, "accountLink": p.AccountLink, "online": true,
		})
	}
	return out
}

func (h *Hub) adapterFor(platformID string) *conn {
	if id, ok := h.platforms[platformID]; ok {
		return h.conns[id]
	}
	return nil
}

func (h *Hub) dispatchAdapter(c *conn, env proto.Envelope) {
	switch env.Type {
	case "venue.room.upsert":
		h.onVenueUpsert(c, env)
	case "venue.room.remove":
		h.onVenueRemove(c, env)
	case "venue.seat.granted", "venue.seat.bridged", "venue.room.created", "error":
		h.onAdapterReply(c, env)
	case "venue.result":
		h.onVenueResult(c, env)
	case "channel.close":
		h.onChannelClose(c, env)
	case "room.list":
		h.onRoomList(c, env)
	default:
		h.send(c, proto.Error(env.ID, proto.CodeUnknownType, "不认识的消息："+env.Type))
	}
}

// ---- 发布房间 ----

var localIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type venueRoomBody struct {
	roomOpenBody
	LocalID string `json:"localId"`
	Join    string `json:"join"`
	Host    *struct {
		DisplayName string `json:"displayName"`
	} `json:"host"`
	Extra map[string]any `json:"extra"`
}

func (h *Hub) onVenueUpsert(c *conn, env proto.Envelope) {
	var b venueRoomBody
	if err := env.Decode(&b); err != nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, err.Error()))
		return
	}
	if !localIDPattern.MatchString(b.LocalID) {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "localId 不合规"))
		return
	}
	if b.Visibility == "private" {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "适配器不得发布不公开的房间"))
		return
	}
	nr, msg := h.buildRoom(b.roomOpenBody)
	if nr == nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, msg))
		return
	}
	p := c.platform
	kind := b.Join
	if kind == "" {
		kind = p.JoinKinds[0]
	}
	if !contains(p.JoinKinds, kind) {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "join 必须是握手时声明过的档："+kind))
		return
	}
	if kind == "bridge" && (nr.codec == "" || !contains(p.Codecs, nr.codec)) {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "桥接房要带 codec，且是握手时声明过的"))
		return
	}
	id := h.cfg.Node.PublicHost + "/" + p.ID + "-" + b.LocalID
	r := h.rooms[id]
	was := false
	if r == nil {
		nr.createdAt = h.now()
		r = nr
		h.rooms[id] = r
	} else {
		was = r.listed()
		nr.createdAt, nr.allowed, nr.matchID = r.createdAt, r.allowed, r.matchID
		*r = *nr
	}
	r.id, r.venue, r.ownerPlatform, r.localID, r.joinKind = id, p.ID, p.ID, b.LocalID, kind
	r.code = p.ID + "-" + b.LocalID
	if b.Host != nil && len([]rune(b.Host.DisplayName)) <= 64 {
		r.hostName = b.Host.DisplayName
	}
	r.extra = map[string]any{}
	for k, v := range b.Extra {
		if strings.HasPrefix(k, "x-") {
			r.extra[k] = v
		}
	}
	if env.ID != "" {
		h.send(c, proto.New("venue.room.upserted", "", env.ID, map[string]any{"roomId": id}))
	}
	h.notifyRoom(r, was)
}

func (h *Hub) onVenueRemove(c *conn, env proto.Envelope) {
	var b struct {
		LocalID string `json:"localId"`
	}
	_ = env.Decode(&b)
	h.removeRoom(h.cfg.Node.PublicHost+"/"+c.platform.ID+"-"+b.LocalID, proto.CodeRoomClosed)
	if env.ID != "" {
		h.send(c, proto.New("venue.room.removed", "", env.ID, nil))
	}
}

// 适配器掉线 60 秒后撤下它发布的全部房间。
func (h *Hub) expireAdapterRooms(now time.Time) {
	for pid, since := range h.offline {
		if now.Sub(since) < 60*time.Second {
			continue
		}
		for id, r := range h.rooms {
			if r.ownerPlatform == pid {
				h.removeRoom(id, proto.CodeVenueUnavailable)
			}
		}
		delete(h.offline, pid)
	}
}

// ---- 向适配器要座位 ----

type pending struct {
	kind       string // seat | create
	adapter    connID
	deadline   time.Time
	player     connID
	playerReq  string
	roomID     string
	as         string
	matchID    string
	bridgeWant bool
}

func (h *Hub) newReqID() string {
	h.nextReq++
	return fmt.Sprintf("n%d", h.nextReq)
}

func (h *Hub) joinVenueRoom(c *conn, env proto.Envelope, r *room, b roomJoinBody) {
	ad := h.adapterFor(r.venue)
	if ad == nil {
		h.send(c, proto.ErrorRetry(env.ID, proto.CodeVenueUnavailable, "这个平台的适配器现在不在线", 30000))
		return
	}
	bridge := r.joinKind == "bridge" && contains(c.codecs, r.codec)
	if r.joinKind == "bridge" && !bridge && !contains(ad.platform.JoinKinds, "handoff") {
		h.send(c, proto.Error(env.ID, proto.CodeCodecMismatch, "你的客户端解不了这间房的对局编码："+r.codec))
		return
	}
	id := h.newReqID()
	h.pendings[id] = &pending{kind: "seat", adapter: ad.id, deadline: h.now().Add(h.seatTimeout()), player: c.id,
		playerReq: env.ID, roomID: r.id, as: b.As, bridgeWant: bridge}
	h.send(ad, proto.New("venue.seat.request", id, "", map[string]any{
		"localId": r.localID, "as": b.As, "password": b.Password, "bridge": bridge,
		"player": map[string]any{"playerId": c.playerID, "displayName": c.displayName, "locale": c.locale, "linked": nil},
	}))
}

func (h *Hub) seatTimeout() time.Duration {
	return time.Duration(h.cfg.Limits.SeatRequestSeconds) * time.Second
}

func (h *Hub) expirePendings(now time.Time) {
	for id, p := range h.pendings {
		if now.After(p.deadline) {
			h.failPending(id, p, proto.CodeVenueUnavailable, "外部平台没有及时回话")
		}
	}
}

// failPending：要座位失败告诉玩家；开房失败把排队的人放回去。
func (h *Hub) failPending(id string, p *pending, code, msg string) {
	delete(h.pendings, id)
	switch p.kind {
	case "seat":
		h.sendTo(p.player, proto.ErrorRetry(p.playerReq, code, msg, 5000))
	case "create":
		h.matchFailed(p.matchID, code, msg)
	}
}

type handoffBody struct {
	URL       string         `json:"url,omitempty"`
	Connect   map[string]any `json:"connect,omitempty"`
	Code      string         `json:"code,omitempty"`
	ExpiresAt string         `json:"expiresAt,omitempty"`
	Note      string         `json:"note,omitempty"`
}

type createdSeat struct {
	handoffBody
	PlayerID string `json:"playerId"`
	Bridge   bool   `json:"bridge"`
}

func (h *Hub) onAdapterReply(c *conn, env proto.Envelope) {
	p := h.pendings[env.Re]
	if p == nil || p.adapter != c.id {
		return // 超时之后才到的回话，丢掉
	}
	delete(h.pendings, env.Re)
	if env.Type == "error" {
		var e proto.ErrorBody
		_ = env.Decode(&e)
		if e.Code == "" {
			e.Code = proto.CodeVenueUnavailable
		}
		if p.kind == "seat" {
			h.sendTo(p.player, proto.New("error", "", p.playerReq, e))
		} else {
			h.matchFailed(p.matchID, e.Code, e.Message)
		}
		return
	}
	switch {
	case p.kind == "seat" && env.Type == "venue.seat.granted":
		var b handoffBody
		_ = env.Decode(&b)
		if msg := h.checkHandoff(c.platform, b); msg != "" {
			h.log.Warn("adapter.bad_handoff", "platform", c.platform.ID, "why", msg)
			h.sendTo(p.player, proto.Error(p.playerReq, proto.CodeVenueUnavailable, "外部平台给的入座信息不合规"))
			return
		}
		h.sendTo(p.player, proto.New("room.joined", "", p.playerReq, map[string]any{"roomId": p.roomID, "handoff": b}))
	case p.kind == "seat" && env.Type == "venue.seat.bridged":
		player := h.conns[p.player]
		if player == nil || !p.bridgeWant {
			return
		}
		ch := h.openChannel(c.id, player.id, p.roomID, p.as == "spectator")
		r := h.rooms[p.roomID]
		local := ""
		if r != nil {
			local = r.localID
		}
		h.send(c, proto.New("channel.opened", "", "", map[string]any{
			"channel": ch, "roomId": p.roomID, "localId": local, "as": p.as,
			"player": map[string]string{"playerId": player.playerID, "displayName": player.displayName},
		}))
		h.send(player, proto.New("room.joined", "", p.playerReq, map[string]any{"roomId": p.roomID, "channel": ch, "as": p.as}))
	case p.kind == "create" && env.Type == "venue.room.created":
		var b struct {
			LocalID string        `json:"localId"`
			Seats   []createdSeat `json:"seats"`
		}
		_ = env.Decode(&b)
		h.matchCreated(c, p.matchID, b.LocalID, b.Seats)
	default:
		h.log.Warn("adapter.unexpected_reply", "platform", c.platform.ID, "type", env.Type)
	}
}

// checkHandoff：链接必须是 https 且落在平台主页的域名下；connect.server 必须是配置里登记过的主机。
func (h *Hub) checkHandoff(p *platform, b handoffBody) string {
	if b.URL == "" && b.Connect == nil {
		return "url 和 connect 至少给一个"
	}
	if b.URL != "" {
		u, err := url.Parse(b.URL)
		if err != nil || u.Scheme != "https" {
			return "url 必须是 https"
		}
		host := strings.ToLower(u.Hostname())
		if host != p.homeHost && !strings.HasSuffix(host, "."+p.homeHost) {
			return "url 不在平台主页的域名下：" + host
		}
	}
	if b.Connect != nil {
		if s, _ := b.Connect["client"].(string); s == "" {
			return "connect.client 必填"
		}
		if raw, ok := b.Connect["server"]; ok {
			s, _ := raw.(string)
			host := strings.ToLower(hostOnly(s))
			okHost := false
			for _, allowed := range h.cfg.Adapters[p.ID].Servers {
				if strings.ToLower(allowed) == host {
					okHost = true
				}
			}
			if !okHost {
				return "connect.server 不在配置登记的服务器里：" + host
			}
		}
	}
	return ""
}

// hostOnly 从 "host:port"、"wss://host/path" 这类写法里取主机名。
func hostOnly(s string) string {
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			return u.Hostname()
		}
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}

// ---- 平台担保的战绩 ----

func (h *Hub) onVenueResult(c *conn, env proto.Envelope) {
	if !h.cfg.Adapters[c.platform.ID].TrustResults {
		h.log.Info("venue.result.ignored", "platform", c.platform.ID)
		return
	}
	var b map[string]any
	_ = env.Decode(&b)
	rec := map[string]any{"attestedBy": c.platform.ID, "report": b, "recordedAt": h.now().UTC().Format(time.RFC3339)}
	if err := h.st.appendLine("results.jsonl", rec); err != nil {
		h.log.Error("store.results", "err", err)
	}
}
