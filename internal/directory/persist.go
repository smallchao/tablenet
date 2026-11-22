package directory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"tablenet/pkg/proto"
)

// 名录状态落盘：data_dir/directory.json。重启后名片、探测、下架、认证、同步游标、各节点的在线记录都接着用。
// 不存登记来源 IP。

type savedNode struct {
	Card      proto.Signed `json:"card"`
	FirstSeen time.Time    `json:"firstSeen"`
	Hours     []int64      `json:"hours"`
}

type savedDir struct {
	Nodes        []savedNode      `json:"nodes"`
	Observations []proto.SignedBy `json:"observations"`
	Delistings   []proto.SignedBy `json:"delistings"`
	Endorsements []proto.SignedBy `json:"endorsements"`
	Cursors      map[string]int64 `json:"cursors"`
}

func (d *Directory) Save(dir string) error {
	d.mu.Lock()
	s := savedDir{Cursors: map[string]int64{}}
	for _, e := range d.nodes {
		n := savedNode{Card: e.card, FirstSeen: e.firstSeen}
		for h := range e.hours {
			n.Hours = append(n.Hours, h)
		}
		s.Nodes = append(s.Nodes, n)
		for _, p := range e.probes {
			s.Observations = append(s.Observations, p.signed)
		}
	}
	for _, x := range d.delist {
		s.Delistings = append(s.Delistings, x.signed)
	}
	for _, x := range d.endorse {
		s.Endorsements = append(s.Endorsements, x.signed)
	}
	for k, v := range d.cursors {
		s.Cursors[k] = v
	}
	d.mu.Unlock()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "directory.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "directory.json"))
}

// Load 读回快照。过期的名片也读回来（保留首次出现时间和在线记录，信任档要用），只是不会出现在列表里。
func (d *Directory) Load(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "directory.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s savedDir
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	d.mu.Lock()
	for _, n := range s.Nodes {
		var c proto.NodeCard
		if json.Unmarshal([]byte(n.Card.Text), &c) != nil || n.Card.Verify(proto.CtxNodeCard, c.NodeID, nil) != nil {
			continue // 文件被改过
		}
		e := &entry{card: n.Card, c: c, firstSeen: n.FirstSeen, change: d.bump(), probes: map[string]*zoneProbe{}, hours: map[int64]bool{}}
		for _, h := range n.Hours {
			e.hours[h] = true
		}
		d.nodes[c.NodeID] = e
	}
	for k, v := range s.Cursors {
		d.cursors[k] = v
	}
	d.mu.Unlock()
	for _, o := range s.Observations {
		_ = d.AddObservation(o)
	}
	for _, x := range s.Delistings {
		_ = d.AddDelisting(x)
	}
	for _, x := range s.Endorsements {
		_ = d.AddEndorsement(x)
	}
	return nil
}
