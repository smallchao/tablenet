//! 身份：Ed25519 公钥就是编号（`ed25519:` 加小写无填充 base32）。握手签名、战绩签名都在这里。

use std::fs;
use std::io;
use std::path::Path;

use data_encoding::{BASE32_NOPAD, BASE64};
use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};

pub use ed25519_dalek::SigningKey as SecretKey;

const PREFIX: &str = "ed25519:";
const HELLO_CONTEXT: &[u8] = b"tablenet/1 hello";
const RESULT_CONTEXT: &[u8] = b"tablenet/1 result";

pub fn random_bytes<const N: usize>() -> [u8; N] {
    let mut buf = [0u8; N];
    getrandom::getrandom(&mut buf).expect("系统随机数不可用");
    buf
}

pub fn generate_key() -> SigningKey {
    SigningKey::from_bytes(&random_bytes::<32>())
}

/// 公钥 → 编号。
pub fn id_of(key: &VerifyingKey) -> String {
    format!("{PREFIX}{}", BASE32_NOPAD.encode(key.as_bytes()).to_ascii_lowercase())
}

pub fn id_of_secret(key: &SigningKey) -> String {
    id_of(&key.verifying_key())
}

/// 编号 → 公钥。格式不对、不是曲线上的点都返回 None。
pub fn key_of(id: &str) -> Option<VerifyingKey> {
    let body = id.strip_prefix(PREFIX)?;
    if body.bytes().any(|b| b.is_ascii_uppercase()) {
        return None;
    }
    let raw = BASE32_NOPAD.decode(body.to_ascii_uppercase().as_bytes()).ok()?;
    let arr: [u8; 32] = raw.try_into().ok()?;
    VerifyingKey::from_bytes(&arr).ok()
}

pub fn b64(bytes: &[u8]) -> String {
    format!("b64:{}", BASE64.encode(bytes))
}

pub fn unb64(s: &str) -> Option<Vec<u8>> {
    BASE64.decode(s.strip_prefix("b64:")?.as_bytes()).ok()
}

/// 握手时签的内容：`"tablenet/1 hello" || nonce || nodeId`。
pub fn hello_message(nonce: &[u8], node_id: &str) -> Vec<u8> {
    let mut m = HELLO_CONTEXT.to_vec();
    m.extend_from_slice(nonce);
    m.extend_from_slice(node_id.as_bytes());
    m
}

pub fn sign_hello(key: &SigningKey, nonce: &[u8], node_id: &str) -> String {
    b64(&key.sign(&hello_message(nonce, node_id)).to_bytes())
}

pub fn verify_hello(player_id: &str, sig: &str, nonce: &[u8], node_id: &str) -> bool {
    verify(player_id, sig, &hello_message(nonce, node_id))
}

/// 战绩确认签的内容：`"tablenet/1 result" || 节点给出的战绩原文`。
pub fn result_message(report_text: &str) -> Vec<u8> {
    let mut m = RESULT_CONTEXT.to_vec();
    m.extend_from_slice(report_text.as_bytes());
    m
}

pub fn sign_result(key: &SigningKey, report_text: &str) -> String {
    b64(&key.sign(&result_message(report_text)).to_bytes())
}

pub fn verify_result(player_id: &str, sig: &str, report_text: &str) -> bool {
    verify(player_id, sig, &result_message(report_text))
}

fn verify(player_id: &str, sig: &str, msg: &[u8]) -> bool {
    let Some(key) = key_of(player_id) else { return false };
    let Some(raw) = unb64(sig) else { return false };
    let Ok(arr) = <[u8; 64]>::try_from(raw.as_slice()) else { return false };
    key.verify(msg, &Signature::from_bytes(&arr)).is_ok()
}

/// 读私钥文件，没有就生成一把写进去。文件内容是一行 `b64:` 编码的 32 字节种子。
pub fn load_or_create_key(path: &Path) -> io::Result<SigningKey> {
    match fs::read_to_string(path) {
        Ok(text) => {
            let raw = unb64(text.trim()).ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "密钥文件格式不对"))?;
            let seed: [u8; 32] =
                raw.try_into().map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "密钥文件长度不对"))?;
            Ok(SigningKey::from_bytes(&seed))
        }
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            let key = generate_key();
            if let Some(dir) = path.parent() {
                fs::create_dir_all(dir)?;
            }
            fs::write(path, format!("{}\n", b64(&key.to_bytes())))?;
            Ok(key)
        }
        Err(e) => Err(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn id_roundtrip() {
        let k = generate_key();
        let id = id_of_secret(&k);
        assert!(id.starts_with("ed25519:"));
        assert_eq!(id.len(), "ed25519:".len() + 52);
        assert_eq!(key_of(&id).unwrap(), k.verifying_key());
        assert!(key_of(&id.to_ascii_uppercase()).is_none());
        assert!(key_of("ed25519:abc").is_none());
        assert!(key_of("rsa:xyz").is_none());
    }

    #[test]
    fn hello_signature() {
        let k = generate_key();
        let id = id_of_secret(&k);
        let nonce = random_bytes::<24>();
        let sig = sign_hello(&k, &nonce, "ed25519:node");
        assert!(verify_hello(&id, &sig, &nonce, "ed25519:node"));
        assert!(!verify_hello(&id, &sig, &nonce, "ed25519:other"), "换了节点签名就不认");
        let other = id_of_secret(&generate_key());
        assert!(!verify_hello(&other, &sig, &nonce, "ed25519:node"));
        assert!(!verify_hello(&id, "b64:AAAA", &nonce, "ed25519:node"));
    }

    /// 固定向量：和 Go 节点（internal/proto 的 TestFixedVector）必须一致。
    #[test]
    fn fixed_vector() {
        let k = SigningKey::from_bytes(&[7u8; 32]);
        assert_eq!(id_of_secret(&k), "ed25519:5jfgyy7ctrjavpxvkb5rglwf7gkuo5vox27hxescd3vgsfcg2iwa");
        assert_eq!(
            sign_hello(&k, b"nonce-123", "ed25519:node"),
            "b64:C/723kdmBJ37woPn1WpfL/QZbSmC0iJOxEmxbm92IRVwZ6faiNoAfbKBgeqFNRNItTXvXYSatiM3tncPJwYdAQ=="
        );
    }

    #[test]
    fn result_signature() {
        let k = generate_key();
        let id = id_of_secret(&k);
        let sig = sign_result(&k, r#"{"a":1}"#);
        assert!(verify_result(&id, &sig, r#"{"a":1}"#));
        assert!(!verify_result(&id, &sig, r#"{"a":2}"#));
    }
}
