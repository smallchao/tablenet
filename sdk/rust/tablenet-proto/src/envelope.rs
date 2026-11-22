//! 公共信封：`{ "v": 1, "type": "...", "id": "...", "re": "...", "body": {...} }`。

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::ENVELOPE_VERSION;

fn default_v() -> u32 {
    ENVELOPE_VERSION
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Envelope {
    #[serde(default = "default_v")]
    pub v: u32,
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub re: Option<String>,
    #[serde(default)]
    pub body: Value,
}

impl Envelope {
    /// 不需要回话的通知。
    pub fn notice(kind: &str, body: Value) -> Self {
        Envelope { v: ENVELOPE_VERSION, kind: kind.to_string(), id: None, re: None, body }
    }

    /// 带请求编号的请求。
    pub fn request(kind: &str, id: impl Into<String>, body: Value) -> Self {
        Envelope { v: ENVELOPE_VERSION, kind: kind.to_string(), id: Some(id.into()), re: None, body }
    }

    /// 回某条请求的话。
    pub fn reply(re: Option<&str>, kind: &str, body: Value) -> Self {
        Envelope { v: ENVELOPE_VERSION, kind: kind.to_string(), id: None, re: re.map(str::to_string), body }
    }

    /// 出错一律用这个。`message` 只给人看，判断看 `code`。
    pub fn error(re: Option<&str>, code: &str, message: &str) -> Self {
        Self::reply(re, "error", json!({ "code": code, "message": message, "retryAfterMs": null }))
    }

    pub fn error_retry(re: Option<&str>, code: &str, message: &str, retry_after_ms: u64) -> Self {
        Self::reply(re, "error", json!({ "code": code, "message": message, "retryAfterMs": retry_after_ms }))
    }

    pub fn parse(text: &str) -> Result<Self, String> {
        let env: Envelope = serde_json::from_str(text).map_err(|e| format!("不是合法的信封：{e}"))?;
        if env.kind.is_empty() {
            return Err("信封缺 type".into());
        }
        Ok(env)
    }

    pub fn to_text(&self) -> String {
        serde_json::to_string(self).expect("信封总能序列化")
    }

    /// 是不是 `error`，是的话给出错误码。
    pub fn error_code(&self) -> Option<&str> {
        if self.kind == "error" {
            self.body.get("code").and_then(Value::as_str)
        } else {
            None
        }
    }

    pub fn str_field(&self, key: &str) -> Option<&str> {
        self.body.get(key).and_then(Value::as_str)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip_and_defaults() {
        let e = Envelope::parse(r#"{"type":"room.list","id":"c1"}"#).unwrap();
        assert_eq!(e.v, 1);
        assert_eq!(e.id.as_deref(), Some("c1"));
        assert_eq!(e.body, Value::Null);
        let back = Envelope::parse(&e.to_text()).unwrap();
        assert_eq!(back, e);
    }

    #[test]
    fn error_shape() {
        let e = Envelope::error(Some("c7"), "no_room", "没有这个房间");
        assert_eq!(e.kind, "error");
        assert_eq!(e.re.as_deref(), Some("c7"));
        assert_eq!(e.error_code(), Some("no_room"));
        assert!(Envelope::parse(r#"{"id":"x"}"#).is_err());
    }
}
