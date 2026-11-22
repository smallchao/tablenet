//! 最小的 Tablenet 客户端：连节点、握手、发请求等回话、收发对局通道帧。
//!
//! 适配器和测试用它；独弈桌面端有自己的实现（同步 TCP 那一套），不依赖这个 crate。

use std::collections::VecDeque;
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde_json::{json, Value};
use tablenet_proto::{frame, ident, Envelope, PROTOCOL};
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::{connect_async, MaybeTlsStream, WebSocketStream};

pub use tablenet_proto::ident::SecretKey;

type Ws = WebSocketStream<MaybeTlsStream<TcpStream>>;

/// 收到的一条东西：一条消息，或者一帧对局数据。
#[derive(Debug, Clone, PartialEq)]
pub enum Incoming {
    Msg(Envelope),
    Frame(u32, Vec<u8>),
}

pub struct Client {
    ws: Ws,
    next_id: u64,
    /// 等某条回话时顺手收到的别的东西，按到达顺序留着给 `recv`。
    backlog: VecDeque<Incoming>,
    pub node_hello: Value,
    pub welcome: Value,
    pub player_id: String,
}

impl Client {
    /// 连上节点并完成握手。`hello` 里放 role、displayName、venues、codecs、platform 这些，
    /// protocol／playerId／sig 由这里补上。
    pub async fn connect(url: &str, key: &SecretKey, mut hello: Value) -> Result<Client, String> {
        let (ws, _) = connect_async(url).await.map_err(|e| format!("连不上 {url}：{e}"))?;
        let mut c = Client {
            ws,
            next_id: 1,
            backlog: VecDeque::new(),
            node_hello: Value::Null,
            welcome: Value::Null,
            player_id: ident::id_of_secret(key),
        };
        let first = c.recv_raw(Duration::from_secs(10)).await?;
        let Incoming::Msg(nh) = first else { return Err("节点先发来的不是 node.hello".into()) };
        if nh.kind != "node.hello" {
            return Err(format!("节点先发来的是 {}，不是 node.hello", nh.kind));
        }
        let nonce = nh.str_field("nonce").and_then(ident::unb64).ok_or("node.hello 缺 nonce")?;
        let node_id = nh.str_field("nodeId").ok_or("node.hello 缺 nodeId")?.to_string();
        if !nh.body.get("protocols").and_then(Value::as_array).is_some_and(|a| a.iter().any(|p| p == PROTOCOL)) {
            return Err("节点不讲 tablenet/1".into());
        }
        let obj = hello.as_object_mut().ok_or("hello 必须是对象")?;
        obj.insert("protocol".into(), json!(PROTOCOL));
        obj.insert("playerId".into(), json!(c.player_id));
        obj.insert("sig".into(), json!(ident::sign_hello(key, &nonce, &node_id)));
        c.node_hello = nh.body;
        let reply = c.request("hello", hello).await?;
        if let Some(code) = reply.error_code() {
            return Err(format!("握手被拒：{code} {}", reply.body["message"]));
        }
        if reply.kind != "welcome" {
            return Err(format!("握手回的是 {}", reply.kind));
        }
        c.welcome = reply.body;
        Ok(c)
    }

    fn take_id(&mut self) -> String {
        let id = format!("c{}", self.next_id);
        self.next_id += 1;
        id
    }

    /// 发一条请求，返回它的编号（回话以后用 `recv` 自己收）。
    pub async fn send(&mut self, kind: &str, body: Value) -> Result<String, String> {
        let id = self.take_id();
        self.send_env(&Envelope::request(kind, id.clone(), body)).await?;
        Ok(id)
    }

    pub async fn notify(&mut self, kind: &str, body: Value) -> Result<(), String> {
        self.send_env(&Envelope::notice(kind, body)).await
    }

    pub async fn reply(&mut self, re: Option<&str>, kind: &str, body: Value) -> Result<(), String> {
        self.send_env(&Envelope::reply(re, kind, body)).await
    }

