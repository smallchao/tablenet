//! 对局通道帧：WebSocket 二进制帧，开头 4 字节是通道号（大端），其后是负载。节点只看通道号，不看负载。

pub fn encode(channel: u32, payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(4 + payload.len());
    out.extend_from_slice(&channel.to_be_bytes());
    out.extend_from_slice(payload);
    out
}

pub fn decode(frame: &[u8]) -> Option<(u32, &[u8])> {
    if frame.len() < 4 {
        return None;
    }
    let ch = u32::from_be_bytes([frame[0], frame[1], frame[2], frame[3]]);
    if ch == 0 {
        return None;
    }
    Some((ch, &frame[4..]))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let f = encode(0x0102_0304, b"hi");
        assert_eq!(&f[..4], &[1, 2, 3, 4]);
        assert_eq!(decode(&f), Some((0x0102_0304, &b"hi"[..])));
        assert_eq!(decode(&[0, 0, 0]), None);
        assert_eq!(decode(&encode(0, b"x")), None, "0 号通道保留");
        assert_eq!(decode(&encode(7, b"")), Some((7, &b""[..])));
    }
}
