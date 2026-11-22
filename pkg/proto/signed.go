package proto

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 签名文书：一段 JSON 原文加签名，签的就是原文的字节（不做 JSON 规范化）。
// 不同用途用不同的前缀，一种文书的签名拿不去冒充另一种。见 spec/09-节点自组网.md。
const (
	CtxNodeCard    = "tablenet/1 node-card"
	CtxNodeProof   = "tablenet/1 node-proof"
	CtxDirectories = "tablenet/1 directories"
	CtxObservation = "tablenet/1 probe"
	CtxEndorsement = "tablenet/1 endorse"
	CtxDelisting   = "tablenet/1 delist"
)

// Signed 是线上传的样子：{"text": "<JSON 原文>", "sig": "b64:…"}。
type Signed struct {
	Text string `json:"text"`
	Sig  string `json:"sig"`
}

func signedMessage(ctx string, text string) []byte {
	return append([]byte(ctx), text...)
}

// Sign 把 v 序列化成原文并签名。
func Sign(ctx string, key ed25519.PrivateKey, v any) (Signed, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Signed{}, err
	}
	text := string(raw)
	return Signed{Text: text, Sig: B64(ed25519.Sign(key, signedMessage(ctx, text)))}, nil
}

// Verify 用 signerID 的公钥验 s，通过后把原文解进 v。
func (s Signed) Verify(ctx, signerID string, v any) error {
	pub := KeyOf(signerID)
	raw, ok := UnB64(s.Sig)
	if pub == nil || !ok || len(raw) != ed25519.SignatureSize || !ed25519.Verify(pub, signedMessage(ctx, s.Text), raw) {
		return fmt.Errorf("签名不对")
	}
	if v != nil {
		if err := json.Unmarshal([]byte(s.Text), v); err != nil {
			return fmt.Errorf("原文不是合法 JSON：%w", err)
		}
	}
	return nil
}

// ---- 节点名片 ----

type Endpoint struct {
	URL       string `json:"url"`
	TLSSHA256 string `json:"tlsSha256,omitempty"` // 自签证书的 SHA-256 指纹（小写十六进制）；客户端按它校验
}

type VenueInfo struct {
	PlatformID string `json:"platformId"`
	Bridge     bool   `json:"bridge"`
}

type NodeLoad struct {
	Players  int `json:"players"`
	Rooms    int `json:"rooms"`
	Channels int `json:"channels"`
}

type NodeCapacity struct {
	Players   int `json:"players"`
	Rooms     int `json:"rooms"`
	RelayMbps int `json:"relayMbps"`
}

// 名片状态
const (
	CardOpen     = "open"     // 正常接客，进推荐
	CardDraining = "draining" // 不接新局
	CardPrivate  = "private"  // 只登记不推荐，凭编号或名字能找到
)

const (
	CardVersion     = 1
	CardMaxBytes    = 4096
	CardMaxLifetime = 30 * time.Minute
)

