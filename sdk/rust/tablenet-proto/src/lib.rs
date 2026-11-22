//! Tablenet 线上格式：信封、身份与签名、对局通道帧、错误码。
//!
//! 规范见仓库根目录的 `spec/01-客户端协议.md`。这个 crate 不做任何网络 IO，节点、客户端、适配器共用。

pub mod codes;
pub mod envelope;
pub mod frame;
pub mod ident;
pub mod room_code;
pub mod signed;
pub mod time;

pub use envelope::Envelope;

/// 协议标识。握手时双方必须有这一个才算讲同一种话。
pub const PROTOCOL: &str = "tablenet/1";

/// 信封版本。
pub const ENVELOPE_VERSION: u32 = 1;

/// WebSocket 默认路径。
pub const WS_PATH: &str = "/tablenet/1";

/// 单条消息上限（文本帧和二进制帧都算）。
pub const MAX_MESSAGE_BYTES: usize = 1024 * 1024;

/// 原生对局里「本节点自己」的场地名。
pub const VENUE_NATIVE: &str = "native";

/// 平台编号是否合规：小写字母、数字、连字符，3–32 位；`native` 和 `x-` 开头的保留。
pub fn valid_platform_id(id: &str) -> bool {
    let n = id.len();
    (3..=32).contains(&n)
        && id != VENUE_NATIVE
        && !id.starts_with("x-")
        && !id.starts_with('-')
        && !id.ends_with('-')
        && id.bytes().all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn platform_ids() {
        assert!(valid_platform_id("xmage"));
        assert!(valid_platform_id("hexproof"));
        assert!(valid_platform_id("end-step2"));
        assert!(!valid_platform_id("native"));
        assert!(!valid_platform_id("x-foo"));
        assert!(!valid_platform_id("ab"));
        assert!(!valid_platform_id("XMage"));
        assert!(!valid_platform_id("-abc"));
    }
}
