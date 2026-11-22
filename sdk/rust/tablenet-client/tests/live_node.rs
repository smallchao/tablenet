//! 连真 Go 节点的实测：设了 TABLENET_LIVE_URL 和 TABLENET_LIVE_NODE（节点编号）才跑。
//!     TABLENET_LIVE_URL=ws://127.0.0.1:18080/tablenet/1 TABLENET_LIVE_NODE=ed25519:… cargo test -p tablenet-client --test live_node

use serde_json::json;
use tablenet_client::Client;
use tablenet_proto::ident::generate_key;

fn live() -> Option<(String, String)> {
    Some((std::env::var("TABLENET_LIVE_URL").ok()?, std::env::var("TABLENET_LIVE_NODE").ok()?))
}

#[tokio::test]
async fn rust_client_verifies_go_node_identity() {
    let Some((url, node)) = live() else { return };
    let hello = || json!({ "role": "player", "displayName": "Rust 客户端", "venues": ["native"], "codecs": ["arcane.v0"] });
    let c = Client::connect_node(&url, &generate_key(), hello(), &node).await.expect("真节点应当证明得了自己");
    assert!(c.welcome.get("nodeProof").and_then(|v| v.as_str()).is_some());
    c.close().await;
    let fake = tablenet_proto::ident::id_of_secret(&generate_key());
    let err = Client::connect_node(&url, &generate_key(), hello(), &fake).await.err().expect("编号对不上要拒");
    assert!(err.contains("不是要连的节点"), "{err}");
}
