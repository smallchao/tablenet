package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"tablenet/pkg/proto"
)

// Version 是节点版本，构建时可以用 -ldflags "-X tablenet/internal/node.Version=..." 覆盖。
var Version = "0.1.0-dev"

const (
	idleBeforePing = 30 * time.Second
	idleLimit      = 90 * time.Second
	writeTimeout   = 15 * time.Second
)

// Server 把 Hub 挂到 HTTP 上：WebSocket、公开的几个 HTTP 端点、本机管理接口、代管的适配器。
type Server struct {
	Hub    *Hub
	cfg    Config
	log    *slog.Logger
	token  string
	public *http.Server
	admin  *http.Server
	wg     sync.WaitGroup
}

func NewServer(cfg Config, log *slog.Logger) (*Server, error) {
	key, err := proto.LoadOrCreateKey(cfg.Node.KeyFile)
	if err != nil {
		return nil, err
	}
	hub, err := NewHub(cfg, key, log)
	if err != nil {
		return nil, err
	}
	tok, err := hub.st.adminToken()
	if err != nil {
		return nil, err
	}
	return &Server{Hub: hub, cfg: cfg, log: log, token: tok}, nil
}

// Handler 是公开端口上的全部路由（测试里直接拿它起 httptest）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(proto.WSPath, s.serveWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/node.json", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Hub.NodeInfo()) })
	mux.HandleFunc("/stats.json", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Hub.Snapshot()) })
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.Listen.TrustForwardedFor {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			return strings.TrimSpace(strings.Split(fwd, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	c, why := s.Hub.Open(ip)
	if c == nil {
		http.Error(w, why, http.StatusServiceUnavailable)
		return
	}
	// 身份靠签名，不靠 Origin，所以浏览器端平台（跨域）也放行。
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.Hub.Close(c)
		return
	}
	ws.SetReadLimit(proto.MaxMessageBytes + 4)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var last struct {
		sync.Mutex
		at time.Time
	}
	last.at = time.Now()
	writerDone := make(chan struct{})
	go func() { // 写协程：节点发的东西都从这里出去
		defer close(writerDone)
		pinger := time.NewTicker(idleBeforePing)
		defer pinger.Stop()
		write := func(m outMsg) error {
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			defer wcancel()
			typ := websocket.MessageText
			if m.binary {
				typ = websocket.MessageBinary
			}
			return ws.Write(wctx, typ, m.data)
		}
		for {
			select {
			case m := <-c.out:
				if write(m) != nil {
					cancel()
					return
				}
			case <-pinger.C:
				last.Lock()
				idle := time.Since(last.at)
				last.Unlock()
				if idle >= idleBeforePing && write(outMsg{data: proto.New("ping", "", "", nil).Bytes()}) != nil {
					cancel()
					return
				}
			case <-c.done:
				for { // 把队列里剩下的（比如最后一句 error）发完再关
					select {
					case m := <-c.out:
						if write(m) != nil {
							_ = ws.CloseNow()
							return
						}
					default:
						_ = ws.Close(websocket.StatusPolicyViolation, truncate(c.reason, 120))
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		rctx, rcancel := context.WithTimeout(ctx, idleLimit)
		typ, data, err := ws.Read(rctx)
		rcancel()
		if err != nil {
			break
		}
		last.Lock()
		last.at = time.Now()
		last.Unlock()
		if typ == websocket.MessageText {
			s.Hub.HandleText(c, data)
		} else {
			s.Hub.HandleBinary(c, data)
		}
	}
	s.Hub.Close(c)
	cancel()
	<-writerDone
	_ = ws.CloseNow()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- 管理接口（只监听本机，带令牌） ----

func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	auth := func(f func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				http.Error(w, "令牌不对", http.StatusUnauthorized)
				return
			}
			f(w, r)
		}
	}
	post := func(f func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return auth(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "要用 POST", http.StatusMethodNotAllowed)
				return
			}
			f(w, r)
		})
	}
	ok := func(w http.ResponseWriter, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
	mux.HandleFunc("/status", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Hub.Snapshot()) }))
	mux.HandleFunc("/rooms", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Hub.RoomsAdmin()) }))
	mux.HandleFunc("/bans", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Hub.Bans()) }))
	mux.HandleFunc("/adapters", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Hub.NodeInfo()["platforms"]) }))
	mux.HandleFunc("/close", post(func(w http.ResponseWriter, r *http.Request) {
		if !s.Hub.AdminCloseRoom(r.URL.Query().Get("room")) {
			ok(w, errors.New("没有这个房间"))
			return
		}
		ok(w, nil)
	}))
	mux.HandleFunc("/ban", post(func(w http.ResponseWriter, r *http.Request) {
		ok(w, s.Hub.AdminBan(r.URL.Query().Get("player"), r.URL.Query().Get("reason")))
	}))
	mux.HandleFunc("/unban", post(func(w http.ResponseWriter, r *http.Request) {
		ok(w, s.Hub.AdminUnban(r.URL.Query().Get("player")))
	}))
	mux.HandleFunc("/adapter-key", post(func(w http.ResponseWriter, r *http.Request) {
		ok(w, s.Hub.AdminSetAdapterKey(r.URL.Query().Get("platform"), r.URL.Query().Get("key")))
	}))
	return mux
}

