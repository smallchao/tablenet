package directory

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tablenet/pkg/proto"
)

// HTTP 接口（spec/09）：
//
//	POST /dir/v1/register        {card: {text, sig}}            节点登记、续期
//	GET  /dir/v1/nodes?region=…  客户端查附近的节点
//	GET  /dir/v1/changes?since=N 名录之间同步
//	POST /dir/v1/observations    {observations: [...]}          独立探测点报结果
//	GET  /dir/v1/directories.json                                 名录清单（根密钥签的）
//	GET  /healthz
//
// 名录不记录查询者的 IP；登记来源 IP 只在内存里用于限流。

const maxBody = 1 << 20

func (d *Directory) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/dir/v1/directories.json", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, d.listText) })
	mux.HandleFunc("/dir/v1/register", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Card proto.Signed `json:"card"`
		}
		if !readJSON(w, r, &b) {
			return
		}
		res, err := d.Register(b.Card, clientIP(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("/dir/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		writeJSON(w, map[string]any{"nodes": d.Nodes(Query{Region: q.Get("region"), Protocol: q.Get("protocol"),
			Venue: q.Get("venue"), Limit: limit, IncludeNew: q.Get("include") == "new", NodeID: q.Get("id"),
			Name: q.Get("name"), All: q.Get("all") == "1"})})
	})
	mux.HandleFunc("/dir/v1/changes", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		writeJSON(w, d.ChangesSince(since))
	})
	mux.HandleFunc("/dir/v1/observations", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Observations []proto.SignedBy `json:"observations"`
		}
		if !readJSON(w, r, &b) {
			return
		}
		n := 0
		for _, s := range b.Observations {
			if d.AddObservation(s) == nil {
				n++
			}
		}
		writeJSON(w, map[string]int{"accepted": n})
	})
	return mux
}

// AdminHandler：只监听本机、带令牌。下架、撤销下架、加认证记录、看状态。
func (d *Directory) AdminHandler(token string) http.Handler {
	mux := http.NewServeMux()
	auth := func(f http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "令牌不对", http.StatusUnauthorized)
				return
			}
			f(w, r)
		}
	}
	mux.HandleFunc("/status", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, d.Status()) }))
	mux.HandleFunc("/nodes", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, d.Nodes(Query{All: true})) }))
	mux.HandleFunc("/delist", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var zones []string
		if z := q.Get("zones"); z != "" {
			zones = strings.Split(z, ",")
		}
		if err := d.Delist(q.Get("node"), zones, q.Get("reason"), q.Get("lift") == "1"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("/endorse", auth(func(w http.ResponseWriter, r *http.Request) {
		var s proto.SignedBy
		if !readJSON(w, r, &s) {
			return
		}
		if err := d.AddEndorsement(s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
	return mux
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "要用 POST", http.StatusMethodNotAllowed)
		return false
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		http.Error(w, "正文不是合法 JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(v)
}

// TrustForwardedFor：名录放在反向代理后面时打开。
var TrustForwardedFor = false

func clientIP(r *http.Request) string {
	if TrustForwardedFor {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- 同步 ----

// pullLoop：每 PullEvery 从清单里别的名录拉一次变更。
func (d *Directory) pullLoop(ctx context.Context) {
	cl := &http.Client{Timeout: 10 * time.Second}
	tick := time.NewTicker(d.cfg.PullEvery)
	defer tick.Stop()
	for {
		for _, peer := range d.list.Directories {
			if peer.ID == d.ID {
				continue
			}
			if err := d.pullOnce(ctx, cl, peer); err != nil {
				d.log.Debug("sync.pull", "peer", peer.URL, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (d *Directory) pullOnce(ctx context.Context, cl *http.Client, peer proto.DirectoryEntry) error {
	d.mu.Lock()
	since := d.cursors[peer.ID]
	d.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/dir/v1/changes?since=%d", strings.TrimRight(peer.URL, "/"), since), nil)
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	var ch Changes
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&ch); err != nil {
		return err
	}
	d.Apply(ch)
	d.mu.Lock()
	if ch.Next < since { // 对方重启、序号归零：从头再拉
		ch.Next = 0
	}
	d.cursors[peer.ID] = ch.Next
	d.mu.Unlock()
	return nil
}

// Run 起探测、同步、清理三个循环，直到 ctx 结束。
func (d *Directory) Run(ctx context.Context) {
	go d.probeLoop(ctx)
	go d.pullLoop(ctx)
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.Prune()
		}
	}
}
