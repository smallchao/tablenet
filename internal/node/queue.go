package node

import (
	"fmt"
	"sort"
	"time"

	"tablenet/pkg/proto"
)

type pool struct {
	Format    string `json:"format"`
	GameType  string `json:"gameType"`
	Structure string `json:"structure"`
}

type ticket struct {
	playerID     string
	displayName  string
	conn         connID
	pools        []pool
	venues       []string
	codecs       []string
	entered      time.Time
	offlineSince time.Time
}

type proposal struct {
	id       string
	pool     pool
	venue    string
	codec    string
	members  []*ticket
	accepted []bool
	order    []int // 接受的先后
	deadline time.Time
	creating bool // 已在等适配器开房
}

// venueCheck 说某个场地能不能开这种局。
type venueCheck func(venue string, p pool) bool

// pairUp 判两张排队单能不能配，配得上就给出池子、场地、编码。规则见 spec/01-客户端协议.md#配对规则。
func pairUp(a, b *ticket, ok venueCheck) (pool, string, string, bool) {
	for _, pa := range a.pools {
		for _, pb := range b.pools {
			if pa != pb {
				continue
			}
			// 原生优先：双方都能去原生局、且有共同的对局编码。
			if contains(a.venues, proto.VenueNative) && contains(b.venues, proto.VenueNative) {
				for _, c := range a.codecs {
					if contains(b.codecs, c) && ok(proto.VenueNative, pa) {
						return pa, proto.VenueNative, c, true
					}
				}
			}
			// 否则取 a 排在最前、b 也能去的外部平台。
			for _, v := range a.venues {
				if v != proto.VenueNative && contains(b.venues, v) && ok(v, pa) {
					return pa, v, "", true
				}
			}
		}
	}
	return pool{}, "", "", false
}

func (h *Hub) venueOK(v string, p pool) bool {
	if v == proto.VenueNative {
		return true
	}
	c := h.adapterFor(v)
	if c == nil {
		return false
	}
	pl := c.platform
	okList := func(list []string, s string) bool { return len(list) == 0 || contains(list, s) }
	return okList(pl.Formats, p.Format) && okList(pl.GameTypes, p.GameType) && okList(pl.Structures, p.Structure)
}

type queueEnterBody struct {
	Pools  []pool    `json:"pools"`
	Venues *[]string `json:"venues"`
	Codecs *[]string `json:"codecs"`
}

func (h *Hub) onQueueEnter(c *conn, env proto.Envelope) {
	var b queueEnterBody
	if err := env.Decode(&b); err != nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, err.Error()))
		return
	}
	if until, cooling := h.cooldown[c.playerID]; cooling {
		h.send(c, proto.ErrorRetry(env.ID, proto.CodeRateLimited, "连续几次没确认匹配，请稍后再排", until.Sub(h.now()).Milliseconds()))
		return
	}
	if len(b.Pools) == 0 || len(b.Pools) > 8 {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "pools 要 1–8 个"))
		return
	}
	for i := range b.Pools {
		p := &b.Pools[i]
		if p.Structure == "" {
			p.Structure = "bo1"
		}
		if p.GameType == "" {
			p.GameType = "duel"
		}
		if p.Format == "" || (p.Structure != "bo1" && p.Structure != "bo3") {
			h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "池子要有 format，structure 只能是 bo1／bo3"))
			return
		}
		if p.GameType != "duel" {
			h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "排队目前只支持一对一（duel）"))
			return
		}
	}
	if h.inProposal(c.playerID) != nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "正在确认一场匹配"))
		return
	}
	t := &ticket{playerID: c.playerID, displayName: c.displayName, conn: c.id, pools: b.Pools,
		venues: c.venues, codecs: c.codecs, entered: h.now()}
	if b.Venues != nil {
		t.venues = intersect(*b.Venues, c.venues)
	}
	if b.Codecs != nil {
		t.codecs = intersect(*b.Codecs, c.codecs)
	}
	if len(t.venues) == 0 {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "没有可去的场地（venues 为空）"))
		return
	}
	if old := h.tickets[c.playerID]; old != nil {
		t.entered = old.entered // 改排队条件不丢位置
	}
	h.tickets[c.playerID] = t
	h.send(c, proto.New("queue.entered", "", env.ID, map[string]any{"pools": t.pools, "venues": t.venues}))
	h.tryMatch(t)
}