// ---- 跑起来 ----

// Run 监听公开端口和管理端口、每秒跑一次 Tick、起代管的适配器，直到 ctx 结束。
func (s *Server) Run(ctx context.Context) error {
	s.public = &http.Server{Addr: s.cfg.Listen.WS, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() {
		var err error
		if s.cfg.Listen.TLSCert != "" {
			err = s.public.ListenAndServeTLS(s.cfg.Listen.TLSCert, s.cfg.Listen.TLSKey)
		} else {
			err = s.public.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	if s.cfg.Listen.Admin != "" {
		s.admin = &http.Server{Addr: s.cfg.Listen.Admin, Handler: s.AdminHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := s.admin.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	s.log.Info("node.start", "nodeId", s.Hub.NodeID, "ws", s.cfg.Listen.WS, "admin", s.cfg.Listen.Admin,
		"tls", s.cfg.Listen.TLSCert != "", "version", Version)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	s.superviseAdapters(ctx)
	for {
		select {
		case <-ticker.C:
			s.Hub.Tick()
		case err := <-errs:
			return err
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.public.Shutdown(sctx)
			if s.admin != nil {
				_ = s.admin.Shutdown(sctx)
			}
			s.wg.Wait()
			return nil
		}
	}
}

// localURL 是代管的适配器连回本节点用的地址。
func (s *Server) localURL() string {
	_, port, _ := net.SplitHostPort(s.cfg.Listen.WS)
	if s.cfg.Listen.TLSCert != "" {
		return "wss://" + net.JoinHostPort(s.cfg.Node.PublicHost, port) + proto.WSPath
	}
	return "ws://" + net.JoinHostPort("127.0.0.1", port) + proto.WSPath
}

// superviseAdapters 把配置里带 run 的适配器当子进程起，崩了 5 秒后重启，节点退出时一起退出。
func (s *Server) superviseAdapters(ctx context.Context) {
	for id, a := range s.cfg.Adapters {
		if a.Run == "" {
			continue
		}
		id, a := id, a
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for ctx.Err() == nil {
				cmd := exec.CommandContext(ctx, a.Run, a.Args...)
				cmd.Env = append(os.Environ(), "TABLENET_NODE_URL="+s.localURL(), "TABLENET_PLATFORM="+id)
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
				s.log.Info("adapter.spawn", "platform", id, "run", a.Run)
				err := cmd.Run()
				if ctx.Err() != nil {
					return
				}
				s.log.Warn("adapter.exit", "platform", id, "err", err)
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
}

// AdminToken 给命令行工具用。
func AdminToken(cfg Config) (string, error) {
	st, err := newStore(cfg.Node.DataDir)
	if err != nil {
		return "", err
	}
	return st.adminToken()
}