type NodeCard struct {
	V         int          `json:"v"`
	NodeID    string       `json:"nodeId"`
	Seq       int64        `json:"seq"`
	IssuedAt  time.Time    `json:"issuedAt"`
	ExpiresAt time.Time    `json:"expiresAt"`
	Name      string       `json:"name"`
	Region    string       `json:"region"`
	Endpoints []Endpoint   `json:"endpoints"`
	Protocols []string     `json:"protocols"`
	Features  []string     `json:"features,omitempty"`
	Venues    []VenueInfo  `json:"venues,omitempty"`
	Capacity  NodeCapacity `json:"capacity"`
	Load      NodeLoad     `json:"load"`
	Status    string       `json:"status"`
	Operator  string       `json:"operator,omitempty"`
	Software  struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"software"`
}

// Regions 是地区表（spec/09#地区）。
var Regions = []string{"cn-north", "cn-east", "cn-south", "cn-west", "hk", "tw", "jp", "kr", "sea", "eu", "na", "oceania", "other"}

func ValidRegion(r string) bool {
	for _, x := range Regions {
		if x == r {
			return true
		}
	}
	return false
}

// ProbeZone：地区归到哪个探测区（cn／hk／intl）。
func ProbeZone(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "cn"
	case region == "hk":
		return "hk"
	}
	return "intl"
}

var ProbeZones = []string{"cn", "hk", "intl"}

// SignCard 填好 nodeId 后签名。
func SignCard(key ed25519.PrivateKey, c NodeCard) (Signed, error) {
	c.V, c.NodeID = CardVersion, IDOfKey(key)
	return Sign(CtxNodeCard, key, c)
}

// VerifyCard 验名片：签名、格式、有效期。now 用来判过期。
func VerifyCard(s Signed, now time.Time) (NodeCard, error) {
	var c NodeCard
	if len(s.Text) > CardMaxBytes {
		return c, fmt.Errorf("名片太大")
	}
	if err := json.Unmarshal([]byte(s.Text), &c); err != nil {
		return c, fmt.Errorf("名片不是合法 JSON")
	}
	if err := s.Verify(CtxNodeCard, c.NodeID, nil); err != nil {
		return c, fmt.Errorf("名片%w", err)
	}
	switch {
	case c.V != CardVersion:
		return c, fmt.Errorf("名片版本 %d 不认", c.V)
	case c.Seq <= 0:
		return c, fmt.Errorf("名片 seq 必须为正")
	case !c.ExpiresAt.After(c.IssuedAt) || c.ExpiresAt.Sub(c.IssuedAt) > CardMaxLifetime:
		return c, fmt.Errorf("名片有效期不对（最长 %v）", CardMaxLifetime)
	case !now.Before(c.ExpiresAt):
		return c, fmt.Errorf("名片已过期")
	case c.IssuedAt.After(now.Add(5 * time.Minute)):
		return c, fmt.Errorf("名片签发时间在未来（时钟不准？）")
	case len([]rune(strings.TrimSpace(c.Name))) == 0 || len([]rune(c.Name)) > 40:
		return c, fmt.Errorf("名称要 1–40 字")
	case !ValidRegion(c.Region):
		return c, fmt.Errorf("地区 %q 不认", c.Region)
	case len(c.Endpoints) == 0 || len(c.Endpoints) > 4:
		return c, fmt.Errorf("地址要 1–4 个")
	case c.Status != CardOpen && c.Status != CardDraining && c.Status != CardPrivate:
		return c, fmt.Errorf("状态 %q 不认", c.Status)
	}
	for _, e := range c.Endpoints {
		if !strings.HasPrefix(e.URL, "wss://") && !strings.HasPrefix(e.URL, "ws://") {
			return c, fmt.Errorf("地址要 ws:// 或 wss://：%s", e.URL)
		}
	}
	return c, nil
}

// ---- 节点证明身份 ----

// NodeProofMessage："tablenet/1 node-proof" || challenge || playerId || nodeId。
func NodeProofMessage(challenge []byte, playerID, nodeID string) []byte {
	m := append([]byte(CtxNodeProof), challenge...)
	m = append(m, playerID...)
	return append(m, nodeID...)
}

func SignNodeProof(key ed25519.PrivateKey, challenge []byte, playerID string) string {
	return B64(ed25519.Sign(key, NodeProofMessage(challenge, playerID, IDOfKey(key))))
}

func VerifyNodeProof(nodeID, proof string, challenge []byte, playerID string) bool {
	return verify(nodeID, proof, NodeProofMessage(challenge, playerID, nodeID))
}

// ---- 名录清单（根密钥签） ----

type DirectoryEntry struct {
	ID     string `json:"id"`     // 名录的公钥编号
	URL    string `json:"url"`    // https://… 或 http://…（测试）
	Region string `json:"region"` // 名录所在地区
}

type ProbeEntry struct {
	ID   string `json:"id"`   // 独立探测点的公钥编号
	Zone string `json:"zone"` // cn／hk／intl
}

type DirectoryList struct {
	V            int              `json:"v"`
	Seq          int64            `json:"seq"`
	IssuedAt     time.Time        `json:"issuedAt"`
	ExpiresAt    time.Time        `json:"expiresAt"`
	Directories  []DirectoryEntry `json:"directories"`
	Probes       []ProbeEntry     `json:"probes,omitempty"`
	Coordinators []string         `json:"coordinators,omitempty"` // 第二期
}

// SignedList 是带签名者的名录清单：签名者必须是根公钥之一。
type SignedList struct {
	Signed
	Signer string `json:"signer"`
}

func SignDirectoryList(root ed25519.PrivateKey, l DirectoryList) (SignedList, error) {
	l.V = 1
	s, err := Sign(CtxDirectories, root, l)
	return SignedList{Signed: s, Signer: IDOfKey(root)}, err
}

// VerifyDirectoryList：签名者在 roots 里、签名对、没过期。
func VerifyDirectoryList(s SignedList, roots []string, now time.Time) (DirectoryList, error) {
	var l DirectoryList
	trusted := false
	for _, r := range roots {
		trusted = trusted || r == s.Signer
	}
	if !trusted {
		return l, fmt.Errorf("名录清单的签名者不是根密钥")
	}
	if err := s.Verify(CtxDirectories, s.Signer, &l); err != nil {
		return l, fmt.Errorf("名录清单%w", err)
	}
	if !now.Before(l.ExpiresAt) {
		return l, fmt.Errorf("名录清单已过期")
	}
	if len(l.Directories) == 0 {
		return l, fmt.Errorf("名录清单是空的")
	}
	return l, nil
}

// HasDirectory／HasProbe：某个公钥是不是清单里的名录、探测点（探测点也认名录自己）。
func (l DirectoryList) HasDirectory(id string) bool {
	for _, d := range l.Directories {
		if d.ID == id {
			return true
		}
	}
	return false
}

func (l DirectoryList) ProbeZoneOf(id string) (string, bool) {
	for _, d := range l.Directories {
		if d.ID == id {
			return ProbeZone(d.Region), true
		}
	}
	for _, p := range l.Probes {
		if p.ID == id {
			return p.Zone, true
		}
	}
	return "", false
}

// ---- 探测结果、认证、下架 ----

type Observation struct {
	NodeID string    `json:"nodeId"`
	Seq    int64     `json:"seq"` // 探测时节点名片的 seq
	Zone   string    `json:"zone"`
	OK     bool      `json:"ok"`
	RTTMs  int       `json:"rttMs"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

type SignedBy struct {
	Signed
	Signer string `json:"signer"`
}

type Endorsement struct {
	NodeID    string    `json:"nodeId"`
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Note      string    `json:"note,omitempty"`
}

type Delisting struct {
	NodeID string    `json:"nodeId"`
	Zones  []string  `json:"zones,omitempty"`  // 空表示整台下架
	Lifted bool      `json:"lifted,omitempty"` // true 表示撤销下架
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

func SignBy(ctx string, key ed25519.PrivateKey, v any) (SignedBy, error) {
	s, err := Sign(ctx, key, v)
	return SignedBy{Signed: s, Signer: IDOfKey(key)}, err
}
