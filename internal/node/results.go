package node

import (
	"encoding/json"
	"time"

	"tablenet/pkg/proto"
)

// 战绩：房主报，节点把它定成一段固定的原文，所有在册玩家（含房主）各自签名确认，签齐了才落盘。

type resultPlayer struct {
	PlayerID string `json:"playerId"`
	Games    int    `json:"games"`
}

// resultReport 字段顺序固定，序列化出来就是要签的原文。
type resultReport struct {
	ResultID  string         `json:"resultId"`
	Node      string         `json:"node"`
	RoomID    string         `json:"roomId"`
	Format    string         `json:"format"`
	Structure string         `json:"structure"`
	Players   []resultPlayer `json:"players"`
	EndedAt   string         `json:"endedAt"`
	Reason    string         `json:"reason"`
	Reporter  string         `json:"reporter"`
}

type pendingResult struct {
	text    string
	report  resultReport
	sigs    map[string]string
	expires time.Time
}

var resultReasons = map[string]bool{"normal": true, "concede": true, "timeout": true, "disconnect": true, "draw": true}

func (h *Hub) onResultReport(c *conn, env proto.Envelope) {
	var b resultReport
	if err := env.Decode(&b); err != nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, err.Error()))
		return
	}
	r := h.rooms[b.RoomID]
	if r == nil || r.ownerPlayer != c.playerID {
		h.send(c, proto.Error(env.ID, proto.CodeNotOwner, "只有房主能报这间房的战绩"))
		return
	}
	if len(b.Players) < 2 || len(b.Players) > 16 || !resultReasons[b.Reason] {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "players 要 2–16 位，reason 取 normal／concede／timeout／disconnect／draw"))
		return
	}
	seen := map[string]bool{}
	for _, p := range b.Players {
		if proto.KeyOf(p.PlayerID) == nil || seen[p.PlayerID] {
			h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "players 里有坏编号或重复"))
			return
		}
		seen[p.PlayerID] = true
	}
	if !seen[c.playerID] {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "房主自己也要在 players 里"))
		return
	}
	b.ResultID = "r_" + hexString(randomBytes(8))
	b.Node, b.Reporter = h.NodeID, c.playerID
	b.Format, b.Structure = r.format, r.structure
	if b.EndedAt == "" {
		b.EndedAt = h.now().UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(b)
	pr := &pendingResult{text: string(raw), report: b, sigs: map[string]string{}, expires: h.now().Add(time.Hour)}
	h.results[b.ResultID] = pr
	e := proto.New("result.pending", "", "", map[string]any{"resultId": b.ResultID, "reportText": pr.text})
	h.send(c, proto.New("result.reported", "", env.ID, map[string]any{"resultId": b.ResultID, "reportText": pr.text}))
	for _, p := range b.Players {
		if p.PlayerID != c.playerID {
			h.send(h.playerConn(p.PlayerID), e)
		}
	}
}

func (h *Hub) onResultConfirm(c *conn, env proto.Envelope) {
	var b struct {
		ResultID string `json:"resultId"`
		Sig      string `json:"sig"`
	}
	_ = env.Decode(&b)
	pr := h.results[b.ResultID]
	if pr == nil {
		h.send(c, proto.Error(env.ID, proto.CodeBadRequest, "没有这条待确认的战绩"))
		return
	}
	listed := false
	for _, p := range pr.report.Players {
		listed = listed || p.PlayerID == c.playerID
	}
	if !listed || !proto.VerifyResult(c.playerID, b.Sig, pr.text) {
		h.send(c, proto.Error(env.ID, proto.CodeBadSignature, "签名不对，或你不在这局里"))
		return
	}
	pr.sigs[c.playerID] = b.Sig
	h.send(c, proto.New("result.signed", "", env.ID, map[string]any{"resultId": b.ResultID}))
	if len(pr.sigs) < len(pr.report.Players) {
		return
	}
	delete(h.results, b.ResultID)
	rec := map[string]any{"report": json.RawMessage(pr.text), "signatures": pr.sigs, "recordedAt": h.now().UTC().Format(time.RFC3339)}
	if err := h.st.appendLine("results.jsonl", rec); err != nil {
		h.log.Error("store.results", "err", err)
	}
	done := proto.New("result.confirmed", "", "", map[string]any{"resultId": b.ResultID})
	for _, p := range pr.report.Players {
		h.send(h.playerConn(p.PlayerID), done)
	}
}

func (h *Hub) expireResults(now time.Time) {
	for id, pr := range h.results {
		if now.After(pr.expires) {
			delete(h.results, id)
		}
	}
}
