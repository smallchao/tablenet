package node

import (
	"encoding/json"
	"time"

	"tablenet/pkg/proto"
)

// 节点状态落盘（spec/09#节点自身要补的）：房间、排队单写进 data_dir/state.json。
// 重启后全部按「房主掉线」处理：房间挂起、排队单转离线，玩家在续接窗口内用同一把密钥重连就接回来。
// 外部平台的房间不存：适配器重连后会重新发布。对局通道不存：重启后两端各自重连、由主机重新接上。
//
// 规范初稿倾向 SQLite；状态量很小（一台节点几百间房），JSON 快照够用，也免了 cgo 依赖。

const stateFile = "state.json"

type savedRoom struct {
	ID          string          `json:"id"`
	Code        string          `json:"code"`
	OwnerPlayer string          `json:"ownerPlayer"`
	HostName    string          `json:"hostName"`
	Title       string          `json:"title"`
	Format      string          `json:"format"`
	GameType    string          `json:"gameType"`
	Structure   string          `json:"structure"`
	Visibility  string          `json:"visibility"`
	Status      string          `json:"status"` // 挂起前的状态
	Codec       string          `json:"codec"`
	SeatsTotal  int             `json:"seatsTotal"`
	SeatsTaken  int             `json:"seatsTaken"`
	SeatsAI     int             `json:"seatsAI"`
	HasPassword bool            `json:"hasPassword"`
	Spectate    bool            `json:"spectate"`
	SeeHands    bool            `json:"seeHands"`
	CodecInfo   json.RawMessage `json:"codecInfo,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	MatchID     string          `json:"matchId,omitempty"`
	Allowed     map[string]bool `json:"allowed,omitempty"`
}

type savedTicket struct {
	PlayerID    string    `json:"playerId"`
	DisplayName string    `json:"displayName"`
	Pools       []pool    `json:"pools"`
	Venues      []string  `json:"venues"`
	Codecs      []string  `json:"codecs"`
	Entered     time.Time `json:"entered"`
}

type savedState struct {
	SavedAt time.Time     `json:"savedAt"`
	Rooms   []savedRoom   `json:"rooms"`
	Tickets []savedTicket `json:"tickets"`
}

// SaveState 写一份快照。
func (h *Hub) SaveState() error {
	h.mu.Lock()
	st := savedState{SavedAt: h.now().UTC(), Rooms: []savedRoom{}, Tickets: []savedTicket{}}
	for _, r := range h.rooms {
		if r.ownerPlayer == "" {
			continue // 外部平台的房
		}
		status := r.status
		if status == "suspended" {
			status = r.prevStatus
		}
		st.Rooms = append(st.Rooms, savedRoom{ID: r.id, Code: r.code, OwnerPlayer: r.ownerPlayer, HostName: r.hostName,
			Title: r.title, Format: r.format, GameType: r.gameType, Structure: r.structure, Visibility: r.visibility,
			Status: status, Codec: r.codec, SeatsTotal: r.seatsTotal, SeatsTaken: r.seatsTaken, SeatsAI: r.seatsAI,
			HasPassword: r.hasPassword, Spectate: r.spectateAllowed, SeeHands: r.seeHands, CodecInfo: r.codecInfo, CreatedAt: r.createdAt,
			MatchID: r.matchID, Allowed: r.allowed})
	}
	for _, t := range h.tickets {
		st.Tickets = append(st.Tickets, savedTicket{PlayerID: t.playerID, DisplayName: t.displayName, Pools: t.pools,
			Venues: t.venues, Codecs: t.codecs, Entered: t.entered})
	}
	h.mu.Unlock()
	return h.st.writeJSON(stateFile, st)
}

// LoadState 启动时读回快照：房间挂起、排队单离线，等玩家重连。快照太旧（超过挂起上限）就不要了。
func (h *Hub) LoadState() (rooms, tickets int, err error) {
	var st savedState
	if err := h.st.readJSON(stateFile, &st); err != nil {
		return 0, 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	resume := time.Duration(h.cfg.Limits.ResumeSeconds) * time.Second
	suspend := time.Duration(h.cfg.Limits.SuspendMinutes) * time.Minute
	if st.SavedAt.IsZero() || now.Sub(st.SavedAt) > suspend {
		return 0, 0, nil
	}
	for _, s := range st.Rooms {
		if s.Status != "waiting" && s.Status != "playing" {
			continue
		}
		hold := resume
		if s.Status == "playing" {
			hold = suspend
		}
		h.rooms[s.ID] = &room{id: s.ID, code: s.Code, venue: proto.VenueNative, ownerPlayer: s.OwnerPlayer, hostName: s.HostName,
			title: s.Title, format: s.Format, gameType: s.GameType, structure: s.Structure, visibility: s.Visibility,
			status: "suspended", prevStatus: s.Status, suspendedUntil: now.Add(hold), codec: s.Codec,
			seatsTotal: s.SeatsTotal, seatsTaken: s.SeatsTaken, seatsAI: s.SeatsAI, hasPassword: s.HasPassword,
			spectateAllowed: s.Spectate, seeHands: s.SeeHands, codecInfo: s.CodecInfo, joinKind: "native", createdAt: s.CreatedAt,
			matchID: s.MatchID, allowed: s.Allowed}
		rooms++
	}
	for _, s := range st.Tickets {
		h.tickets[s.PlayerID] = &ticket{playerID: s.PlayerID, displayName: s.DisplayName, pools: s.Pools,
			venues: s.Venues, codecs: s.Codecs, entered: s.Entered, offlineSince: now}
		tickets++
	}
	return rooms, tickets, nil
}
