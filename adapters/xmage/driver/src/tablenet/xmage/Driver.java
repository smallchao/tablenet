// SPDX-License-Identifier: MIT

package tablenet.xmage;

import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.google.gson.JsonParser;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.OutputStreamWriter;
import java.io.PrintStream;
import java.io.Writer;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collection;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

import mage.cards.decks.DeckCardInfo;
import mage.cards.decks.DeckCardLists;
import mage.cards.repository.CardInfo;
import mage.cards.repository.CardRepository;
import mage.cards.repository.CardScanner;
import mage.cards.repository.RepositoryUtil;
import mage.constants.ManaType;
import mage.constants.MatchTimeLimit;
import mage.constants.MultiplayerAttackOption;
import mage.constants.PlayerAction;
import mage.constants.RangeOfInfluence;
import mage.constants.SkillLevel;
import mage.game.match.MatchOptions;
import mage.interfaces.MageClient;
import mage.interfaces.callback.ClientCallback;
import mage.players.PlayerType;
import mage.players.net.UserData;
import mage.remote.Connection;
import mage.remote.Session;
import mage.remote.SessionImpl;
import mage.utils.MageVersion;
import mage.view.GameTypeView;
import mage.view.SeatView;
import mage.view.TableView;

/**
 * XMage 驱动：替 Go 写的适配器主体连 XMage 服务器。
 *
 * <p>XMage 的客户端和服务器之间是 JBoss Remoting 加 Java 序列化，别的语言接不了，所以这一小段用 Java，
 * 直接用 XMage 自己的 {@code Mage.Common}（运行时从 XMAGE_LIB 指向的 XMage 服务器 lib 目录加载，本程序不带 XMage 的 jar）。
 *
 * <p>和 Go 那边用标准输入输出讲 JSON Lines：
 * <ul>
 *   <li>Go → 驱动：{@code {"id":1,"op":"connect","s":"lobby",...}}，每条有回话 {@code {"re":1,"ok":true,...}} 或 {@code {"re":1,"error":"..."}}；</li>
 *   <li>驱动 → Go：XMage 的回调 {@code {"ev":"cb","s":"会话","m":"GAME_SELECT","obj":"…","d":{...}}}，
 *       会话断了 {@code {"ev":"lost","s":"…"}}。</li>
 * </ul>
 * 一个进程里可以开多个会话（大厅机器人一个，每位桥接玩家各一个）。局面和提问在这里压平成普通 JSON（见 {@link Views}），
 * 翻成独弈 arcane.v0 的事在 Go 那边做。
 */
public final class Driver {

    static final Gson GSON = new GsonBuilder().serializeNulls().disableHtmlEscaping().create();
    private static final Object OUT_LOCK = new Object();
    private static Writer out;

    private final Map<String, Sess> sessions = new ConcurrentHashMap<>();
    private final ExecutorService pool = Executors.newCachedThreadPool(r -> {
        Thread t = new Thread(r, "driver-op");
        t.setDaemon(true);
        return t;
    });

    public static void main(String[] args) throws Exception {
        // XMage 的库会往标准输出打日志，那会搅乱 JSON Lines：真正的输出另存一份，System.out 改指标准错误。
        out = new OutputStreamWriter(new java.io.FileOutputStream(java.io.FileDescriptor.out), StandardCharsets.UTF_8);
        System.setOut(new PrintStream(new java.io.FileOutputStream(java.io.FileDescriptor.err), true, "UTF-8"));
        new Driver().loop();
    }

    static void emit(Map<String, Object> m) {
        String line = GSON.toJson(m);
        synchronized (OUT_LOCK) {
            try {
                out.write(line);
                out.write('\n');
                out.flush();
            } catch (Exception e) {
                System.exit(3); // Go 那头没了
            }
        }
    }

