package proto

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
)

func TestEnvelopeRoundtrip(t *testing.T) {
	e, err := Parse([]byte(`{"type":"room.list","id":"c1"}`))
	if err != nil || e.V != 1 || e.ID != "c1" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := Parse([]byte(`{"id":"x"}`)); err == nil {
		t.Fatal("缺 type 应当报错")
	}
	er := Error("c7", CodeNoRoom, "没有这个房间")
	back, _ := Parse(er.Bytes())
	if back.ErrorCode() != CodeNoRoom || back.Re != "c7" {
		t.Fatalf("%s", er.Bytes())
	}
	if !strings.Contains(string(er.Bytes()), `"retryAfterMs":null`) {
		t.Fatalf("retryAfterMs 应当是 null：%s", er.Bytes())
	}
}

func TestIdentity(t *testing.T) {
	k := GenerateKey()
	id := IDOfKey(k)
	if !strings.HasPrefix(id, "ed25519:") || len(id) != len("ed25519:")+52 {
		t.Fatal(id)
	}
	if !bytes.Equal(KeyOf(id), k.Public().(ed25519.PublicKey)) {
		t.Fatal("编号解不回公钥")
	}
	if KeyOf(strings.ToUpper(id)) != nil || KeyOf("ed25519:abc") != nil {
		t.Fatal("坏编号应当解不出")
	}
	nonce := RandomBytes(24)
	sig := SignHello(k, nonce, "ed25519:node")
	if !VerifyHello(id, sig, nonce, "ed25519:node") || VerifyHello(id, sig, nonce, "ed25519:other") {
		t.Fatal("握手签名")
	}
	rs := SignResult(k, `{"a":1}`)
	if !VerifyResult(id, rs, `{"a":1}`) || VerifyResult(id, rs, `{"a":2}`) {
		t.Fatal("战绩签名")
	}
}

// 固定向量：Go 和 Rust SDK（sdk/rust/tablenet-proto 的 fixed_vector 测试）必须算出同样的编号和签名。
const (
	vectorID  = "ed25519:5jfgyy7ctrjavpxvkb5rglwf7gkuo5vox27hxescd3vgsfcg2iwa"
	vectorSig = "b64:C/723kdmBJ37woPn1WpfL/QZbSmC0iJOxEmxbm92IRVwZ6faiNoAfbKBgeqFNRNItTXvXYSatiM3tncPJwYdAQ=="
)

func TestFixedVector(t *testing.T) {
	k := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	if got := IDOfKey(k); got != vectorID {
		t.Fatalf("编号 %s", got)
	}
	if got := SignHello(k, []byte("nonce-123"), "ed25519:node"); got != vectorSig {
		t.Fatalf("签名 %s", got)
	}
}

func TestFrameAndRoomCode(t *testing.T) {
	f := EncodeFrame(0x01020304, []byte("hi"))
	ch, p, ok := DecodeFrame(f)
	if !ok || ch != 0x01020304 || string(p) != "hi" {
		t.Fatal("帧往返")
	}
	if _, _, ok := DecodeFrame(EncodeFrame(0, nil)); ok {
		t.Fatal("0 号通道保留")
	}
	for i := 0; i < 200; i++ {
		if c := NewRoomCode(); !ValidRoomCode(c) {
			t.Fatal(c)
		}
	}
	if ValidRoomCode("K7Q2M0") || ValidRoomCode("k7q2md") {
		t.Fatal("坏房号")
	}
}

func TestPlatformID(t *testing.T) {
	for _, ok := range []string{"xmage", "hexproof", "end-step2"} {
		if !ValidPlatformID(ok) {
			t.Fatal(ok)
		}
	}
	for _, bad := range []string{"native", "x-foo", "ab", "XMage", "-abc"} {
		if ValidPlatformID(bad) {
			t.Fatal(bad)
		}
	}
}
