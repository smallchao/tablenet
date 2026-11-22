//! 签名文书（spec/09 节点自组网）：节点名片、节点身份证明、名录清单。和 Go 的 pkg/proto/signed.go 逐字节一致。
//!
//! 签的是 JSON 原文的字节，不做规范化：验签用收到的 `text` 原样算，解析只在验过之后。

use ed25519_dalek::{Signature, Signer, SigningKey, Verifier};
use serde::{Deserialize, Serialize};

use crate::ident::{b64, id_of_secret, key_of, unb64};

pub const CTX_NODE_CARD: &str = "tablenet/1 node-card";
pub const CTX_NODE_PROOF: &str = "tablenet/1 node-proof";
pub const CTX_DIRECTORIES: &str = "tablenet/1 directories";

pub const CARD_MAX_BYTES: usize = 4096;
pub const CARD_MAX_LIFETIME_SECS: i64 = 30 * 60;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Signed {
    pub text: String,
    pub sig: String,
}

fn verify_ctx(ctx: &str, signer_id: &str, text: &str, sig: &str) -> bool {
    let Some(key) = key_of(signer_id) else { return false };
    let Some(raw) = unb64(sig) else { return false };
    let Ok(arr) = <[u8; 64]>::try_from(raw.as_slice()) else { return false };
    let mut msg = ctx.as_bytes().to_vec();
    msg.extend_from_slice(text.as_bytes());
    key.verify(&msg, &Signature::from_bytes(&arr)).is_ok()
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Endpoint {
    pub url: String,
    #[serde(rename = "tlsSha256", default, skip_serializing_if = "Option::is_none")]
    pub tls_sha256: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize, Default)]
pub struct NodeLoad {
    #[serde(default)]
    pub players: u64,
    #[serde(default)]
    pub rooms: u64,
    #[serde(default)]
    pub channels: u64,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct VenueInfo {
    #[serde(rename = "platformId")]
    pub platform_id: String,
    #[serde(default)]
    pub bridge: bool,
}

/// 节点名片里客户端用得到的字段（多出来的字段忽略）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct NodeCard {
    pub v: u32,
    #[serde(rename = "nodeId")]
    pub node_id: String,
    pub seq: i64,
    #[serde(rename = "issuedAt")]
    pub issued_at: String,
    #[serde(rename = "expiresAt")]
    pub expires_at: String,
    pub name: String,
    pub region: String,
    pub endpoints: Vec<Endpoint>,
    pub protocols: Vec<String>,
    #[serde(default)]
    pub venues: Vec<VenueInfo>,
    #[serde(default)]
    pub load: NodeLoad,
    pub status: String,
}

/// 验名片：签名是 nodeId 那把密钥签的、版本、有效期。`now` 是 Unix 秒。
pub fn verify_card(s: &Signed, now: i64) -> Result<NodeCard, String> {
    if s.text.len() > CARD_MAX_BYTES {
        return Err("名片太大".into());
    }
    let c: NodeCard = serde_json::from_str(&s.text).map_err(|e| format!("名片不是合法 JSON：{e}"))?;
    if !verify_ctx(CTX_NODE_CARD, &c.node_id, &s.text, &s.sig) {
        return Err("名片签名不对".into());
    }
    if c.v != 1 {
        return Err(format!("名片版本 {} 不认", c.v));
    }
    let issued = crate::time::parse_rfc3339(&c.issued_at).ok_or("issuedAt 格式不对")?;
    let expires = crate::time::parse_rfc3339(&c.expires_at).ok_or("expiresAt 格式不对")?;
    if expires <= issued || expires - issued > CARD_MAX_LIFETIME_SECS {
        return Err("名片有效期不对".into());
    }
    if now >= expires {
        return Err("名片已过期".into());
    }
    if c.endpoints.is_empty() {
        return Err("名片没有地址".into());
    }
    Ok(c)
}

/// 节点证明身份要签的内容：`"tablenet/1 node-proof" || challenge || playerId || nodeId`。
pub fn node_proof_message(challenge: &[u8], player_id: &str, node_id: &str) -> Vec<u8> {
    let mut m = CTX_NODE_PROOF.as_bytes().to_vec();
    m.extend_from_slice(challenge);
    m.extend_from_slice(player_id.as_bytes());
    m.extend_from_slice(node_id.as_bytes());
    m
}

pub fn sign_node_proof(key: &SigningKey, challenge: &[u8], player_id: &str) -> String {
    b64(&key.sign(&node_proof_message(challenge, player_id, &id_of_secret(key))).to_bytes())
}

