package node

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// 计数器：只增不减，Prometheus 的 counter。
var (
	metricHandshakeFail atomic.Int64
	metricRateLimited   atomic.Int64
	metricRelayBytes    atomic.Int64
	metricRelayFrames   atomic.Int64
)

// serveMetrics：Prometheus 文本格式。只有计数和当前值，不含任何玩家信息。
func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	snap := s.Hub.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	gauge := func(name, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}
	counter := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	gauge("tablenet_connections", "当前连接数", snap["connections"])
	gauge("tablenet_players", "当前在线玩家", snap["players"])
	gauge("tablenet_rooms", "当前房间数", snap["rooms"])
	gauge("tablenet_channels", "当前对局通道数", snap["channels"])
	gauge("tablenet_queued", "排队中的人数", snap["queued"])
	draining := 0
	if d, _ := snap["draining"].(bool); d {
		draining = 1
	}
	gauge("tablenet_draining", "是否在排空（1 是）", draining)
	counter("tablenet_handshake_failures_total", "握手失败次数", metricHandshakeFail.Load())
	counter("tablenet_rate_limited_total", "被限流的消息数", metricRateLimited.Load())
	counter("tablenet_relay_bytes_total", "转发的对局字节数", metricRelayBytes.Load())
	counter("tablenet_relay_frames_total", "转发的对局帧数", metricRelayFrames.Load())
}
