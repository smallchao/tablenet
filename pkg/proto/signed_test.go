package proto

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func testCard(now time.Time) NodeCard {
	c := NodeCard{Seq: now.UnixMilli(), IssuedAt: now, ExpiresAt: now.Add(30 * time.Minute), Name: "华东私服",
		Region: "cn-east", Endpoints: []Endpoint{{URL: "wss://203.0.113.7:8443/tablenet/1", TLSSHA256: "ab"}},
		Protocols: []string{Protocol}, Status: CardOpen}
	c.Software.Name, c.Software.Version = "tablenet-node", "0.2.0"
	return c
}

func TestCardSignVerify(t *testing.T) {
	k := GenerateKey()
	now := time.Now().UTC().Truncate(time.Second)
	s, err := SignCard(k, testCard(now))
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyCard(s, now)
	if err != nil || c.NodeID != IDOfKey(k) || c.Name != "华东私服" {
		t.Fatal(c, err)
	}
	// 改一个字就不认。
	bad := s
	bad.Text = strings.Replace(s.Text, "华东私服", "官方服务器", 1)
	if _, err := VerifyCard(bad, now); err == nil {
		t.Fatal("改过的名片应当验不过")
	}
	// 别人的签名不认：把 nodeId 换成别的公钥。
	other := IDOfKey(GenerateKey())
	forged := Signed{Text: strings.Replace(s.Text, IDOfKey(k), other, 1), Sig: s.Sig}
	if _, err := VerifyCard(forged, now); err == nil {
		t.Fatal("冒充别人的名片应当验不过")
	}
	if _, err := VerifyCard(s, now.Add(31*time.Minute)); err == nil {
		t.Fatal("过期名片不认")
	}
	long := testCard(now)
	long.ExpiresAt = now.Add(2 * time.Hour)
	ls, _ := SignCard(k, long)
	if _, err := VerifyCard(ls, now); err == nil {
		t.Fatal("有效期超过 30 分钟不认")
	}
	region := testCard(now)
	region.Region = "mars"
	rs, _ := SignCard(k, region)
	if _, err := VerifyCard(rs, now); err == nil {
		t.Fatal("不认识的地区不认")
	}
}

func TestNodeProof(t *testing.T) {
	k := GenerateKey()
	ch := RandomBytes(24)
	p := SignNodeProof(k, ch, "ed25519:player")
	if !VerifyNodeProof(IDOfKey(k), p, ch, "ed25519:player") {
		t.Fatal("证明应当通过")
	}
	if VerifyNodeProof(IDOfKey(k), p, ch, "ed25519:other") || VerifyNodeProof(IDOfKey(GenerateKey()), p, ch, "ed25519:player") {
		t.Fatal("换了玩家或节点就不该通过")
	}
	// 不同用途的签名不能互相冒充：拿握手签名当节点证明。
	hello := SignHello(k, ch, "ed25519:player")
	if VerifyNodeProof(IDOfKey(k), hello, ch, "ed25519:player") {
		t.Fatal("握手签名不能当节点证明用")
	}
}

func TestDirectoryList(t *testing.T) {
	root, backup := GenerateKey(), GenerateKey()
	roots := []string{IDOfKey(root), IDOfKey(backup)}
	now := time.Now()
	dir := GenerateKey()
	l := DirectoryList{Seq: 1, IssuedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour),
		Directories: []DirectoryEntry{{ID: IDOfKey(dir), URL: "https://dir-cn.example.org", Region: "cn-east"}},
		Probes:      []ProbeEntry{{ID: "ed25519:probe", Zone: "intl"}}}
	for _, signer := range []ed25519.PrivateKey{root, backup} {
		s, _ := SignDirectoryList(signer, l)
		got, err := VerifyDirectoryList(s, roots, now)
		if err != nil || !got.HasDirectory(IDOfKey(dir)) {
			t.Fatal(err)
		}
		if z, ok := got.ProbeZoneOf(IDOfKey(dir)); !ok || z != "cn" {
			t.Fatal("名录兼任所在区的探测点", z)
		}
	}
	stranger := GenerateKey()
	s, _ := SignDirectoryList(stranger, l)
	if _, err := VerifyDirectoryList(s, roots, now); err == nil {
		t.Fatal("非根密钥签的清单不认")
	}
	s, _ = SignDirectoryList(root, l)
	if _, err := VerifyDirectoryList(s, roots, now.Add(31*24*time.Hour)); err == nil {
		t.Fatal("过期清单不认")
	}
}

func TestProbeZones(t *testing.T) {
	for r, z := range map[string]string{"cn-east": "cn", "cn-west": "cn", "hk": "hk", "tw": "intl", "eu": "intl"} {
		if ProbeZone(r) != z {
			t.Fatal(r)
		}
	}
}

// 固定向量：Rust SDK 的 node_proof_vector 必须一致。
func TestNodeProofVector(t *testing.T) {
	k := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	got := SignNodeProof(k, []byte("challenge-1"), "ed25519:player")
	const want = "b64:1pq+jUX3EO5xamQS3+BxaQJRw9pHiGHw7hrg55UNHxExmsqfrY8aInqdvzp+SxBCsV4wNWU4JvXCnKxclbHSDQ=="
	if got != want {
		t.Fatal(got)
	}
}
