// Package client 是最小的 Tablenet 客户端：连节点、握手、发请求等回话、收发对局通道帧。
//
// 适配器和测试用它。它对外公开，别人写 Go 适配器可以直接 import。
package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"tablenet/pkg/discovery"
	"tablenet/pkg/proto"
)

// Message 是收到的一条消息或一帧对局数据（Frame 非零时是帧）。
type Message struct {
	Env     proto.Envelope
	Channel uint32
	Payload []byte
}

func (m Message) IsFrame() bool { return m.Channel != 0 }

type Client struct {
	ws        *websocket.Conn
	key       ed25519.PrivateKey
	PlayerID  string
	NodeHello map[string]any
	Welcome   map[string]any

	mu      sync.Mutex
	nextID  int
	waiters map[string]chan proto.Envelope
	inbox   chan Message
	closed  chan struct{}
	err     error
}

// Dial 连上节点并完成握手。hello 里放 role、displayName、venues、codecs、platform；protocol／playerId／sig 由这里补。
func Dial(ctx context.Context, url string, key ed25519.PrivateKey, hello map[string]any) (*Client, error) {
	return DialWith(ctx, url, key, hello, Options{})
}

// Options：连一台从名录找到的节点时用。
type Options struct {
	// HTTPClient 用来做 WebSocket 握手；连自签证书的节点时用 discovery.HTTPClientFor 给的（按指纹校验）。
	HTTPClient *http.Client
	// ExpectNodeID 不为空时，要求对面就是这台节点：node.hello 的编号对得上，而且 welcome 里的节点证明验得过。
	ExpectNodeID string
}

// DialNode 连名录给的一台节点：按名片里的证书指纹校验 TLS，并要求节点证明身份。
func DialNode(ctx context.Context, card proto.NodeCard, endpoint int, key ed25519.PrivateKey, hello map[string]any) (*Client, error) {
	if endpoint < 0 || endpoint >= len(card.Endpoints) {
		return nil, errors.New("没有这个地址")
	}
	e := card.Endpoints[endpoint]
	return DialWith(ctx, e.URL, key, hello, Options{HTTPClient: discovery.HTTPClientFor(e), ExpectNodeID: card.NodeID})
}

func DialWith(ctx context.Context, url string, key ed25519.PrivateKey, hello map[string]any, o Options) (*Client, error) {
	var dopts *websocket.DialOptions
	if o.HTTPClient != nil {
		dopts = &websocket.DialOptions{HTTPClient: o.HTTPClient}
	}
	ws, _, err := websocket.Dial(ctx, url, dopts)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：%w", url, err)
	}
	ws.SetReadLimit(proto.MaxMessageBytes + 4)
	c := &Client{ws: ws, key: key, PlayerID: proto.IDOfKey(key), waiters: map[string]chan proto.Envelope{},
		inbox: make(chan Message, 1024), closed: make(chan struct{})}
	_, data, err := ws.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("等 node.hello：%w", err)
	}
	nh, err := proto.Parse(data)
	if err != nil || nh.Type != "node.hello" {
		return nil, fmt.Errorf("节点先发来的不是 node.hello")
	}
	var body struct {
		Protocols []string `json:"protocols"`
		NodeID    string   `json:"nodeId"`
		Nonce     string   `json:"nonce"`
	}
	_ = nh.Decode(&body)
	_ = nh.Decode(&c.NodeHello)
	nonce, ok := proto.UnB64(body.Nonce)
	if !ok || body.NodeID == "" {
		return nil, errors.New("node.hello 缺 nonce 或 nodeId")
	}
	speaks := false
	for _, p := range body.Protocols {
		speaks = speaks || p == proto.Protocol
	}
	if !speaks {
		return nil, errors.New("节点不讲 " + proto.Protocol)
	}
	if hello == nil {
		hello = map[string]any{}
	}
	if o.ExpectNodeID != "" && body.NodeID != o.ExpectNodeID {
		ws.CloseNow()
		return nil, fmt.Errorf("对面自称 %s，不是要连的节点 %s", body.NodeID, o.ExpectNodeID)
	}
	hello["protocol"], hello["playerId"], hello["sig"] = proto.Protocol, c.PlayerID, proto.SignHello(key, nonce, body.NodeID)
	var challenge []byte
	if o.ExpectNodeID != "" {
		challenge = proto.RandomBytes(24)
		hello["nodeChallenge"] = proto.B64(challenge)
	}
	go c.readLoop()
	reply, err := c.Request(ctx, "hello", hello)
	if err != nil {
		return nil, err
	}
	if code := reply.ErrorCode(); code != "" {
		return nil, fmt.Errorf("握手被拒：%s %s", code, string(reply.Body))
	}
	_ = reply.Decode(&c.Welcome)
	if challenge != nil {
		proof, _ := c.Welcome["nodeProof"].(string)
		if !proto.VerifyNodeProof(o.ExpectNodeID, proof, challenge, c.PlayerID) {
			c.Close()
			return nil, errors.New("节点没能证明自己的身份（可能被冒充）")
		}
	}
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.closed)
	for {
		typ, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
		if typ == websocket.MessageBinary {
			ch, payload, ok := proto.DecodeFrame(data)
			if ok {
				c.inbox <- Message{Channel: ch, Payload: append([]byte(nil), payload...)}
			}
			continue
		}
		env, err := proto.Parse(data)
		if err != nil {
			continue
		}
		if env.Type == "ping" {
			_ = c.SendEnv(context.Background(), proto.New("pong", "", env.ID, nil))
			continue
		}
		c.mu.Lock()
		w := c.waiters[env.Re]
		if w != nil {
			delete(c.waiters, env.Re)
		}
		c.mu.Unlock()
		if w != nil {
			w <- env
			continue
		}
		c.inbox <- Message{Env: env}
	}
}

