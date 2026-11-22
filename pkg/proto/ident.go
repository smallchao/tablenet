package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const idPrefix = "ed25519:"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// RandomBytes 取 n 个系统随机字节。
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("系统随机数不可用：" + err.Error())
	}
	return b
}

func GenerateKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(RandomBytes(ed25519.SeedSize))
}

// IDOf：公钥 → 编号（ed25519: 加小写无填充 base32）。
func IDOf(pub ed25519.PublicKey) string {
	return idPrefix + strings.ToLower(b32.EncodeToString(pub))
}

func IDOfKey(priv ed25519.PrivateKey) string {
	return IDOf(priv.Public().(ed25519.PublicKey))
}

// KeyOf：编号 → 公钥。格式不对返回 nil。
func KeyOf(id string) ed25519.PublicKey {
	body, ok := strings.CutPrefix(id, idPrefix)
	if !ok || body != strings.ToLower(body) {
		return nil
	}
	raw, err := b32.DecodeString(strings.ToUpper(body))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

func B64(b []byte) string { return "b64:" + base64.StdEncoding.EncodeToString(b) }

func UnB64(s string) ([]byte, bool) {
	body, ok := strings.CutPrefix(s, "b64:")
	if !ok {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(body)
	return b, err == nil
}

// HelloMessage 是握手签名的内容："tablenet/1 hello" || nonce || nodeId。
func HelloMessage(nonce []byte, nodeID string) []byte {
	m := append([]byte("tablenet/1 hello"), nonce...)
	return append(m, nodeID...)
}

func SignHello(priv ed25519.PrivateKey, nonce []byte, nodeID string) string {
	return B64(ed25519.Sign(priv, HelloMessage(nonce, nodeID)))
}

func VerifyHello(playerID, sig string, nonce []byte, nodeID string) bool {
	return verify(playerID, sig, HelloMessage(nonce, nodeID))
}

// ResultMessage 是战绩确认签名的内容："tablenet/1 result" || 节点给出的战绩原文。
func ResultMessage(reportText string) []byte {
	return append([]byte("tablenet/1 result"), reportText...)
}

func SignResult(priv ed25519.PrivateKey, reportText string) string {
	return B64(ed25519.Sign(priv, ResultMessage(reportText)))
}

func VerifyResult(playerID, sig, reportText string) bool {
	return verify(playerID, sig, ResultMessage(reportText))
}

func verify(playerID, sig string, msg []byte) bool {
	pub := KeyOf(playerID)
	raw, ok := UnB64(sig)
	if pub == nil || !ok || len(raw) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, raw)
}

// LoadOrCreateKey 读私钥文件，没有就生成一把写进去。内容是一行 b64: 编码的 32 字节种子，和 Rust SDK 相同。
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	text, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := GenerateKey()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(B64(key.Seed())+"\n"), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	seed, ok := UnB64(strings.TrimSpace(string(text)))
	if !ok || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("密钥文件 %s 格式不对", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// LoadKey 只读不建：文件不存在就报错（签根文书时用，免得误生成一把新根密钥）。
func LoadKey(path string) (ed25519.PrivateKey, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("读密钥 %s：%w", path, err)
	}
	return LoadOrCreateKey(path)
}