    private void loop() throws Exception {
        BufferedReader in = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
        String line;
        while ((line = in.readLine()) != null) {
            line = line.trim();
            if (line.isEmpty()) {
                continue;
            }
            JsonObject req;
            try {
                req = JsonParser.parseString(line).getAsJsonObject();
            } catch (Exception e) {
                continue;
            }
            final JsonObject r = req;
            // 每条请求单独一条线程：XMage 的远程调用会阻塞，别让一位玩家的慢请求挡住别人。
            pool.submit(() -> handle(r));
        }
        for (Sess s : sessions.values()) {
            s.stop();
        }
        System.exit(0);
    }

    private void handle(JsonObject req) {
        Object id = req.has("id") ? GSON.fromJson(req.get("id"), Object.class) : null;
        Map<String, Object> reply = new LinkedHashMap<>();
        reply.put("re", id);
        try {
            Map<String, Object> body = dispatch(req);
            reply.put("ok", true);
            if (body != null) {
                reply.putAll(body);
            }
        } catch (Throwable e) {
            reply.put("ok", false);
            String msg = e.getMessage();
            reply.put("error", msg == null ? e.getClass().getSimpleName() : msg);
        }
        if (id != null) {
            emit(reply);
        }
    }

    private Sess sess(JsonObject req) {
        String s = str(req, "s");
        Sess x = sessions.get(s);
        if (x == null) {
            throw new IllegalStateException("没有这个会话：" + s);
        }
        return x;
    }