func (c *Client) SendEnv(ctx context.Context, e proto.Envelope) error {
	return c.ws.Write(ctx, websocket.MessageText, e.Bytes())
}

// Notify 发一条不需要回话的消息。
func (c *Client) Notify(ctx context.Context, typ string, body any) error {
	return c.SendEnv(ctx, proto.New(typ, "", "", body))
}

// Reply 回某条请求。
func (c *Client) Reply(ctx context.Context, re, typ string, body any) error {
	return c.SendEnv(ctx, proto.New(typ, "", re, body))
}

// Request 发请求并等回话（error 也算回话）。
func (c *Client) Request(ctx context.Context, typ string, body any) (proto.Envelope, error) {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("c%d", c.nextID)
	w := make(chan proto.Envelope, 1)
	c.waiters[id] = w
	c.mu.Unlock()
	if err := c.SendEnv(ctx, proto.New(typ, id, "", body)); err != nil {
		return proto.Envelope{}, err
	}
	select {
	case e := <-w:
		return e, nil
	case <-c.closed:
		return proto.Envelope{}, c.Err()
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
		return proto.Envelope{}, ctx.Err()
	}
}

func (c *Client) SendFrame(ctx context.Context, channel uint32, payload []byte) error {
	return c.ws.Write(ctx, websocket.MessageBinary, proto.EncodeFrame(channel, payload))
}

// Next 收下一条不是回话的东西（通知、节点发来的请求、对局帧）。
func (c *Client) Next(ctx context.Context) (Message, error) {
	select {
	case m := <-c.inbox:
		return m, nil
	case <-c.closed:
		select {
		case m := <-c.inbox:
			return m, nil
		default:
		}
		return Message{}, c.Err()
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

// Expect 一直收，直到某种消息出现；中间的东西丢掉。测试里用。
func (c *Client) Expect(ctx context.Context, typ string) (proto.Envelope, error) {
	for {
		m, err := c.Next(ctx)
		if err != nil {
			return proto.Envelope{}, fmt.Errorf("等 %s：%w", typ, err)
		}
		if !m.IsFrame() && m.Env.Type == typ {
			return m.Env, nil
		}
	}
}

// ExpectFrame 一直收，直到收到一帧。
func (c *Client) ExpectFrame(ctx context.Context) (uint32, []byte, error) {
	for {
		m, err := c.Next(ctx)
		if err != nil {
			return 0, nil, err
		}
		if m.IsFrame() {
			return m.Channel, m.Payload, nil
		}
	}
}

func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		return errors.New("连接已关闭")
	}
	return c.err
}

// Done 在连接断开时关闭。
func (c *Client) Done() <-chan struct{} { return c.closed }

func (c *Client) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// Key 返回私钥（签战绩用）。
func (c *Client) Key() ed25519.PrivateKey { return c.key }

// DecodeBody 小工具：把消息正文解进 map。
func DecodeBody(e proto.Envelope) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(e.Body, &m)
	return m
}

// Timeout 是常用的带超时 context。
func Timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
