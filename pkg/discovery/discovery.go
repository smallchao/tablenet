// Package discovery 是节点自组网的客户端一侧（spec/09）：取名录清单、登记名片、查附近的节点、
// 按证书指纹和节点证明安全地连上一台节点。节点、探测点、客户端共用。
package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tablenet/pkg/proto"
)

// DefaultRoots 是代码里写死的根公钥（主钥、备用钥）。项目方生成根密钥后填在这里，
// 或在构建时用 -ldflags "-X tablenet/pkg/discovery.rootList=ed25519:…,ed25519:…" 注入。
var DefaultRoots = splitList(rootList)

// DefaultListURLs 是拉名录清单的地址（官方现在只有香港一台）。同上，可构建时注入。
var DefaultListURLs = splitList(listURLs)

var (
	// 官方网络（独弈项目方运营）的根公钥与名录清单地址。自建网络构建时用 -ldflags 换掉。
	rootList = "ed25519:nsplsu2gduidpizgg52kmt23ylzkzehhlga7rqer5yw5rjlndy2a,ed25519:rrnky5kawyt7nsrpyt2mgudbvmhmp56s7hyqlxgk74ucexlv4txq"
	listURLs = "http://mtg.tosimple.top:39232/dir/v1/directories.json"
)

func splitList(s string) []string {
	out := []string{}
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s：HTTP %d %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, v)
}

func postJSON(ctx context.Context, u string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s：HTTP %d %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// FetchList 依次试每个地址，拿到第一份验签通过的名录清单。
func FetchList(ctx context.Context, urls, roots []string) (proto.DirectoryList, error) {
	if len(roots) == 0 {
		return proto.DirectoryList{}, errors.New("没有配置根公钥")
	}
	var last error = errors.New("没有配置名录清单地址")
	for _, u := range urls {
		var s proto.SignedList
		if err := getJSON(ctx, u, &s); err != nil {
			last = err
			continue
		}
		l, err := proto.VerifyDirectoryList(s, roots, time.Now())
		if err != nil {
			last = fmt.Errorf("%s：%w", u, err)
			continue
		}
		return l, nil
	}
	return proto.DirectoryList{}, last
}

// ---- 登记 ----

type RegisterResult struct {
	Seq     int64    `json:"seq"`
	Listed  bool     `json:"listed"`
	Reasons []string `json:"reasons"`
	Upgrade *struct {
		Version string `json:"version"`
		Note    string `json:"note"`
	} `json:"upgrade,omitempty"`
}

func Register(ctx context.Context, dirURL string, card proto.Signed) (RegisterResult, error) {
	var r RegisterResult
	err := postJSON(ctx, strings.TrimRight(dirURL, "/")+"/dir/v1/register", map[string]any{"card": card}, &r)
	return r, err
}

// ---- 查节点 ----

type Query struct {
	Region     string // 客户端自报地区；名录按探测区过滤
	Protocol   string
	Venue      string // 只要挂了某个外部平台的节点
	Limit      int
	IncludeNew bool   // 新节点默认不进推荐
	NodeID     string // 按编号找（private 的也能找到）
	Name       string // 按名字找
}

type ProbeResult struct {
	Zone  string    `json:"zone"`
	OK    bool      `json:"ok"`
	RTTMs int       `json:"rttMs"`
	At    time.Time `json:"at"`
}

// Listing 是名录回的一条：名片原文（客户端自己验签）、信任档、探测结果。
type Listing struct {
	Card   proto.Signed  `json:"card"`
	Tier   string        `json:"tier"` // verified | stable | new
	Probes []ProbeResult `json:"probes"`

	Node proto.NodeCard `json:"-"` // 验签后填上
}

func ListNodes(ctx context.Context, dirURL string, q Query) ([]Listing, error) {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("region", q.Region)
	set("protocol", q.Protocol)
	set("venue", q.Venue)
	set("id", q.NodeID)
	set("name", q.Name)
	if q.Limit > 0 {
		v.Set("limit", fmt.Sprint(q.Limit))
	}
	if q.IncludeNew {
		v.Set("include", "new")
	}
	var body struct {
		Nodes []Listing `json:"nodes"`
	}
	if err := getJSON(ctx, strings.TrimRight(dirURL, "/")+"/dir/v1/nodes?"+v.Encode(), &body); err != nil {
		return nil, err
	}
	out := []Listing{}
	now := time.Now()
	for _, l := range body.Nodes {
		c, err := proto.VerifyCard(l.Card, now)
		if err != nil {
			continue // 名录给的名片验不过就丢掉：名录改不了名片
		}
		l.Node = c
		out = append(out, l)
	}
	return out, nil
}

// ---- 安全地连一台节点 ----

// TLSConfigFor 返回连某个地址用的 TLS 配置：名片里有指纹就按指纹校验（自签证书也安全），没有就走系统证书链。
func TLSConfigFor(e proto.Endpoint) *tls.Config {
	if e.TLSSHA256 == "" {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	want := strings.ToLower(e.TLSSHA256)
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // 不走证书链，改由下面按指纹校验
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("对方没给证书")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("证书指纹和名片对不上")
			}
			return nil
		},
	}
}

// HTTPClientFor 是连 e 用的 HTTP 客户端（WebSocket 握手走它）。
func HTTPClientFor(e proto.Endpoint) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: TLSConfigFor(e)}}
}