    private Map<String, Object> dispatch(JsonObject req) throws Exception {
        String op = str(req, "op");
        Map<String, Object> res = new LinkedHashMap<>();
        switch (op) {
            case "hello": {
                res.put("clientVersion", new MageVersion(MageClient.class).toString(false));
                return res;
            }
            case "cards": {
                // 准备本地牌库（按名字补系列和收藏编号用）。第一次要从 XMage 的 jar 里建库，二三十秒。
                RepositoryUtil.bootstrapLocalDb();
                CardScanner.scan();
                res.put("cards", CardRepository.instance.getNames().size());
                return res;
            }
            case "connect": {
                String s = str(req, "s");
                Sess old = sessions.remove(s);
                if (old != null) {
                    old.stop();
                }
                Sess x = new Sess(s);
                Connection c = new Connection();
                c.setHost(str(req, "host"));
                c.setPort(req.get("port").getAsInt());
                c.setUsername(str(req, "user"));
                if (req.has("password") && !req.get("password").isJsonNull()) {
                    c.setPassword(str(req, "password"));
                }
                c.setProxyType(Connection.ProxyType.NONE);
                UserData ud = UserData.getDefaultUserDataView();
                if (req.has("autoPay")) {
                    ud.setManaPoolAutomatic(req.get("autoPay").getAsBoolean());
                }
                // 触发排序交给玩家（独弈有排序面板）；替代效应同理。
                ud.setAutoOrderTrigger(false);
                c.setUserData(ud);
                x.session = new SessionImpl(x);
                if (!x.session.connectStart(c)) {
                    // 服务器拒绝登录的原因（例如「Wrong username or password. You must register…」）是登录调用返回之后
                    // 才用 SHOW_USERMESSAGE 单独发来的：稍等一下把它带上，适配器才认得出「这台要注册」。
                    String why = x.session.getLastError();
                    for (int i = 0; i < 20 && x.userMessage == null; i++) {
                        Thread.sleep(100);
                    }
                    if (x.userMessage != null) {
                        why = x.userMessage;
                    }
                    throw new IllegalStateException("连不上 XMage：" + (why == null || why.isEmpty() ? "未知原因" : why));
                }
                sessions.put(s, x);
                x.roomId = x.session.getMainRoomId();
                res.put("roomId", String.valueOf(x.roomId));
                res.put("serverVersion", x.session.getVersionInfo());
                return res;
            }
            case "disconnect": {
                Sess x = sessions.remove(str(req, "s"));
                if (x != null) {
                    x.stop();
                }
                return res;
            }
            case "gametypes": {
                Sess x = sess(req);
                List<Map<String, Object>> types = new ArrayList<>();
                for (GameTypeView g : x.session.getGameTypes()) {
                    Map<String, Object> m = new LinkedHashMap<>();
                    m.put("name", g.getName());
                    m.put("minPlayers", g.getMinPlayers());
                    m.put("maxPlayers", g.getMaxPlayers());
                    types.add(m);
                }
                res.put("gameTypes", types);
                List<String> decks = new ArrayList<>();
                for (String d : x.session.getDeckTypes()) {
                    decks.add(d);
                }
                res.put("deckTypes", decks);
                return res;
            }
            case "tables": {
                Sess x = sess(req);
                List<Map<String, Object>> list = new ArrayList<>();
                Collection<TableView> tables = x.session.getTables(x.roomId);
                if (tables != null) {
                    for (TableView t : tables) {
                        list.add(table(t));
                    }
                }
                res.put("tables", list);
                return res;
            }
            case "create_table": {
                Sess x = sess(req);
                MatchOptions o = new MatchOptions(str(req, "name"), str(req, "gameType"), req.get("seats").getAsInt() > 2);
                int seats = req.get("seats").getAsInt();
                for (int i = 0; i < seats; i++) {
                    o.getPlayerTypes().add(PlayerType.HUMAN);
                }
                o.setDeckType(str(req, "deckType"));
                o.setLimited(false);
                o.setAttackOption(MultiplayerAttackOption.MULTIPLE);
                o.setRange(RangeOfInfluence.ALL);
                o.setWinsNeeded(req.has("winsNeeded") ? req.get("winsNeeded").getAsInt() : 1);
                o.setMatchTimeLimit(MatchTimeLimit.NONE);
                o.setSkillLevel(SkillLevel.CASUAL);
                o.setSpectatorsAllowed(req.has("spectators") && req.get("spectators").getAsBoolean());
                o.setRollbackTurnsAllowed(false);
                if (req.has("password") && !req.get("password").isJsonNull()) {
                    o.setPassword(str(req, "password"));
                }
                TableView t = x.session.createTable(x.roomId, o);
                if (t == null) {
                    throw new IllegalStateException("XMage 不让开桌：" + lastError(x));
                }
                res.put("table", table(t));
                return res;
            }
            case "table": {
                Sess x = sess(req);
                TableView t = x.session.getTable(x.roomId, UUID.fromString(str(req, "table"))).orElse(null);
                res.put("table", t == null ? null : table(t));
                return res;
            }
            case "join_table": {
                Sess x = sess(req);
                DeckCardLists deck = deck(req.getAsJsonObject("deck"));
                String pw = req.has("password") && !req.get("password").isJsonNull() ? str(req, "password") : "";
                boolean ok = x.session.joinTable(x.roomId, UUID.fromString(str(req, "table")), str(req, "name"), PlayerType.HUMAN, 1, deck, pw);
                if (!ok) {
                    throw new IllegalStateException("XMage 拒绝入座：" + lastError(x));
                }
                return res;
            }
            case "start_match": {
                Sess x = sess(req);
                if (!x.session.startMatch(x.roomId, UUID.fromString(str(req, "table")))) {
                    throw new IllegalStateException("XMage 开不了这一局：" + lastError(x));
                }
                return res;
            }
            case "leave_table": {
                Sess x = sess(req);
                x.session.leaveTable(x.roomId, UUID.fromString(str(req, "table")));
                return res;
            }
            case "remove_table": {
                Sess x = sess(req);
                x.session.removeTable(x.roomId, UUID.fromString(str(req, "table")));
                return res;
            }
            case "join_game": {
                Sess x = sess(req);
                x.session.joinGame(UUID.fromString(str(req, "game")));
                return res;
            }
            case "quit_match": {
                Sess x = sess(req);
                x.session.quitMatch(UUID.fromString(str(req, "game")));
                return res;
            }
            case "submit_deck": {
                Sess x = sess(req);
                if (!x.session.submitDeck(UUID.fromString(str(req, "table")), deck(req.getAsJsonObject("deck")))) {
                    throw new IllegalStateException("XMage 不收这副牌：" + lastError(x));
                }
                return res;
            }
            case "uuid": {
                Sess x = sess(req);
                x.session.sendPlayerUUID(UUID.fromString(str(req, "game")), UUID.fromString(str(req, "v")));
                return res;
            }
            case "bool": {
                Sess x = sess(req);
                x.session.sendPlayerBoolean(UUID.fromString(str(req, "game")), req.get("v").getAsBoolean());
                return res;
            }
            case "int": {
                Sess x = sess(req);
                x.session.sendPlayerInteger(UUID.fromString(str(req, "game")), req.get("v").getAsInt());
                return res;
            }
            case "string": {
                Sess x = sess(req);
                x.session.sendPlayerString(UUID.fromString(str(req, "game")), str(req, "v"));
                return res;
            }
            case "mana": {
                Sess x = sess(req);
                x.session.sendPlayerManaType(UUID.fromString(str(req, "game")), UUID.fromString(str(req, "player")), ManaType.valueOf(str(req, "v")));
                return res;
            }
            case "action": {
                Sess x = sess(req);
                Object data = null;
                if (req.has("data") && !req.get("data").isJsonNull()) {
                    JsonElement d = req.get("data");
                    if (d.isJsonPrimitive() && d.getAsJsonPrimitive().isString()) {
                        String v = d.getAsString();
                        try {
                            data = UUID.fromString(v);
                        } catch (IllegalArgumentException e) {
                            data = v;
                        }
                    } else if (d.isJsonPrimitive() && d.getAsJsonPrimitive().isNumber()) {
                        data = d.getAsInt();
                    }
                }
                UUID game = req.has("game") && !req.get("game").isJsonNull() ? UUID.fromString(str(req, "game")) : null;
                x.session.sendPlayerAction(PlayerAction.valueOf(str(req, "v")), game, data);
                return res;
            }
            default:
                throw new IllegalArgumentException("不认识的操作：" + op);
        }
    }

