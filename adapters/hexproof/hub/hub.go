// SPDX-License-Identifier: GPL-3.0-or-later

// Package hub 是 Hexproof hub 的最小客户端：握手、发请求等回话、收推送。
//
// 线上格式按 Hexproof protocol/v3 的 schema 写（ClayStan404/hexproof，GPL-3.0）：
// 每条 WebSocket 消息是 {type, id?, seq?, payload?}；请求的 id 只回给发请求的那条连接。
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"
)

// Trace 不为空时，每条收发的原始消息都交给它（排查用，默认关）。
var Trace func(conn, dir string, data []byte)

// Protocol 是本适配器讲的 Hexproof 协议版本。hub 要求完全相等。
const Protocol = "hexproof.v3"

type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Seq     *int64          `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (e Envelope) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// ErrorPayload 是 hub 的 error 正文。
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HubError 是 hub 回的 error。
type HubError struct{ ErrorPayload }

func (e *HubError) Error() string { return "hexproof: " + e.Code + " " + e.Message }

// Welcome 是 session.welcome 里适配器关心的字段。
type Welcome struct {
	V                   string `json:"v"`
	ConnectionID        string `json:"connectionId"`
	ServerVersion       string `json:"serverVersion"`
	ResumeToken         string `json:"resumeToken"`
	ForgeRulesAvailable bool   `json:"forgeRulesAvailable"`
}

type Conn struct {
	ws      *websocket.Conn
	name    string
	Welcome Welcome

	mu      sync.Mutex
	nextID  int
	waiters map[string]chan Envelope
	push    chan Envelope
	closed  chan struct{}
	err     error
}

// Dial 连 hub 并完成 session.hello。
func Dial(ctx context.Context, url, displayName, clientVersion string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("连不上 Hexproof hub %s：%w", url, err)
	}
	ws.SetReadLimit(1 << 20)
	c := &Conn{ws: ws, name: displayName, waiters: map[string]chan Envelope{}, push: make(chan Envelope, 1024), closed: make(chan struct{})}
	go c.readLoop()
	reply, err := c.Request(ctx, "session.hello", map[string]any{
		"displayName": displayName, "clientVersion": clientVersion, "protocol": Protocol,
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	if reply.Type != "session.welcome" {
		c.Close()
		return nil, fmt.Errorf("hub 握手回的是 %s", reply.Type)
	}
	_ = reply.Decode(&c.Welcome)
	if c.Welcome.V != Protocol {
		c.Close()
		return nil, fmt.Errorf("hub 讲 %s，本适配器讲 %s", c.Welcome.V, Protocol)
	}
	return c, nil
}

func (c *Conn) readLoop() {
	defer close(c.closed)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
		if Trace != nil {
			Trace(c.name, "收", data)
		}
		var e Envelope
		if json.Unmarshal(data, &e) != nil || e.Type == "" {
			continue
		}
		if e.Type == "session.ping" {
			_ = c.Send(context.Background(), "session.pong", nil)
			continue
		}
		c.mu.Lock()
		w := c.waiters[e.ID]
		if w != nil {
			delete(c.waiters, e.ID)
		}
		c.mu.Unlock()
		if w != nil {
			w <- e
			continue
		}
		c.push <- e
	}
}

func (c *Conn) write(ctx context.Context, e Envelope) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if Trace != nil {
		Trace(c.name, "发", b)
	}
	return c.ws.Write(ctx, websocket.MessageText, b)
}

func payload(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(v)
	return b
}

// Send 发一条不等回话的消息。
func (c *Conn) Send(ctx context.Context, typ string, body any) error {
	return c.write(ctx, Envelope{Type: typ, Payload: payload(body)})
}

// Request 发请求并等带同一 id 的回话；回的是 error 时返回 *HubError。
func (c *Conn) Request(ctx context.Context, typ string, body any) (Envelope, error) {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("t%d", c.nextID)
	w := make(chan Envelope, 1)
	c.waiters[id] = w
	c.mu.Unlock()
	if err := c.write(ctx, Envelope{Type: typ, ID: id, Payload: payload(body)}); err != nil {
		return Envelope{}, err
	}
	select {
	case e := <-w:
		if e.Type == "error" {
			var p ErrorPayload
			_ = e.Decode(&p)
			return e, &HubError{p}
		}
		return e, nil
	case <-c.closed:
		return Envelope{}, c.Err()
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		return Envelope{}, ctx.Err()
	}
}

// Next 收下一条推送（不是某个请求的回话的）。
func (c *Conn) Next(ctx context.Context) (Envelope, error) {
	select {
	case e := <-c.push:
		return e, nil
	case <-c.closed:
		select {
		case e := <-c.push:
			return e, nil
		default:
		}
		return Envelope{}, c.Err()
	case <-ctx.Done():
		return Envelope{}, ctx.Err()
	}
}

func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		return errors.New("hub 连接已关闭")
	}
	return c.err
}

func (c *Conn) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// ---- 消息正文（只列适配器用到的字段） ----

type RoomListEntry struct {
	RoomID            string `json:"roomId"`
	RoomKind          string `json:"roomKind"`
	Name              string `json:"name"`
	Format            string `json:"format"`     // modern | duel | edh（牌桌类型）
	DeckFormat        string `json:"deckFormat"` // standard、modern、commander……（赛制）
	MatchMode         string `json:"matchMode"`  // bo1 | bo3
	RulesMode         string `json:"rulesMode"`  // manual | forge
	HostingMode       string `json:"hostingMode"`
	MaxSeats          int    `json:"maxSeats"`
	PlayerCount       int    `json:"playerCount"`
	SpectatorCount    int    `json:"spectatorCount"`
	AllowSpectators   bool   `json:"allowSpectators"`
	SpectatorsSeeHand bool   `json:"spectatorsSeeHands"`
	HasPassword       bool   `json:"hasPassword"`
	Phase             string `json:"phase"`
	PlayerJoinable    bool   `json:"playerJoinable"`
	SpectatorJoinable bool   `json:"spectatorJoinable"`
}

type DeckCard struct {
	Name            string `json:"name"`
	Count           int    `json:"count"`
	SetCode         string `json:"setCode"`
	CollectorNumber string `json:"collectorNumber"`
}

type DeckSelect struct {
	Name       string     `json:"name"`
	Format     string     `json:"format"`
	DeckFormat string     `json:"deckFormat"`
	Commanders []string   `json:"commanders,omitempty"`
	Mainboard  []DeckCard `json:"mainboard"`
	Sideboard  []DeckCard `json:"sideboard"`
}

type CardIdentity struct {
	Name            string `json:"name"`
	SetCode         string `json:"setCode"`
	CollectorNumber string `json:"collectorNumber"`
	Token           bool   `json:"token"`
}

type Counter struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

type PlayerState struct {
	Seat    int       `json:"seat"`
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Life    int       `json:"life"`
	Counter []Counter `json:"counters"`
	Mana    []Counter `json:"manaPool"`
}

type CardState struct {
	ID              string        `json:"id"`
	Visible         bool          `json:"visible"`
	Identity        *CardIdentity `json:"identity"`
	OwnerSeat       int           `json:"ownerSeat"`
	ControllerSeat  int           `json:"controllerSeat"`
	Tapped          bool          `json:"tapped"`
	SummoningSick   bool          `json:"summoningSick"`
	FaceDown        bool          `json:"faceDown"`
	Attacking       bool          `json:"attacking"`
	Blocking        []string      `json:"blocking"`
	Power           string        `json:"power"`
	Toughness       string        `json:"toughness"`
	Counters        []Counter     `json:"counters"`
	Damage          int           `json:"damage"`
	AttachedTo      string        `json:"attachedTo"`
	AttackingTarget string        `json:"attackingTarget"`
	AttackingPlayer *int          `json:"attackingPlayer"`
}

type ZoneState struct {
	Zone      string      `json:"zone"`
	OwnerSeat int         `json:"ownerSeat"`
	Count     int         `json:"count"`
	Cards     []CardState `json:"cards"`
}

type StackTarget struct {
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	ObjectID string `json:"objectId"`
	Seat     *int   `json:"seat"`
}

type StackObject struct {
	ID             string        `json:"id"`
	SourceID       string        `json:"sourceId"`
	ControllerSeat int           `json:"controllerSeat"`
	Identity       CardIdentity  `json:"identity"`
	Text           string        `json:"text"`
	Targets        []StackTarget `json:"targets"`
}

type Snapshot struct {
	RoomID       string        `json:"roomId"`
	GameID       string        `json:"gameId"`
	Turn         int           `json:"turn"`
	Step         string        `json:"step"`
	ActiveSeat   int           `json:"activeSeat"`
	PrioritySeat int           `json:"prioritySeat"`
	Players      []PlayerState `json:"players"`
	Zones        []ZoneState   `json:"zones"`
	Stack        []StackObject `json:"stack"`
	GameOver     bool          `json:"gameOver"`
	WinnerSeat   *int          `json:"winnerSeat"`
}

type PromptOption struct {
	ResponseID string `json:"responseId"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	CardID     string `json:"cardId"`
}