    pub async fn send_env(&mut self, env: &Envelope) -> Result<(), String> {
        self.ws.send(Message::text(env.to_text())).await.map_err(|e| format!("发送失败：{e}"))
    }

    pub async fn send_frame(&mut self, channel: u32, payload: &[u8]) -> Result<(), String> {
        self.ws.send(Message::binary(frame::encode(channel, payload))).await.map_err(|e| format!("发送失败：{e}"))
    }

    /// 发请求并等它的回话（`re` 对上的那条，`error` 也算）。中途收到的别的东西留给 `recv`。
    pub async fn request(&mut self, kind: &str, body: Value) -> Result<Envelope, String> {
        let id = self.send(kind, body).await?;
        self.wait_reply(&id, Duration::from_secs(15)).await
    }

    pub async fn wait_reply(&mut self, id: &str, timeout: Duration) -> Result<Envelope, String> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let left = deadline.saturating_duration_since(tokio::time::Instant::now());
            if left.is_zero() {
                return Err(format!("等 {id} 的回话超时"));
            }
            match self.recv_raw(left).await? {
                Incoming::Msg(env) if env.re.as_deref() == Some(id) => return Ok(env),
                other => self.backlog.push_back(other),
            }
        }
    }

    /// 收下一条（先吐积压的）。节点的 `ping` 在这里自动回 `pong`，不交给调用方。
    pub async fn recv(&mut self, timeout: Duration) -> Result<Incoming, String> {
        if let Some(x) = self.backlog.pop_front() {
            return Ok(x);
        }
        self.recv_raw(timeout).await
    }

    /// 一直收，直到某种消息出现；中间的东西丢回积压队列。
    pub async fn recv_kind(&mut self, kind: &str, timeout: Duration) -> Result<Envelope, String> {
        let deadline = tokio::time::Instant::now() + timeout;
        let mut skipped = VecDeque::new();
        let found = loop {
            if let Some(pos) = self.backlog.iter().position(|x| matches!(x, Incoming::Msg(e) if e.kind == kind)) {
                let Some(Incoming::Msg(e)) = self.backlog.remove(pos) else { unreachable!() };
                break Ok(e);
            }
            let left = deadline.saturating_duration_since(tokio::time::Instant::now());
            if left.is_zero() {
                break Err(format!("等 {kind} 超时"));
            }
            match self.recv_raw(left).await {
                Ok(Incoming::Msg(e)) if e.kind == kind => break Ok(e),
                Ok(other) => skipped.push_back(other),
                Err(e) => break Err(e),
            }
        };
        while let Some(x) = skipped.pop_back() {
            self.backlog.push_front(x);
        }
        found
    }

    async fn recv_raw(&mut self, timeout: Duration) -> Result<Incoming, String> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let left = deadline.saturating_duration_since(tokio::time::Instant::now());
            let msg = match tokio::time::timeout(left, self.ws.next()).await {
                Err(_) => return Err("接收超时".into()),
                Ok(None) => return Err("连接已关闭".into()),
                Ok(Some(Err(e))) => return Err(format!("接收失败：{e}")),
                Ok(Some(Ok(m))) => m,
            };
            match msg {
                Message::Text(t) => {
                    let env = Envelope::parse(t.as_str())?;
                    if env.kind == "ping" {
                        self.reply(env.id.as_deref(), "pong", Value::Null).await?;
                        continue;
                    }
                    return Ok(Incoming::Msg(env));
                }
                Message::Binary(b) => {
                    let (ch, payload) = frame::decode(&b).ok_or("收到坏的通道帧")?;
                    return Ok(Incoming::Frame(ch, payload.to_vec()));
                }
                Message::Close(_) => return Err("连接已关闭".into()),
                _ => continue,
            }
        }
    }

    pub async fn close(mut self) {
        let _ = self.ws.close(None).await;
    }
}
