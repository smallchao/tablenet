package directory

import (
	"context"
	"crypto/ed25519"
	"sync"
	"time"

	"tablenet/pkg/client"
	"tablenet/pkg/proto"
)

// ProbeNode 从本探测区连一次节点：TLS 按名片里的指纹校验、走 Tablenet 握手、要求节点证明身份，记下往返时间。
// 任一地址通过就算连得上。结果由 key（名录或独立探测点）签名。
func ProbeNode(ctx context.Context, card proto.NodeCard, zone string, key ed25519.PrivateKey) (proto.SignedBy, error) {
	o := proto.Observation{NodeID: card.NodeID, Seq: card.Seq, Zone: zone, At: time.Now().UTC()}
	throwaway := proto.GenerateKey() // 探测不带真身份
	for i := range card.Endpoints {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		start := time.Now()
		c, err := client.DialNode(pctx, card, i, throwaway, map[string]any{
			"role": "spectator-only", "displayName": "tablenet-probe", "client": map[string]string{"name": "tablenet-probe"},
		})
		cancel()
		if err != nil {
			o.Reason = err.Error()
			continue
		}
		o.OK, o.RTTMs, o.Reason = true, int(time.Since(start).Milliseconds()), ""
		c.Close()
		break
	}
	if len(o.Reason) > 200 {
		o.Reason = o.Reason[:200]
	}
	return proto.SignBy(proto.CtxObservation, key, o)
}

// probeAll 探测一批节点，并发 16。
func probeAll(ctx context.Context, cards []proto.NodeCard, zone string, key ed25519.PrivateKey, report func(proto.SignedBy)) {
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, c := range cards {
		c := c
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if s, err := ProbeNode(ctx, c, zone, key); err == nil {
				report(s)
			}
		}()
	}
	wg.Wait()
}

// liveCards：现在有效的全部名片（给探测用）。
func (d *Directory) liveCards() []proto.NodeCard {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	out := []proto.NodeCard{}
	for _, e := range d.nodes {
		if now.Before(e.c.ExpiresAt) {
			out = append(out, e.c)
		}
	}
	return out
}

func (d *Directory) cardOf(id string) (proto.NodeCard, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.nodes[id]
	if e == nil {
		return proto.NodeCard{}, false
	}
	return e.c, true
}

// probeLoop：新登记、地址变了的节点马上探；全部节点每 ProbeEvery 抽探一遍。
func (d *Directory) probeLoop(ctx context.Context) {
	report := func(s proto.SignedBy) {
		if err := d.AddObservation(s); err != nil {
			d.log.Warn("probe.add", "err", err)
		}
	}
	tick := time.NewTicker(d.cfg.ProbeEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-d.probeWant:
			if c, ok := d.cardOf(id); ok {
				go probeAll(ctx, []proto.NodeCard{c}, d.zone, d.key, report)
			}
		case <-tick.C:
			go probeAll(ctx, d.liveCards(), d.zone, d.key, report)
		}
	}
}
