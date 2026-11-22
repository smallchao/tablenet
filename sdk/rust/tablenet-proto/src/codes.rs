//! 错误码。含义见 `spec/01-客户端协议.md#错误码` 和 `spec/02-适配器协议.md`。

pub const UNSUPPORTED_PROTOCOL: &str = "unsupported_protocol";
pub const BAD_SIGNATURE: &str = "bad_signature";
pub const UNKNOWN_TYPE: &str = "unknown_type";
pub const BAD_REQUEST: &str = "bad_request";
pub const RATE_LIMITED: &str = "rate_limited";
pub const BANNED: &str = "banned";
pub const NO_ROOM: &str = "no_room";
pub const ROOM_FULL: &str = "room_full";
pub const WRONG_PASSWORD: &str = "wrong_password";
pub const FORMAT_MISMATCH: &str = "format_mismatch";
pub const CODEC_MISMATCH: &str = "codec_mismatch";
pub const VENUE_UNAVAILABLE: &str = "venue_unavailable";
pub const VERSION_MISMATCH: &str = "version_mismatch";
pub const SPECTATE_DISABLED: &str = "spectate_disabled";
pub const NOT_OWNER: &str = "not_owner";
pub const MATCH_EXPIRED: &str = "match_expired";
pub const HOST_OFFLINE: &str = "host_offline";
pub const NOT_AUTHENTICATED: &str = "not_authenticated";
pub const LIMIT_REACHED: &str = "limit_reached";
pub const INTERNAL: &str = "internal";

// 适配器
pub const PLATFORM_CONFLICT: &str = "platform_conflict";
pub const PLATFORM_NOT_ALLOWED: &str = "platform_not_allowed";
pub const ACCOUNT_REQUIRED: &str = "account_required";
pub const LINK_FAILED: &str = "link_failed";
pub const BAD_HANDOFF: &str = "bad_handoff";

// 通道关闭原因
pub const PEER_LEFT: &str = "peer_left";
pub const ROOM_CLOSED: &str = "room_closed";
