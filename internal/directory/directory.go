// Package directory 是名录服务（spec/09）：收节点名片、探测、回答「附近有哪些节点」、和别的名录同步。
// 只由项目方运营。名录改不了名片，只能收或拒；探测结果、下架记录由名录自己签名，认证记录由根密钥签名。
package directory

import (
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tablenet/pkg/proto"
)

type Config struct {
	Region     string        // 名录所在地区；它兼任这个地区所属探测区的探测点
	DataDir    string        //
	MinVersion string        // 节点软件低于它时，在登记回话里提醒升级
	MinNote    string        //
	ProbeEvery time.Duration // 默认 5 分钟
	PullEvery  time.Duration // 默认 5 秒
	// 登记限流：同一地址段每小时最多几次新登记、同一地址最多挂几台节点。
	NewPerPrefixPerHour int
	MaxNodesPerIP       int
	Roots               []string
}

func (c *Config) defaults() {
	if c.ProbeEvery == 0 {
		c.ProbeEvery = 5 * time.Minute
	}
	if c.PullEvery == 0 {
		c.PullEvery = 5 * time.Second
	}
	if c.NewPerPrefixPerHour == 0 {
		c.NewPerPrefixPerHour = 5
	}
	if c.MaxNodesPerIP == 0 {
		c.MaxNodesPerIP = 3
	}
}

// 信任档
const (
	TierVerified = "verified"
	TierStable   = "stable"
	TierNew      = "new"
)

const (
	stableAge    = 7 * 24 * time.Hour
	stableUptime = 0.9
	hideAfter    = 3 // 某个探测区连续几次连不上就对那个区隐藏
)

type zoneProbe struct {
	obs    proto.Observation
	signed proto.SignedBy
	fails  int  // 连续失败次数
	everOK bool // 这个区连上过它
	change int64
}

type entry struct {
	card      proto.Signed
	c         proto.NodeCard
	firstSeen time.Time
	change    int64
	probes    map[string]*zoneProbe
	hours     map[int64]bool // 哪些小时里有有效名片（算在线率），只留最近 7 天
	ip        string         // 登记来源地址（只在内存里，用于限流，不落盘、不外传）
}

type delistRec struct {
	d      proto.Delisting
	signed proto.SignedBy
	change int64
}

type endorseRec struct {
	e      proto.Endorsement
	signed proto.SignedBy
	change int64
}

type Directory struct {
	mu   sync.Mutex
	cfg  Config
	key  ed25519.PrivateKey
	ID   string
	zone string
	log  *slog.Logger
	now  func() time.Time

	list     proto.DirectoryList
	listText proto.SignedList

	nodes     map[string]*entry
	delist    map[string]*delistRec
	endorse   map[string]*endorseRec
	change    int64
	cursors   map[string]int64       // 别的名录 → 拉到哪个变更序号
	newByPfx  map[string][]time.Time // 地址段 → 最近一小时的新登记时间
	probeWant chan string            // 要立刻探测的节点
}

func New(cfg Config, key ed25519.PrivateKey, list proto.SignedList, log *slog.Logger) (*Directory, error) {
	cfg.defaults()
	l, err := proto.VerifyDirectoryList(list, cfg.Roots, time.Now())
	if err != nil {
		return nil, err
	}
	id := proto.IDOfKey(key)
	if !l.HasDirectory(id) {
		return nil, fmt.Errorf("本名录（%s）不在名录清单里", id)
	}
	if !proto.ValidRegion(cfg.Region) {
		return nil, fmt.Errorf("地区 %q 不认", cfg.Region)
	}
	return &Directory{cfg: cfg, key: key, ID: id, zone: proto.ProbeZone(cfg.Region), log: log, now: time.Now,
		list: l, listText: list, nodes: map[string]*entry{}, delist: map[string]*delistRec{}, endorse: map[string]*endorseRec{},
		cursors: map[string]int64{}, newByPfx: map[string][]time.Time{}, probeWant: make(chan string, 1024)}, nil
}

