//! 6 位房号。字母表去掉了易混的 0/O/1/I/L，和独弈 0.3 中继一样。

use crate::ident::random_bytes;

pub const ALPHABET: &[u8] = b"23456789ABCDEFGHJKMNPQRSTUVWXYZ";
pub const LEN: usize = 6;

pub fn generate() -> String {
    // 31 个字符，取 248 以下的字节做均匀映射（248 = 31 × 8），多抽几次直到够数。
    let mut out = String::with_capacity(LEN);
    while out.len() < LEN {
        for b in random_bytes::<16>() {
            if b < 248 && out.len() < LEN {
                out.push(ALPHABET[(b % 31) as usize] as char);
            }
        }
    }
    out
}

pub fn is_valid(code: &str) -> bool {
    code.len() == LEN && code.bytes().all(|b| ALPHABET.contains(&b))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codes_are_valid() {
        for _ in 0..200 {
            let c = generate();
            assert!(is_valid(&c), "{c}");
        }
        assert!(!is_valid("K7Q2M0"));
        assert!(!is_valid("k7q2md"));
        assert!(!is_valid("K7Q2M"));
    }
}
