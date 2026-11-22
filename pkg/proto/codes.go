package proto

// 错误码。含义见 spec/01-客户端协议.md#错误码 与 spec/02-适配器协议.md。
const (
	CodeUnsupportedProtocol = "unsupported_protocol"
	CodeBadSignature        = "bad_signature"
	CodeUnknownType         = "unknown_type"
	CodeBadRequest          = "bad_request"
	CodeRateLimited         = "rate_limited"
	CodeBanned              = "banned"
	CodeNoRoom              = "no_room"
	CodeRoomFull            = "room_full"
	CodeWrongPassword       = "wrong_password"
	CodeFormatMismatch      = "format_mismatch"
	CodeCodecMismatch       = "codec_mismatch"
	CodeVenueUnavailable    = "venue_unavailable"
	CodeVersionMismatch     = "version_mismatch"
	CodeSpectateDisabled    = "spectate_disabled"
	CodeNotOwner            = "not_owner"
	CodeMatchExpired        = "match_expired"
	CodeHostOffline         = "host_offline"
	CodeNotAuthenticated    = "not_authenticated"
	CodeLimitReached        = "limit_reached"
	CodeInternal            = "internal"

	CodePlatformConflict   = "platform_conflict"
	CodePlatformNotAllowed = "platform_not_allowed"
	CodeAccountRequired    = "account_required"
	CodeLinkFailed         = "link_failed"

	// 通道关闭原因
	CodePeerLeft   = "peer_left"
	CodeRoomClosed = "room_closed"
	CodeReplaced   = "replaced"
)