func (d *Directory) bump() int64 { d.change++; return d.change }

// ---- 登记 ----

type RegisterResult struct {
	Seq     int64    `json:"seq"`
	Listed  bool     `json:"listed"`
	Reasons []string `json:"reasons"`
	Upgrade *Upgrade `json:"upgrade,omitempty"`
}

type Upgrade struct {
	Version string `json:"version"`
	Note    string `json:"note"`
}

// Register 收一张节点自己送来的名片。ip 是请求来源，只用来限流。
func (d *Directory) Register(card proto.Signed, ip string) (RegisterResult, error) {
	now := d.now()
	c, err := proto.VerifyCard(card, now)
	if err != nil {
		return RegisterResult{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.nodes[c.NodeID]
	if e == nil {
		if why := d.admit(ip, now); why != "" {
			return RegisterResult{Seq: c.Seq, Reasons: []string{why}}, nil
		}
	}
	accepted, endpointsChanged := d.accept(card, c, now)
	e = d.nodes[c.NodeID]
	if ip != "" {
		e.ip = ip
	}
	if !accepted && c.Seq < e.c.Seq {
		return RegisterResult{Seq: e.c.Seq, Listed: d.listedAnywhere(e), Reasons: []string{"stale_seq"}}, nil
	}
	if endpointsChanged {
		select {
		case d.probeWant <- c.NodeID:
		default:
		}
	}
	res := RegisterResult{Seq: e.c.Seq, Listed: d.listedAnywhere(e)}
	if !res.Listed {
		res.Reasons = d.reasons(e)
	}
	if d.cfg.MinVersion != "" && versionLess(c.Software.Version, d.cfg.MinVersion) {
		res.Upgrade = &Upgrade{Version: d.cfg.MinVersion, Note: d.cfg.MinNote}
	}
	return res, nil
}

// admit：新节点的登记限流。
func (d *Directory) admit(ip string, now time.Time) string {
	if ip == "" {
		return ""
	}
	n := 0
	for _, e := range d.nodes {
		if e.ip == ip {
			n++
		}
	}
	if n >= d.cfg.MaxNodesPerIP {
		return "too_many_nodes_on_ip"
	}
	pfx := prefixOf(ip)
	recent := d.newByPfx[pfx][:0]
	for _, t := range d.newByPfx[pfx] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= d.cfg.NewPerPrefixPerHour {
		d.newByPfx[pfx] = recent
		return "rate_limited"
	}
	d.newByPfx[pfx] = append(recent, now)
	return ""
}

// prefixOf：IPv4 取 /24，IPv6 取 /48。
func prefixOf(ip string) string {
	if strings.Contains(ip, ":") {
		parts := strings.Split(ip, ":")
		if len(parts) > 3 {
			parts = parts[:3]
		}
		return strings.Join(parts, ":")
	}
	if i := strings.LastIndexByte(ip, '.'); i > 0 {
		return ip[:i]
	}
	return ip
}

// accept：比 seq 收下更新的名片（自己收到的和同步来的都走这里）。返回是否收下、地址有没有变。
func (d *Directory) accept(card proto.Signed, c proto.NodeCard, now time.Time) (bool, bool) {
	e := d.nodes[c.NodeID]
	if e == nil {
		e = &entry{firstSeen: now, probes: map[string]*zoneProbe{}, hours: map[int64]bool{}}
		d.nodes[c.NodeID] = e
	} else if c.Seq <= e.c.Seq {
		return false, false
	}
	changed := e.c.Seq == 0 || !sameEndpoints(e.c.Endpoints, c.Endpoints)
	e.card, e.c, e.change = card, c, d.bump()
	e.hours[now.Unix()/3600] = true
	if changed {
		e.probes = map[string]*zoneProbe{} // 地址变了，旧的探测结果作废
	}
	return true, changed
}

func sameEndpoints(a, b []proto.Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (d *Directory) listedAnywhere(e *entry) bool {
	for _, p := range e.probes {
		if p.obs.OK {
			return true
		}
	}
	return false
}

func (d *Directory) reasons(e *entry) []string {
	if len(e.probes) == 0 {
		return []string{"probe_pending"}
	}
	out := []string{}
	for _, z := range proto.ProbeZones {
		if p := e.probes[z]; p != nil && !p.obs.OK {
			out = append(out, "unreachable_from:"+z)
		}
	}
	return out
}

// ---- 探测结果 ----

// AddObservation 收一条探测结果（名录自己的、同步来的、独立探测点报的都走这里）。
func (d *Directory) AddObservation(s proto.SignedBy) error {
	var o proto.Observation
	if err := s.Verify(proto.CtxObservation, s.Signer, &o); err != nil {
		return err
	}
	zone, ok := d.list.ProbeZoneOf(s.Signer)
	if !ok || zone != o.Zone {
		return fmt.Errorf("探测者 %s 不在名录清单里，或探测区不符", s.Signer)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.nodes[o.NodeID]
	if e == nil || d.now().Sub(o.At) > 30*time.Minute || o.At.After(d.now().Add(5*time.Minute)) {
		return nil // 不认识的节点，或者太旧（太新）的观测
	}
	p := e.probes[o.Zone]
	if p != nil && !o.At.After(p.obs.At) {
		return nil
	}
	np := &zoneProbe{obs: o, signed: s, everOK: o.OK, change: d.bump()}
	if p != nil {
		np.everOK = np.everOK || p.everOK
		if !o.OK {
			np.fails = p.fails + 1
		}
	} else if !o.OK {
		np.fails = 1
	}
	e.probes[o.Zone] = np
	return nil
}

// ---- 下架、认证 ----

func (d *Directory) AddDelisting(s proto.SignedBy) error {
	var x proto.Delisting
	if err := s.Verify(proto.CtxDelisting, s.Signer, &x); err != nil {
		return err
	}
	if !d.list.HasDirectory(s.Signer) {
		return fmt.Errorf("下架记录不是清单里的名录签的")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if old := d.delist[x.NodeID]; old != nil && !x.At.After(old.d.At) {
		return nil
	}
	d.delist[x.NodeID] = &delistRec{d: x, signed: s, change: d.bump()}
	return nil
}

func (d *Directory) AddEndorsement(s proto.SignedBy) error {
	var x proto.Endorsement
	trusted := false
	for _, r := range d.cfg.Roots {
		trusted = trusted || r == s.Signer
	}
	if !trusted {
		return fmt.Errorf("认证记录必须由根密钥签")
	}
	if err := s.Verify(proto.CtxEndorsement, s.Signer, &x); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if old := d.endorse[x.NodeID]; old != nil && !x.IssuedAt.After(old.e.IssuedAt) {
		return nil
	}
	d.endorse[x.NodeID] = &endorseRec{e: x, signed: s, change: d.bump()}
	return nil
}

// Delist 由本名录签发一条下架（或撤销下架）记录。
func (d *Directory) Delist(nodeID string, zones []string, reason string, lifted bool) error {
	s, err := proto.SignBy(proto.CtxDelisting, d.key, proto.Delisting{NodeID: nodeID, Zones: zones, Reason: reason, Lifted: lifted, At: d.now().UTC()})
	if err != nil {
		return err
	}
	return d.AddDelisting(s)
}

// ---- 列表 ----

type Query struct {
	Region     string
	Protocol   string
	Venue      string
	Limit      int
	IncludeNew bool
	NodeID     string
	Name       string
	All        bool // 探测点用：所有没过期的名片，不过滤
}

type ProbeView struct {
	Zone  string    `json:"zone"`
	OK    bool      `json:"ok"`
	RTTMs int       `json:"rttMs"`
	At    time.Time `json:"at"`
}

type Listing struct {
	Card   proto.Signed `json:"card"`
	Tier   string       `json:"tier"`
	Probes []ProbeView  `json:"probes"`
}

func (d *Directory) tier(id string, e *entry, now time.Time) string {
	if r := d.endorse[id]; r != nil && now.Before(r.e.ExpiresAt) {
		return TierVerified
	}
	age := now.Sub(e.firstSeen)
	if age < stableAge {
		return TierNew
	}
	from := now.Add(-stableAge).Unix() / 3600
	to := now.Unix() / 3600
	seen := 0
	for h := from; h <= to; h++ {
		if e.hours[h] {
			seen++
		}
	}
	if float64(seen)/float64(to-from+1) >= stableUptime && d.listedAnywhere(e) {
		return TierStable
	}
	return TierNew
}

func tierRank(t string) int {
	switch t {
	case TierVerified:
		return 0
	case TierStable:
		return 1
	}
	return 2
}

// zoneProbed：网络里有没有这个探测区的探测者。没有的话就不按这个区过滤（任一区连得上就算）。
func (d *Directory) zoneProbed(zone string) bool {
	for _, x := range d.list.Directories {
		if proto.ProbeZone(x.Region) == zone {
			return true
		}
	}
	for _, p := range d.list.Probes {
		if p.Zone == zone {
			return true
		}
	}
	return false
}

func (d *Directory) reachable(e *entry, zone string) (bool, int) {
	if d.zoneProbed(zone) {
		p := e.probes[zone]
		switch {
		case p == nil, p.fails >= hideAfter, !p.obs.OK && !p.everOK:
			return false, 0 // 没探测过、连续失败、或者这个区从没连上过
		case !p.obs.OK:
			return true, 9999 // 偶尔失败、还没到隐藏的次数：排在后面
		}
		return true, p.obs.RTTMs
	}
	best := -1
	for _, p := range e.probes {
		if p.obs.OK && (best < 0 || p.obs.RTTMs < best) {
			best = p.obs.RTTMs
		}
	}
	return best >= 0, best
}

func (d *Directory) Nodes(q Query) []Listing {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	zone := proto.ProbeZone(q.Region)
	type cand struct {
		l     Listing
		rank  int
		load  float64
		rtt   int
		doubt bool
	}
	out := []cand{}
	for id, e := range d.nodes {
		if !now.Before(e.c.ExpiresAt) {
			continue
		}
		if q.All {
			out = append(out, cand{l: d.listing(id, e, now)})
			continue
		}
		if q.NodeID != "" && id != q.NodeID {
			continue
		}
		if q.Name != "" && !strings.Contains(strings.ToLower(e.c.Name), strings.ToLower(q.Name)) {
			continue
		}
		if x := d.delist[id]; x != nil && !x.d.Lifted && (len(x.d.Zones) == 0 || contains(x.d.Zones, zone)) {
			continue
		}
		direct := q.NodeID != "" || q.Name != ""
		if !direct && e.c.Status != proto.CardOpen {
			continue
		}
		if q.Protocol != "" && !contains(e.c.Protocols, q.Protocol) {
			continue
		}
		if q.Venue != "" && !hasVenue(e.c, q.Venue) {
			continue
		}
		ok, rtt := d.reachable(e, zone)
		if !ok {
			continue
		}
		tier := d.tier(id, e, now)
		if tier == TierNew && !q.IncludeNew && !direct {
			continue
		}
		capacity := e.c.Capacity.Players
		if capacity <= 0 {
			capacity = 500
		}
		doubt := proto.ProbeZone(e.c.Region) == zone && zone != "intl" && rtt > 150
		out = append(out, cand{l: d.listing(id, e, now), rank: tierRank(tier), load: float64(e.c.Load.Players) / float64(capacity), rtt: rtt, doubt: doubt})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.doubt != b.doubt {
			return !a.doubt
		}
		if a.load >= 0.9 != (b.load >= 0.9) { // 快满的排后面
			return a.load < 0.9
		}
		if a.rtt != b.rtt {
			return a.rtt < b.rtt
		}
		return a.load < b.load
	})
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if q.All {
		limit = len(out)
	}
	res := []Listing{}
	for i := 0; i < len(out) && i < limit; i++ {
		res = append(res, out[i].l)
	}
	return res
}

func (d *Directory) listing(id string, e *entry, now time.Time) Listing {
	l := Listing{Card: e.card, Tier: d.tier(id, e, now), Probes: []ProbeView{}}
	for _, z := range proto.ProbeZones {
		if p := e.probes[z]; p != nil {
			l.Probes = append(l.Probes, ProbeView{Zone: z, OK: p.obs.OK, RTTMs: p.obs.RTTMs, At: p.obs.At})
		}
	}
	return l
}

func hasVenue(c proto.NodeCard, v string) bool {
	for _, x := range c.Venues {
		if x.PlatformID == v {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// ---- 变更流（给别的名录拉） ----

type Changes struct {
	Next         int64            `json:"next"`
	Cards        []proto.Signed   `json:"cards"`
	Observations []proto.SignedBy `json:"observations"`
	Delistings   []proto.SignedBy `json:"delistings"`
	Endorsements []proto.SignedBy `json:"endorsements"`
}

func (d *Directory) ChangesSince(since int64) Changes {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch := Changes{Next: d.change, Cards: []proto.Signed{}, Observations: []proto.SignedBy{}, Delistings: []proto.SignedBy{}, Endorsements: []proto.SignedBy{}}
	now := d.now()
	for _, e := range d.nodes {
		if !now.Before(e.c.ExpiresAt) {
			continue
		}
		if e.change > since {
			ch.Cards = append(ch.Cards, e.card)
		}
		for _, p := range e.probes {
			if p.change > since {
				ch.Observations = append(ch.Observations, p.signed)
			}
		}
	}
	for _, x := range d.delist {
		if x.change > since {
			ch.Delistings = append(ch.Delistings, x.signed)
		}
	}
	for _, x := range d.endorse {
		if x.change > since {
			ch.Endorsements = append(ch.Endorsements, x.signed)
		}
	}
	return ch
}

// Apply 收一批从别的名录拉来的变更：名片照样验签、比 seq；探测、下架、认证照样验签名者。
func (d *Directory) Apply(ch Changes) (cards, obs int) {
	now := d.now()
	for _, s := range ch.Cards {
		c, err := proto.VerifyCard(s, now)
		if err != nil {
			continue
		}
		d.mu.Lock()
		accepted, changed := d.accept(s, c, now)
		d.mu.Unlock()
		if accepted {
			cards++
			if changed {
				select {
				case d.probeWant <- c.NodeID:
				default:
				}
			}
		}
	}
	for _, s := range ch.Observations {
		if d.AddObservation(s) == nil {
			obs++
		}
	}
	for _, s := range ch.Delistings {
		_ = d.AddDelisting(s)
	}
	for _, s := range ch.Endorsements {
		_ = d.AddEndorsement(s)
	}
	return cards, obs
}

// ---- 清理与状态 ----

// Prune 删掉过期一天以上的名片，修剪 7 天以前的在线记录。
func (d *Directory) Prune() {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	oldest := now.Add(-stableAge).Unix()/3600 - 1
	for id, e := range d.nodes {
		if now.Sub(e.c.ExpiresAt) > 24*time.Hour {
			delete(d.nodes, id)
			continue
		}
		for h := range e.hours {
			if h < oldest {
				delete(e.hours, h)
			}
		}
	}
}

func (d *Directory) Status() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	live, open := 0, 0
	for _, e := range d.nodes {
		if now.Before(e.c.ExpiresAt) {
			live++
			if e.c.Status == proto.CardOpen {
				open++
			}
		}
	}
	return map[string]any{"id": d.ID, "zone": d.zone, "listSeq": d.list.Seq, "nodes": live, "open": open,
		"delisted": len(d.delist), "endorsed": len(d.endorse), "change": d.change, "cursors": d.cursors}
}

// versionLess：按点分数字比较版本号（0.2.0 < 0.10.0），后缀（-dev）忽略。
func versionLess(a, b string) bool {
	pa, pb := splitVer(a), splitVer(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func splitVer(v string) [3]int {
	v = strings.SplitN(v, "-", 2)[0]
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}