func intersect(want, have []string) []string {
	out := []string{}
	for _, w := range want {
		if contains(have, w) && !contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func (h *Hub) onQueueLeave(c *conn, env proto.Envelope) {
	delete(h.tickets, c.playerID)
	h.send(c, proto.New("queue.left", "", env.ID, map[string]any{"reason": "left"}))
}

// waitingTickets：在线、按排队先后排好的单子。
func (h *Hub) waitingTickets() []*ticket {
	list := []*ticket{}
	for _, t := range h.tickets {
		if t.offlineSince.IsZero() {
			list = append(list, t)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].entered.Before(list[j].entered) })
	return list
}

func (h *Hub) tryMatch(t *ticket) {
	for _, other := range h.waitingTickets() {
		if other.playerID == t.playerID {
			continue
		}
		// 让先排的人当 a：场地按先排的人的偏好挑。
		a, b := other, t
		pl, venue, codec, ok := pairUp(a, b, h.venueOK)
		if !ok {
			continue
		}
		h.propose(pl, venue, codec, []*ticket{a, b})
		return
	}
}

func (h *Hub) propose(pl pool, venue, codec string, members []*ticket) {
	id := "m_" + hexString(randomBytes(6))
	p := &proposal{id: id, pool: pl, venue: venue, codec: codec, members: members,
		accepted: make([]bool, len(members)), deadline: h.now().Add(time.Duration(h.cfg.Limits.MatchAcceptSeconds) * time.Second)}
	h.proposals[id] = p
	for _, m := range members {
		delete(h.tickets, m.playerID)
	}
	for i, m := range members {
		opp := []map[string]string{}
		for j, o := range members {
			if j != i {
				opp = append(opp, map[string]string{"playerId": o.playerID, "displayName": o.displayName})
			}
		}
		h.sendTo(m.conn, proto.New("queue.match", "", "", map[string]any{
			"matchId": id, "pool": pl, "venue": venue, "opponents": opp,
			"acceptBy": p.deadline.UTC().Format(time.RFC3339),
		}))
	}
	h.log.Info("match.proposed", "match", id, "venue", venue, "format", pl.Format)
}

func (h *Hub) inProposal(playerID string) *proposal {
	for _, p := range h.proposals {
		for _, m := range p.members {
			if m.playerID == playerID {
				return p
			}
		}
	}
	return nil
}

func (h *Hub) onQueueAnswer(c *conn, env proto.Envelope) {
	var b struct {
		MatchID string `json:"matchId"`
	}
	_ = env.Decode(&b)
	p := h.proposals[b.MatchID]
	idx := -1
	if p != nil {
		for i, m := range p.members {
			if m.playerID == c.playerID {
				idx = i
			}
		}
	}
	if idx < 0 || p.creating {
		h.send(c, proto.Error(env.ID, proto.CodeMatchExpired, "这场匹配已经不在了"))
		return
	}
	if env.Type == "queue.decline" {
		h.send(c, proto.New("queue.declined", "", env.ID, map[string]any{"matchId": p.id}))
		h.cancelProposal(p, map[int]string{idx: "declined"})
		return
	}
	if !p.accepted[idx] {
		p.accepted[idx] = true
		p.order = append(p.order, idx)
	}
	h.send(c, proto.New("queue.accepted", "", env.ID, map[string]any{"matchId": p.id}))
	if len(p.order) == len(p.members) {
		h.finalize(p)
	}
}

// cancelProposal：out 里的人出队（附原因），其余人回到原来的排队位置。
func (h *Hub) cancelProposal(p *proposal, out map[int]string) {
	delete(h.proposals, p.id)
	for i, m := range p.members {
		if why, gone := out[i]; gone {
			h.sendTo(m.conn, proto.New("queue.left", "", "", map[string]any{"reason": why, "matchId": p.id}))
			continue
		}
		h.sendTo(m.conn, proto.New("queue.match_cancelled", "", "", map[string]any{"matchId": p.id}))
		h.tickets[m.playerID] = m
	}
	for i, m := range p.members {
		if _, gone := out[i]; !gone {
			h.tryMatch(m)
		}
	}
}

func (h *Hub) expireProposals(now time.Time) {
	for _, p := range h.proposals {
		if p.creating || now.Before(p.deadline) {
			continue
		}
		out := map[int]string{}
		for i, ok := range p.accepted {
			if !ok {
				out[i] = "timeout"
				id := p.members[i].playerID
				if h.strikes[id]++; h.strikes[id] >= 3 {
					h.strikes[id] = 0
					h.cooldown[id] = now.Add(5 * time.Minute)
				}
			}
		}
		h.cancelProposal(p, out)
	}
}

func (h *Hub) expireTickets(now time.Time) {
	hold := time.Duration(h.cfg.Limits.ResumeSeconds) * time.Second
	for id, t := range h.tickets {
		if !t.offlineSince.IsZero() && now.Sub(t.offlineSince) > hold {
			delete(h.tickets, id)
		}
	}
}

// finalize：双方都接受了。原生局由先接受的人当主机；外部平台请适配器开房。
func (h *Hub) finalize(p *proposal) {
	for _, m := range p.members {
		h.strikes[m.playerID] = 0
	}
	if p.venue == proto.VenueNative {
		host := p.members[p.order[0]]
		hc := h.playerConn(host.playerID)
		if hc == nil {
			h.cancelProposal(p, map[int]string{p.order[0]: "host_offline"})
			return
		}
		delete(h.proposals, p.id)
		r := &room{title: "排队对局", format: p.pool.Format, gameType: p.pool.GameType, structure: p.pool.Structure,
			visibility: "private", status: "waiting", codec: p.codec, seatsTotal: len(p.members), seatsTaken: 1,
			joinKind: "native", venue: proto.VenueNative, ownerPlayer: host.playerID, hostName: hc.displayName,
			createdAt: h.now(), matchID: p.id, allowed: map[string]bool{}}
		r.code = h.newCode()
		r.id = h.cfg.Node.PublicHost + "/" + r.code
		for _, m := range p.members {
			r.allowed[m.playerID] = true
		}
		h.rooms[r.id] = r
		opp := []map[string]string{}
		for _, m := range p.members {
			if m.playerID != host.playerID {
				opp = append(opp, map[string]string{"playerId": m.playerID, "displayName": m.displayName})
				h.sendTo(m.conn, proto.New("match.join", "", "", map[string]any{
					"matchId": p.id, "roomId": r.id, "host": map[string]string{"playerId": host.playerID, "displayName": hc.displayName},
				}))
			}
		}
		h.send(hc, proto.New("match.host", "", "", map[string]any{"matchId": p.id, "room": h.roomJSON(r, false), "opponents": opp}))
		h.log.Info("match.native", "match", p.id, "room", r.id)
		return
	}
	ad := h.adapterFor(p.venue)
	if ad == nil {
		h.cancelProposal(p, map[int]string{})
		return
	}
	p.creating = true
	players := []map[string]any{}
	for _, m := range p.members {
		bridge := contains(ad.platform.JoinKinds, "bridge") && len(intersect(m.codecs, ad.platform.Codecs)) > 0
		players = append(players, map[string]any{"playerId": m.playerID, "displayName": m.displayName, "linked": nil, "bridge": bridge})
	}
	id := h.newReqID()
	h.pendings[id] = &pending{kind: "create", adapter: ad.id, deadline: h.now().Add(h.seatTimeout()), matchID: p.id}
	h.send(ad, proto.New("venue.room.create", id, "", map[string]any{
		"matchId": p.id, "format": p.pool.Format, "gameType": p.pool.GameType, "structure": p.pool.Structure,
		"players": players, "visibility": "private", "spectate": map[string]bool{"allowed": false},
	}))
}

// matchFailed：适配器开不了房，双方放回排队（不算他们拒绝）。
func (h *Hub) matchFailed(matchID, code, msg string) {
	p := h.proposals[matchID]
	if p == nil {
		return
	}
	h.log.Warn("match.venue_failed", "match", matchID, "venue", p.venue, "code", code, "msg", msg)
	p.creating = false
	h.cancelProposal(p, map[int]string{})
}

func (h *Hub) matchCreated(ad *conn, matchID, localID string, seats []createdSeat) {
	p := h.proposals[matchID]
	if p == nil {
		return
	}
	bySeat := map[string]createdSeat{}
	for _, s := range seats {
		bySeat[s.PlayerID] = s
	}
	for _, m := range p.members {
		s, ok := bySeat[m.playerID]
		if !ok {
			h.matchFailed(matchID, proto.CodeVenueUnavailable, "适配器少给了一位玩家的座位")
			return
		}
		if !s.Bridge {
			if msg := h.checkHandoff(ad.platform, s.handoffBody); msg != "" {
				h.matchFailed(matchID, proto.CodeVenueUnavailable, msg)
				return
			}
		} else if len(intersect(m.codecs, ad.platform.Codecs)) == 0 {
			h.matchFailed(matchID, proto.CodeCodecMismatch, "给不能桥接的玩家开了桥接座位")
			return
		}
	}
	delete(h.proposals, matchID)
	roomID := h.cfg.Node.PublicHost + "/" + ad.platform.ID + "-" + localID
	for _, m := range p.members {
		s := bySeat[m.playerID]
		if !s.Bridge {
			h.sendTo(m.conn, proto.New("match.handoff", "", "", map[string]any{
				"matchId": matchID, "venue": p.venue, "roomId": roomID, "handoff": s.handoffBody}))
			continue
		}
		pc := h.conns[m.conn]
		if pc == nil {
			continue
		}
		ch := h.openChannel(ad.id, pc.id, roomID, false)
		h.send(ad, proto.New("channel.opened", "", "", map[string]any{
			"channel": ch, "roomId": roomID, "localId": localID, "as": "player", "matchId": matchID,
			"player": map[string]string{"playerId": pc.playerID, "displayName": pc.displayName},
		}))
		h.send(pc, proto.New("match.bridged", "", "", map[string]any{"matchId": matchID, "venue": p.venue, "roomId": roomID, "channel": ch}))
	}
	h.log.Info("match.venue", "match", matchID, "venue", p.venue)
}

func (h *Hub) broadcastQueueStatus() {
	counts := map[pool]int{}
	for _, t := range h.tickets {
		for _, p := range t.pools {
			counts[p]++
		}
	}
	for _, t := range h.waitingTickets() {
		status := []map[string]any{}
		for _, p := range t.pools {
			status = append(status, map[string]any{"pool": p, "waiting": counts[p]})
		}
		h.sendTo(t.conn, proto.New("queue.status", "", "", map[string]any{
			"pools": status, "waitedSeconds": int(h.now().Sub(t.entered).Seconds())}))
	}
}

func (p pool) String() string { return fmt.Sprintf("%s/%s/%s", p.Format, p.GameType, p.Structure) }