type PromptCard struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	SetCode         string `json:"setCode"`
	CollectorNumber string `json:"collectorNumber"`
	Token           bool   `json:"token"`
	Selected        bool   `json:"selected"`
	ReadOnly        bool   `json:"readOnly"`
}

type PromptChoice struct {
	ResponseID string `json:"responseId"`
	Label      string `json:"label"`
	Weight     int    `json:"weight"`
	CanRepeat  bool   `json:"canRepeat"`
}

type PromptOrderItem struct {
	ResponseID string `json:"responseId"`
	Name       string `json:"name"`
}

type PromptTarget struct {
	ResponseID string `json:"responseId"`
	Kind       string `json:"kind"` // card | player
	Label      string `json:"label"`
	ObjectID   string `json:"objectId"`
	Seat       *int   `json:"seat"`
	Name       string `json:"name"`
	Selected   bool   `json:"selected"`
}

type CombatSource struct {
	ResponseID     string   `json:"responseId"`
	ObjectID       string   `json:"objectId"`
	Label          string   `json:"label"`
	Name           string   `json:"name"`
	ValidTargetIDs []string `json:"validTargetIds"`
	MaxAssignments int      `json:"maxAssignments"`
}

type CombatTarget struct {
	ResponseID     string `json:"responseId"`
	Kind           string `json:"kind"`
	Label          string `json:"label"`
	ObjectID       string `json:"objectId"`
	Seat           *int   `json:"seat"`
	MaxAssignments int    `json:"maxAssignments"`
}

