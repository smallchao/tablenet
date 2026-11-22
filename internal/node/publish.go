package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"tablenet/pkg/discovery"
	"tablenet/pkg/proto"
)

// 名片续期节奏（spec/09）：每 10 分钟签一张、有效 30 分钟；登记失败 1 分钟后重试。
var (
	cardEvery    = 10 * time.Minute
	cardLifetime = 30 * time.Minute
	retryEvery   = time.Minute
	pendingEvery = 30 * time.Second // 名录还在探测：过一会儿再登记一次，好让运营者早点看到「已公开」
	listCacheFor = time.Hour
)

type publisher struct {
	s       *Server
	trigger chan struct{}

	mu       sync.Mutex
	list     proto.DirectoryList
	listAt   time.Time
	results  map[string]string // 名录 URL → 最近一次结果
	lastCard string
}

func newPublisher(s *Server) *publisher {
	return &publisher{s: s, trigger: make(chan struct{}, 1), results: map[string]string{}}
}

// poke 让发布循环马上重签一次（状态变了，比如进入排空）。
func (p *publisher) poke() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

func (p *publisher) run(ctx context.Context) {
	for {
		pending, err := p.publishOnce(ctx)
		wait := cardEvery
		if pending && pendingEvery < wait {
			wait = pendingEvery
		}
		if err != nil {
			p.s.log.Warn("directory.publish", "err", err)
			wait = retryEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-p.trigger:
		case <-time.After(wait):
		}
	}
}

func (p *publisher) roots() []string {
	if r := p.s.cfg.Directory.Roots; len(r) > 0 {
		return r
	}
	return discovery.DefaultRoots
}

func (p *publisher) loadList(ctx context.Context) (proto.DirectoryList, error) {
	p.mu.Lock()
	if !p.listAt.IsZero() && time.Since(p.listAt) < listCacheFor {
		l := p.list
		p.mu.Unlock()
		return l, nil
	}
	p.mu.Unlock()
	d := p.s.cfg.Directory
	var l proto.DirectoryList
	var err error
	if d.ListFile != "" {
		var s proto.SignedList
		b, rerr := os.ReadFile(d.ListFile)
		if rerr != nil {
			return l, rerr
		}
		if err := json.Unmarshal(b, &s); err != nil {
			return l, fmt.Errorf("名录清单文件：%w", err)
		}
		l, err = proto.VerifyDirectoryList(s, p.roots(), time.Now())
	} else {
		urls := d.ListURLs
		if len(urls) == 0 {
			urls = discovery.DefaultListURLs
		}
		l, err = discovery.FetchList(ctx, urls, p.roots())
	}
	if err != nil {
		return l, err
	}
	p.mu.Lock()
	p.list, p.listAt = l, time.Now()
	p.mu.Unlock()
	return l, nil
}

// targets：登记到本探测区的名录，再加一台别的区的（spec/09#一台节点登记到哪几台名录）。
func targets(l proto.DirectoryList, region string) []proto.DirectoryEntry {
	zone := proto.ProbeZone(region)
	var same, other []proto.DirectoryEntry
	for _, d := range l.Directories {
		if proto.ProbeZone(d.Region) == zone {
			same = append(same, d)
		} else {
			other = append(other, d)
		}
	}
	out := same
	if len(other) > 0 {
		out = append(out, other[0])
	}
	if len(out) == 0 {
		out = l.Directories
	}
	return out
}

func (p *publisher) publicURLs() ([]proto.Endpoint, error) {
	cfg := p.s.cfg
	fp := ""
	if cfg.Listen.TLSCert != "" {
		var err error
		if fp, err = CertFingerprint(cfg.Listen.TLSCert); err != nil {
			return nil, err
		}
	}
	urls := cfg.Directory.PublicURLs
	if len(urls) == 0 {
		_, port, _ := net.SplitHostPort(cfg.Listen.WS)
		scheme := "ws"
		if cfg.Listen.TLSCert != "" {
			scheme = "wss"
		}
		urls = []string{scheme + "://" + net.JoinHostPort(cfg.Node.PublicHost, port) + proto.WSPath}
	}
	out := []proto.Endpoint{}
	for _, u := range urls {
		e := proto.Endpoint{URL: u}
		if strings.HasPrefix(u, "wss://") {
			e.TLSSHA256 = fp
		}
		out = append(out, e)
	}
	return out, nil
}

// Card 签一张当前的名片。
func (p *publisher) Card() (proto.Signed, error) {
	cfg := p.s.cfg
	eps, err := p.publicURLs()
	if err != nil {
		return proto.Signed{}, err
	}
	load, venues, draining := p.s.Hub.cardState()
	now := time.Now().UTC().Truncate(time.Second)
	c := proto.NodeCard{
		Seq: time.Now().UnixMilli(), IssuedAt: now, ExpiresAt: now.Add(cardLifetime),
		Name: cfg.Node.Name, Region: cfg.Directory.Region, Endpoints: eps, Protocols: []string{proto.Protocol},
		Features: p.s.Hub.features(), Venues: venues, Load: load, Status: cfg.Directory.Status, Operator: cfg.Directory.Operator,
		Capacity: proto.NodeCapacity{Players: cfg.Directory.CapacityPlayers, Rooms: cfg.Directory.CapacityRooms, RelayMbps: cfg.Directory.RelayMbps},
	}
	if draining {
		c.Status = proto.CardDraining
	}
	c.Software.Name, c.Software.Version = "tablenet-node", Version
	return proto.SignCard(p.s.Hub.key, c)
}

func (p *publisher) publishOnce(ctx context.Context) (pending bool, _ error) {
	l, err := p.loadList(ctx)
	if err != nil {
		return false, fmt.Errorf("取名录清单：%w", err)
	}
	card, err := p.Card()
	if err != nil {
		return false, fmt.Errorf("签名片：%w", err)
	}
	var firstErr error
	ok := 0
	for _, d := range targets(l, p.s.cfg.Directory.Region) {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		res, err := discovery.Register(rctx, d.URL, card)
		cancel()
		msg := ""
		switch {
		case err != nil:
			msg = "失败：" + err.Error()
			if firstErr == nil {
				firstErr = err
			}
		case res.Listed:
			msg, ok = "已公开", ok+1
		default:
			msg, ok = "已登记、未公开："+strings.Join(res.Reasons, "，"), ok+1
			pending = pending || contains(res.Reasons, "probe_pending")
		}
		if err == nil && res.Upgrade != nil {
			msg += fmt.Sprintf("（名录建议升级到 %s：%s）", res.Upgrade.Version, res.Upgrade.Note)
		}
		p.mu.Lock()
		p.results[d.URL] = time.Now().UTC().Format(time.RFC3339) + " " + msg
		p.mu.Unlock()
		p.s.log.Info("directory.register", "dir", d.URL, "result", msg)
	}
	p.mu.Lock()
	p.lastCard = card.Text
	p.mu.Unlock()
	if ok == 0 && firstErr != nil {
		return pending, firstErr
	}
	return pending, nil
}

func (p *publisher) status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := map[string]string{}
	for k, v := range p.results {
		res[k] = v
	}
	return map[string]any{"directories": res, "listSeq": p.list.Seq}
}
