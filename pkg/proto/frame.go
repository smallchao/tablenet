package proto

import "encoding/binary"

// EncodeFrame：对局通道帧 = 4 字节通道号（大端）+ 负载。0 号通道保留。
func EncodeFrame(channel uint32, payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out, channel)
	copy(out[4:], payload)
	return out
}

func DecodeFrame(frame []byte) (uint32, []byte, bool) {
	if len(frame) < 4 {
		return 0, nil, false
	}
	ch := binary.BigEndian.Uint32(frame)
	if ch == 0 {
		return 0, nil, false
	}
	return ch, frame[4:], true
}

// 房号：6 位，字母表去掉易混的 0/O/1/I/L，和独弈 0.3 中继一样。
const RoomCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
const RoomCodeLen = 6

func NewRoomCode() string {
	out := make([]byte, 0, RoomCodeLen)
	for len(out) < RoomCodeLen {
		for _, b := range RandomBytes(16) {
			// 31 × 8 = 248：只用 248 以下的字节，保证均匀。
			if b < 248 && len(out) < RoomCodeLen {
				out = append(out, RoomCodeAlphabet[int(b)%31])
			}
		}
	}
	return string(out)
}

func ValidRoomCode(code string) bool {
	if len(code) != RoomCodeLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		if !containsByte(RoomCodeAlphabet, code[i]) {
			return false
		}
	}
	return true
}

func containsByte(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}