type DamageSource struct {
	ObjectID string `json:"objectId"`
	Label    string `json:"label"`
	Name     string `json:"name"`
}

type DamageTarget struct {
	ResponseID   string `json:"responseId"`
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	ObjectID     string `json:"objectId"`
	LethalDamage int    `json:"lethalDamage"`
}

type Prompt struct {
	RoomID               string            `json:"roomId"`
	GameID               string            `json:"gameId"`
	Pending              bool              `json:"pending"`
	PromptID             int64             `json:"promptId"`
	Kind                 string            `json:"kind"`
	Supported            bool              `json:"supported"`
	AutoPassEligible     bool              `json:"autoPassEligible"`
	Title                string            `json:"title"`
	Detail               string            `json:"detail"`
	Options              []PromptOption    `json:"options"`
	Choices              []PromptChoice    `json:"choices"`
	Cards                []PromptCard      `json:"cards"`
	ScryDestinations     []string          `json:"scryDestinations"`
	OrderItems           []PromptOrderItem `json:"orderItems"`
	ContextCards         []PromptCard      `json:"contextCards"`
	ContextText          string            `json:"contextText"`
	RequiredSelections   int               `json:"requiredSelections"`
	MinCardSelections    int               `json:"minCardSelections"`
	MaxCardSelections    int               `json:"maxCardSelections"`
	Targets              []PromptTarget    `json:"targets"`
	CombatSources        []CombatSource    `json:"combatSources"`
	CombatTargets        []CombatTarget    `json:"combatTargets"`
	DamageSource         *DamageSource     `json:"damageSource"`
	DamageTargets        []DamageTarget    `json:"damageTargets"`
	TotalDamage          int               `json:"totalDamage"`
	DamageAssignmentMode string            `json:"damageAssignmentMode"`
	MinSelections        int               `json:"minSelections"`
	MaxSelections        int               `json:"maxSelections"`
	Cancellable          bool              `json:"cancellable"`
	MinChoiceTotal       int               `json:"minChoiceTotal"`
	MaxChoiceTotal       int               `json:"maxChoiceTotal"`
	MinNumber            int               `json:"minNumber"`
	MaxNumber            int               `json:"maxNumber"`
}

type Assignment struct {
	SourceID string `json:"sourceId"`
	TargetID string `json:"targetId"`
}

type ScryPile struct {
	Destination string   `json:"destination"`
	CardIDs     []string `json:"cardIds"`
}

type DamageAssignment struct {
	TargetID string `json:"targetId"`
	Damage   int    `json:"damage"`
}

// Respond 是 rules.respond 的正文。
type Respond struct {
	PromptID          int64              `json:"promptId"`
	ResponseID        string             `json:"responseId"`
	CardIDs           []string           `json:"cardIds,omitempty"`
	TargetIDs         []string           `json:"targetIds,omitempty"`
	Assignments       []Assignment       `json:"assignments,omitempty"`
	ChoiceIDs         []string           `json:"choiceIds,omitempty"`
	OrderedIDs        []string           `json:"orderedIds,omitempty"`
	ScryPiles         []ScryPile         `json:"scryPiles,omitempty"`
	DamageOrderIDs    []string           `json:"damageOrderIds,omitempty"`
	DamageAssignments []DamageAssignment `json:"damageAssignments,omitempty"`
	ChosenNumber      *int               `json:"chosenNumber,omitempty"`
	Name              string             `json:"name,omitempty"`
}