    private static String lastError(Sess x) {
        String e = x.session.getLastError();
        if (x.lastMessage != null && !x.lastMessage.isEmpty()) {
            return x.lastMessage;
        }
        return e == null || e.isEmpty() ? "原因不明" : e;
    }

    static String str(JsonObject o, String k) {
        JsonElement e = o.get(k);
        return e == null || e.isJsonNull() ? null : e.getAsString();
    }

    /** 独弈交来的牌表（名字，可能带系列和收藏编号）→ XMage 的 DeckCardLists。认不出的牌直接报错，说清是哪几张。 */
    static DeckCardLists deck(JsonObject d) {
        DeckCardLists out = new DeckCardLists();
        out.setName(d.has("name") ? str(d, "name") : "Tablenet");
        List<String> missing = new ArrayList<>();
        out.setCards(cards(d.getAsJsonArray("main"), missing));
        out.setSideboard(d.has("side") ? cards(d.getAsJsonArray("side"), missing) : new ArrayList<>());
        if (!missing.isEmpty()) {
            throw new IllegalStateException("XMage 没有这几张牌：" + String.join("、", missing));
        }
        return out;
    }

    private static List<DeckCardInfo> cards(com.google.gson.JsonArray arr, List<String> missing) {
        List<DeckCardInfo> list = new ArrayList<>();
        if (arr == null) {
            return list;
        }
        for (JsonElement e : arr) {
            JsonObject c = e.getAsJsonObject();
            String name = str(c, "name");
            String set = c.has("set") ? str(c, "set") : null;
            String num = c.has("number") ? str(c, "number") : null;
            int count = c.has("count") ? c.get("count").getAsInt() : 1;
            CardInfo info = null;
            if (set != null && !set.isEmpty()) {
                info = CardRepository.instance.findCardWithPreferredSetAndNumber(name, set, num);
            }
            if (info == null) {
                info = CardRepository.instance.findPreferredCoreExpansionCard(name);
            }
            if (info == null) {
                info = CardRepository.instance.findCard(name);
            }
            if (info == null) {
                missing.add(name);
                continue;
            }
            list.add(new DeckCardInfo(info.getName(), info.getCardNumber(), info.getSetCode(), count));
        }
        return list;
    }

