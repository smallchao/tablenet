package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// store 是 data_dir 里的几份小文件。节点只存这些：黑名单、适配器公钥、战绩。对局内容、牌表、聊天一概不存。
type store struct {
	dir string
	mu  sync.Mutex
}

func newStore(dir string) (*store, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	return &store{dir: dir}, nil
}

func (s *store) path(name string) string { return filepath.Join(s.dir, name) }

func (s *store) readJSON(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSON 先写临时文件再改名，进程中途死掉也不会留下半个文件。
func (s *store) writeJSON(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(name + ".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

func (s *store) appendLine(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// adminToken 读管理令牌，没有就生成一个。
func (s *store) adminToken() (string, error) {
	p := s.path("admin.token")
	b, err := os.ReadFile(p)
	if err == nil && len(b) > 0 {
		return string(trimSpace(b)), nil
	}
	tok := hexString(randomBytes(24))
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = digits[c>>4], digits[c&15]
	}
	return string(out)
}
