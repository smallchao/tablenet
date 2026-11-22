// Package proto 是 Tablenet 的线上格式：信封、身份与签名、对局通道帧、房号、错误码。
//
// 规范见 spec/01-客户端协议.md。Rust 版在 sdk/rust/tablenet-proto，两边逐字节一致。
package proto

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	Protocol        = "tablenet/1"
	EnvelopeVersion = 1
	WSPath          = "/tablenet/1"
	MaxMessageBytes = 1 << 20
	VenueNative     = "native"
)

// Envelope 是公共信封。Body 保持原样，由各处理函数自己解。
type Envelope struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	Re   string          `json:"re,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
}

// New 组一条消息。body 为 nil 时不带 body。
func New(typ, id, re string, body any) Envelope {
	e := Envelope{V: EnvelopeVersion, Type: typ, ID: id, Re: re}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			panic(fmt.Sprintf("消息正文序列化失败：%v", err))
		}
		e.Body = raw
	}
	return e
}

// ErrorBody 是 error 消息的正文。
type ErrorBody struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	RetryAfterMs *int64 `json:"retryAfterMs"`
}

func Error(re, code, message string) Envelope {
	return New("error", "", re, ErrorBody{Code: code, Message: message})
}

func ErrorRetry(re, code, message string, retryAfterMs int64) Envelope {
	return New("error", "", re, ErrorBody{Code: code, Message: message, RetryAfterMs: &retryAfterMs})
}

func Parse(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return e, fmt.Errorf("不是合法的信封：%w", err)
	}
	if e.Type == "" {
		return e, fmt.Errorf("信封缺 type")
	}
	if e.V == 0 {
		e.V = EnvelopeVersion
	}
	return e, nil
}

func (e Envelope) Bytes() []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	return b
}

// Decode 把 body 解进 v；没有 body 时 v 保持零值。
func (e Envelope) Decode(v any) error {
	if len(e.Body) == 0 || string(e.Body) == "null" {
		return nil
	}
	return json.Unmarshal(e.Body, v)
}

// ErrorCode 是 error 消息的错误码，不是 error 时返回空串。
func (e Envelope) ErrorCode() string {
	if e.Type != "error" {
		return ""
	}
	var b ErrorBody
	_ = e.Decode(&b)
	return b.Code
}

// ValidPlatformID：小写字母、数字、连字符，3–32 位；native 和 x- 开头的保留。
func ValidPlatformID(id string) bool {
	if len(id) < 3 || len(id) > 32 || id == VenueNative || strings.HasPrefix(id, "x-") ||
		strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
