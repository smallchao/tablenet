// SPDX-License-Identifier: MIT

package xmage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// Event 是驱动发来的一条 XMage 回调，或者会话断线（M 为 "lost"）。
type Event struct {
	S   string          `json:"s"`
	M   string          `json:"m"`
	Obj string          `json:"obj"`
	D   json.RawMessage `json:"d"`
}

// Driver 是 Java 驱动进程（driver/src/tablenet/xmage/Driver.java）的 Go 一侧：
// 发请求、等回话，把回调按会话名分给订阅者。
type Driver struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	inMu sync.Mutex
	next atomic.Int64

	mu      sync.Mutex
	waiting map[int64]chan map[string]any
	subs    map[string]chan Event
	done    chan struct{}
	err     error
}

// DriverConfig：怎么起 Java 驱动。
type DriverConfig struct {
	Java     string // java 可执行文件，默认 PATH 里的 java
	Jar      string // 驱动的 jar（build.sh 打出来的 tablenet-xmage-driver.jar）或 classes 目录
	XMageLib string // XMage 服务器的 lib 目录（mage-common 等 jar 都在这）
	WorkDir  string // 驱动的工作目录：本地牌库 db/ 建在这里
	Stderr   io.Writer
}

// StartDriver 起驱动进程。
func StartDriver(cfg DriverConfig) (*Driver, error) {
	if cfg.Java == "" {
		cfg.Java = "java"
	}
	if cfg.XMageLib == "" {
		return nil, errors.New("没给 XMage 的 lib 目录（XMAGE_LIB）")
	}
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	cp := cfg.Jar + sep + filepath.Join(cfg.XMageLib, "*")
	args := []string{"-Xmx1536m", "-Djava.awt.headless=true", "-Dfile.encoding=UTF-8",
		// XMage 的 JBoss Remoting 要反射进 java.io 等（不加会报 Wrong java version）
		"--add-opens", "java.base/java.io=ALL-UNNAMED", "--add-opens", "java.base/java.lang=ALL-UNNAMED",
		"--add-opens", "java.base/java.util=ALL-UNNAMED",
		"-cp", cp, "tablenet.xmage.Driver"}
	cmd := exec.Command(cfg.Java, args...)
	if cfg.WorkDir != "" {
		if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
			return nil, err
		}
		cmd.Dir = cfg.WorkDir
	}
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("起不了 Java 驱动：%w", err)
	}
	d := &Driver{cmd: cmd, in: in, waiting: map[int64]chan map[string]any{}, subs: map[string]chan Event{}, done: make(chan struct{})}
	go d.read(out)
	return d, nil
}

func (d *Driver) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20) // 局面一行可能上 MB
	for sc.Scan() {
		line := sc.Bytes()
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if re, ok := m["re"].(float64); ok {
			d.mu.Lock()
			ch := d.waiting[int64(re)]
			delete(d.waiting, int64(re))
			d.mu.Unlock()
			if ch != nil {
				ch <- m
			}
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if m["ev"] == "lost" {
			ev.M = "lost"
		}
		d.mu.Lock()
		sub := d.subs[ev.S]
		d.mu.Unlock()
		if sub != nil {
			select {
			case sub <- ev:
			case <-d.done:
			}
		}
	}
	d.mu.Lock()
	d.err = errors.New("Java 驱动退出了")
	if e := sc.Err(); e != nil {
		d.err = fmt.Errorf("Java 驱动退出了：%w", e)
	}
	for _, ch := range d.waiting {
		close(ch)
	}
	d.waiting = map[int64]chan map[string]any{}
	d.mu.Unlock()
	close(d.done)
}

// Done 在驱动进程退出时关闭。
func (d *Driver) Done() <-chan struct{} { return d.done }

func (d *Driver) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// Subscribe：会话 s 的回调都送到返回的通道里（缓冲大，局面帧密）。再订阅同名会话会顶掉旧的。
func (d *Driver) Subscribe(s string) <-chan Event {
	ch := make(chan Event, 1024)
	d.mu.Lock()
	d.subs[s] = ch
	d.mu.Unlock()
	return ch
}

func (d *Driver) Unsubscribe(s string) {
	d.mu.Lock()
	delete(d.subs, s)
	d.mu.Unlock()
}

// Call 发一个请求等回话。body 里不用带 id 和 op。
func (d *Driver) Call(ctx context.Context, op string, body map[string]any) (map[string]any, error) {
	id := d.next.Add(1)
	req := map[string]any{"id": id, "op": op}
	for k, v := range body {
		req[k] = v
	}
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ch := make(chan map[string]any, 1)
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return nil, d.err
	}
	d.waiting[id] = ch
	d.mu.Unlock()
	d.inMu.Lock()
	_, err = d.in.Write(append(line, '\n'))
	d.inMu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, d.Err()
		}
		if ok, _ := m["ok"].(bool); !ok {
			msg, _ := m["error"].(string)
			return nil, &DriverError{Op: op, Msg: msg}
		}
		return m, nil
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.waiting, id)
		d.mu.Unlock()
		return nil, ctx.Err()
	}
}

// DriverError 是 XMage 那边拒绝或者出错。
type DriverError struct {
	Op, Msg string
}

func (e *DriverError) Error() string { return e.Msg }

// Close 关驱动：关掉标准输入，它会断开所有会话后退出。
func (d *Driver) Close() {
	_ = d.in.Close()
	select {
	case <-d.done:
	default:
	}
	go func() { _ = d.cmd.Wait() }()
}

// decodeAs 把回话里的某个字段解成 v。
func decodeAs(m map[string]any, key string, v any) error {
	b, err := json.Marshal(m[key])
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