/// 客户端握手后验 `welcome.nodeProof`：对面确实持有名片上那把密钥。
pub fn verify_node_proof(node_id: &str, proof: &str, challenge: &[u8], player_id: &str) -> bool {
    let Some(key) = key_of(node_id) else { return false };
    let Some(raw) = unb64(proof) else { return false };
    let Ok(arr) = <[u8; 64]>::try_from(raw.as_slice()) else { return false };
    key.verify(&node_proof_message(challenge, player_id, node_id), &Signature::from_bytes(&arr)).is_ok()
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DirectoryEntry {
    pub id: String,
    pub url: String,
    pub region: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct DirectoryList {
    pub v: u32,
    pub seq: i64,
    #[serde(rename = "issuedAt")]
    pub issued_at: String,
    #[serde(rename = "expiresAt")]
    pub expires_at: String,
    pub directories: Vec<DirectoryEntry>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SignedList {
    pub text: String,
    pub sig: String,
    pub signer: String,
}

/// 验名录清单：签名者是根公钥之一、签名对、没过期。
pub fn verify_directory_list(s: &SignedList, roots: &[&str], now: i64) -> Result<DirectoryList, String> {
    if !roots.contains(&s.signer.as_str()) {
        return Err("名录清单的签名者不是根密钥".into());
    }
    if !verify_ctx(CTX_DIRECTORIES, &s.signer, &s.text, &s.sig) {
        return Err("名录清单签名不对".into());
    }
    let l: DirectoryList = serde_json::from_str(&s.text).map_err(|e| format!("名录清单不是合法 JSON：{e}"))?;
    let expires = crate::time::parse_rfc3339(&l.expires_at).ok_or("expiresAt 格式不对")?;
    if now >= expires {
        return Err("名录清单已过期".into());
    }
    if l.directories.is_empty() {
        return Err("名录清单是空的".into());
    }
    Ok(l)
}

/// 地区归到哪个探测区：cn／hk／intl。
pub fn probe_zone(region: &str) -> &'static str {
    if region.starts_with("cn-") {
        "cn"
    } else if region == "hk" {
        "hk"
    } else {
        "intl"
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Go 端（pkg/proto）用种子全 7 的密钥签出来的名片，原样拿来验。
    const CARD_TEXT: &str = r#"{"v":1,"nodeId":"ed25519:5jfgyy7ctrjavpxvkb5rglwf7gkuo5vox27hxescd3vgsfcg2iwa","seq":1795334400000,"issuedAt":"2026-11-22T08:00:00Z","expiresAt":"2026-11-22T08:30:00Z","name":"华东私服","region":"cn-east","endpoints":[{"url":"wss://203.0.113.7:8443/tablenet/1","tlsSha256":"ab"}],"protocols":["tablenet/1"],"capacity":{"players":0,"rooms":0,"relayMbps":0},"load":{"players":0,"rooms":0,"channels":0},"status":"open","software":{"name":"tablenet-node","version":"0.2.0"}}"#;
    const CARD_SIG: &str = "b64:cX3rbn7tip0BiXwPFWxYe59fjJsu4xumabOCsJKjRlrb3ftHl89pniFJ94JkihhPw50/i7iO+hmJFcB538/YDQ==";
    const NOW: i64 = 1_795_334_400 + 600; // 2026-11-22T08:10:00Z

    #[test]
    fn go_card_verifies() {
        let s = Signed { text: CARD_TEXT.into(), sig: CARD_SIG.into() };
        let c = verify_card(&s, NOW).unwrap();
        assert_eq!(c.name, "华东私服");
        assert_eq!(c.endpoints[0].tls_sha256.as_deref(), Some("ab"));
        assert_eq!(probe_zone(&c.region), "cn");
        assert!(verify_card(&s, NOW + 3600).is_err(), "过期不认");
        let tampered = Signed { text: CARD_TEXT.replace("华东私服", "官方服务器"), sig: CARD_SIG.into() };
        assert!(verify_card(&tampered, NOW).is_err(), "改过的不认");
    }

    #[test]
    fn node_proof_matches_go() {
        let k = SigningKey::from_bytes(&[7u8; 32]);
        let p = sign_node_proof(&k, b"challenge-1", "ed25519:player");
        assert_eq!(p, "b64:1pq+jUX3EO5xamQS3+BxaQJRw9pHiGHw7hrg55UNHxExmsqfrY8aInqdvzp+SxBCsV4wNWU4JvXCnKxclbHSDQ==");
        let id = id_of_secret(&k);
        assert!(verify_node_proof(&id, &p, b"challenge-1", "ed25519:player"));
        assert!(!verify_node_proof(&id, &p, b"challenge-2", "ed25519:player"));
        assert!(!verify_node_proof(&id_of_secret(&crate::ident::generate_key()), &p, b"challenge-1", "ed25519:player"));
    }
}