    static Map<String, Object> table(TableView t) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("id", String.valueOf(t.getTableId()));
        m.put("name", t.getTableName());
        m.put("controller", t.getControllerName());
        m.put("gameType", t.getGameType());
        m.put("deckType", t.getDeckType());
        m.put("state", t.getTableState() == null ? null : t.getTableState().name());
        m.put("passworded", t.isPassworded());
        m.put("spectators", t.getSpectatorsAllowed());
        m.put("tournament", t.isTournament());
        m.put("limited", t.isLimited());
        m.put("rated", t.isRated());
        m.put("info", t.getAdditionalInfoShort());
        m.put("games", t.getGames() == null ? new ArrayList<>() : t.getGames().stream().map(String::valueOf).toArray());
        List<Map<String, Object>> seats = new ArrayList<>();
        if (t.getSeats() != null) {
            for (SeatView s : t.getSeats()) {
                Map<String, Object> sm = new LinkedHashMap<>();
                sm.put("player", s.getPlayerId() == null ? null : String.valueOf(s.getPlayerId()));
                sm.put("name", s.getPlayerName());
                sm.put("type", s.getPlayerType() == null ? null : String.valueOf(s.getPlayerType()));
                seats.add(sm);
            }
        }
        m.put("seats", seats);
        return m;
    }

    /** 一个 XMage 会话：大厅机器人，或者一位桥接玩家。 */
    final class Sess implements MageClient {
        final String name;
        Session session;
        UUID roomId;
        volatile String lastMessage;
        volatile String userMessage; // 最近一条 SHOW_USERMESSAGE 的正文
        private final UUID clientId = UUID.randomUUID();

        Sess(String name) {
            this.name = name;
        }

        void stop() {
            try {
                if (session != null) {
                    session.connectStop(false, false);
                }
            } catch (Throwable ignored) {
            }
        }

        @Override
        public MageVersion getVersion() {
            return new MageVersion(MageClient.class);
        }

        @Override
        public void connected(String message) {
        }

        @Override
        public void disconnected(boolean askToReconnect, boolean keepMySessionActive) {
            if (sessions.get(name) == this) {
                Map<String, Object> m = new LinkedHashMap<>();
                m.put("ev", "lost");
                m.put("s", name);
                emit(m);
            }
        }

        @Override
        public void showMessage(String message) {
            lastMessage = message;
            event("message", null, Map.of("text", message == null ? "" : message));
        }

        @Override
        public void showError(String message) {
            lastMessage = message;
            event("error", null, Map.of("text", message == null ? "" : message));
        }

        @Override
        public void onNewConnection() {
        }

        @Override
        public void onCallback(ClientCallback cb) {
            try {
                cb.decompressData();
                if (cb.getMethod() == mage.interfaces.callback.ClientCallbackMethod.SHOW_USERMESSAGE && cb.getData() instanceof List) {
                    List<?> lines = (List<?>) cb.getData();
                    userMessage = lines.isEmpty() ? "" : String.valueOf(lines.get(lines.size() - 1));
                }
                Object data = Views.data(cb.getMethod(), cb.getData());
                event(cb.getMethod().name(), cb.getObjectId(), data);
            } catch (Throwable e) {
                e.printStackTrace();
                event("driver_error", cb.getObjectId(), Map.of("method", String.valueOf(cb.getMethod()), "text", String.valueOf(e)));
            }
        }

        private void event(String method, UUID obj, Object data) {
            Map<String, Object> m = new LinkedHashMap<>();
            m.put("ev", "cb");
            m.put("s", name);
            m.put("m", method);
            m.put("obj", obj == null ? null : obj.toString());
            m.put("d", data);
            emit(m);
        }
    }
}
