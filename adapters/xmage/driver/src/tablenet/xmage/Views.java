// SPDX-License-Identifier: MIT

package tablenet.xmage;

import java.io.Serializable;
import java.util.ArrayList;
import java.util.Collection;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;

import mage.choices.Choice;
import mage.interfaces.callback.ClientCallbackMethod;
import mage.players.PlayableObjectStats;
import mage.players.PlayableObjectsList;
import mage.view.AbilityPickerView;
import mage.view.CardView;
import mage.view.CardsView;
import mage.view.ChatMessage;
import mage.view.CombatGroupView;
import mage.view.CommandObjectView;
import mage.view.CounterView;
import mage.view.ExileView;
import mage.view.GameClientMessage;
import mage.view.GameEndView;
import mage.view.GameView;
import mage.view.LookedAtView;
import mage.view.ManaPoolView;
import mage.util.MultiAmountMessage;
import mage.view.PermanentView;
import mage.view.PlayerView;
import mage.view.RevealedView;
import mage.view.SimpleCardView;
import mage.view.StackAbilityView;
import mage.view.TableClientMessage;
import mage.view.UserRequestMessage;

/** 把 XMage 回调里的 Java 对象压平成普通 JSON（Map／List），只留适配器用得到的字段。 */
final class Views {

    private Views() {
    }

    static Object data(ClientCallbackMethod method, Object d) {
        if (d == null) {
            return null;
        }
        if (d instanceof GameView) {
            return Map.of("view", game((GameView) d));
        }
        if (d instanceof GameClientMessage) {
            return message((GameClientMessage) d);
        }
        if (d instanceof AbilityPickerView) {
            AbilityPickerView a = (AbilityPickerView) d;
            Map<String, Object> m = new LinkedHashMap<>();
            m.put("message", a.getMessage());
            List<Map<String, Object>> choices = new ArrayList<>();
            for (Map.Entry<UUID, String> e : a.getChoices().entrySet()) {
                choices.add(Map.of("id", e.getKey().toString(), "text", e.getValue() == null ? "" : e.getValue()));
            }
            m.put("choices", choices);
            if (a.getGameView() != null) {
                m.put("view", game(a.getGameView()));
            }
            return m;
        }
        if (d instanceof TableClientMessage) {
            TableClientMessage t = (TableClientMessage) d;
            Map<String, Object> m = new LinkedHashMap<>();
            m.put("game", s(t.getGameId()));
            m.put("player", s(t.getPlayerId()));
            m.put("table", s(t.getCurrentTableId()));
            m.put("parentTable", s(t.getParentTableId()));
            m.put("room", s(t.getRoomId()));
            m.put("time", t.getTime());
            m.put("flag", t.getFlag());
            if (t.getDeck() != null) {
                m.put("hasDeck", true);
            }
            return m;
        }
        if (d instanceof GameEndView) {
            GameEndView g = (GameEndView) d;
            Map<String, Object> m = new LinkedHashMap<>();
            m.put("won", g.hasWon());
            m.put("wins", g.getWins());
            m.put("loses", g.getLoses());
            m.put("winsNeeded", g.getWinsNeeded());
            m.put("gameInfo", g.getGameInfo());
            m.put("matchInfo", g.getMatchInfo());
            m.put("additional", g.getAdditionalInfo());
            return m;
        }
        if (d instanceof UserRequestMessage) {
            UserRequestMessage u = (UserRequestMessage) d;
            return Map.of("title", n(u.getTitle()), "message", n(u.getMessage()));
        }
        if (d instanceof ChatMessage) {
            ChatMessage c = (ChatMessage) d;
            return Map.of("message", n(c.getMessage()));
        }
        if (d instanceof List) {
            List<String> lines = new ArrayList<>();
            for (Object o : (List<?>) d) {
                lines.add(String.valueOf(o));
            }
            return Map.of("lines", lines);
        }
        if (d instanceof String || d instanceof Number || d instanceof Boolean) {
            return Map.of("value", d);
        }
        return Map.of("class", d.getClass().getName());
    }

    static String s(UUID id) {
        return id == null ? null : id.toString();
    }

    static String n(String x) {
        return x == null ? "" : x;
    }

    static Map<String, Object> message(GameClientMessage g) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("message", g.getMessage());
        m.put("flag", g.isFlag());
        m.put("min", g.getMin());
        m.put("max", g.getMax());
        List<String> targets = new ArrayList<>();
        if (g.getTargets() != null) {
            for (UUID t : g.getTargets()) {
                targets.add(t.toString());
            }
        }
        m.put("targets", targets);
        m.put("options", options(g.getOptions()));
        m.put("cards1", cards(g.getCardsView1()));
        m.put("cards2", cards(g.getCardsView2()));
        if (g.getChoice() != null) {
            m.put("choice", choice(g.getChoice()));
        }
        if (g.getMessages() != null) {
            List<Map<String, Object>> ms = new ArrayList<>();
            for (MultiAmountMessage a : g.getMessages()) {
                Map<String, Object> x = new LinkedHashMap<>();
                x.put("message", a.message);
                x.put("min", a.min);
                x.put("max", a.max);
                x.put("default", a.defaultValue);
                ms.add(x);
            }
            m.put("multi", ms);
        }
        if (g.getGameView() != null) {
            m.put("view", game(g.getGameView()));
        }
        return m;
    }

    static Map<String, Object> choice(Choice c) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("message", c.getMessage());
        m.put("subMessage", c.getSubMessage());
        m.put("required", c.isRequired());
        m.put("search", c.isSearchEnabled());
        m.put("manaColor", c.isManaColorChoice());
        m.put("keyChoice", c.isKeyChoice());
        if (c.isKeyChoice()) {
            List<Map<String, Object>> items = new ArrayList<>();
            for (Map.Entry<String, String> e : c.getKeyChoices().entrySet()) {
                items.add(Map.of("key", e.getKey(), "text", n(e.getValue())));
            }
            m.put("items", items);
        } else {
            List<Map<String, Object>> items = new ArrayList<>();
            for (String x : c.getChoices()) {
                items.add(Map.of("key", x, "text", x));
            }
            m.put("items", items);
        }
        m.put("special", c.isSpecialEnabled() ? c.getSpecialText() : null);
        return m;
    }

    static Map<String, Object> options(Map<String, Serializable> o) {
        Map<String, Object> m = new LinkedHashMap<>();
        if (o == null) {
            return m;
        }
        for (Map.Entry<String, Serializable> e : o.entrySet()) {
            m.put(e.getKey(), plain(e.getValue()));
        }
        return m;
    }

    static Object plain(Object v) {
        if (v == null || v instanceof String || v instanceof Number || v instanceof Boolean) {
            return v;
        }
        if (v instanceof UUID) {
            return v.toString();
        }
        if (v instanceof Enum) {
            return ((Enum<?>) v).name();
        }
        if (v instanceof Collection) {
            List<Object> l = new ArrayList<>();
            for (Object x : (Collection<?>) v) {
                l.add(plain(x));
            }
            return l;
        }
        if (v instanceof Map) {
            Map<String, Object> m = new LinkedHashMap<>();
            for (Map.Entry<?, ?> e : ((Map<?, ?>) v).entrySet()) {
                m.put(String.valueOf(e.getKey()), plain(e.getValue()));
            }
            return m;
        }
        return String.valueOf(v);
    }

    static Map<String, Object> game(GameView g) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("turn", g.getTurn());
        m.put("phase", g.getPhase() == null ? null : g.getPhase().name());
        m.put("step", g.getStep() == null ? null : g.getStep().name());
        m.put("activePlayerId", s(g.getActivePlayerId()));
        m.put("activePlayerName", g.getActivePlayerName());
        m.put("priorityPlayerName", g.getPriorityPlayerName());
        m.put("special", g.getSpecial());
        List<Map<String, Object>> players = new ArrayList<>();
        for (PlayerView p : g.getPlayers()) {
            players.add(player(p));
        }
        m.put("players", players);
        m.put("myPlayerId", g.getMyPlayer() == null ? null : s(g.getMyPlayer().getPlayerId()));
        m.put("hand", cards(g.getMyHand()));
        List<Map<String, Object>> stack = new ArrayList<>();
        if (g.getStack() != null) {
            for (CardView c : g.getStack().values()) {
                stack.add(stackItem(c));
            }
        }
        m.put("stack", stack);
        List<Map<String, Object>> exile = new ArrayList<>();
        if (g.getExile() != null) {
            for (ExileView e : g.getExile()) {
                Map<String, Object> x = new LinkedHashMap<>();
                x.put("id", s(e.getId()));
                x.put("name", e.getName());
                x.put("cards", cards(e));
                exile.add(x);
            }
        }
        m.put("exile", exile);
        List<Map<String, Object>> revealed = new ArrayList<>();
        if (g.getRevealed() != null) {
            for (RevealedView r : g.getRevealed()) {
                revealed.add(Map.of("name", n(r.getName()), "cards", cards(r.getCards())));
            }
        }
        m.put("revealed", revealed);
        List<Map<String, Object>> looked = new ArrayList<>();
        if (g.getLookedAt() != null) {
            for (LookedAtView r : g.getLookedAt()) {
                List<Map<String, Object>> ids = new ArrayList<>();
                for (SimpleCardView c : r.getCards().values()) {
                    ids.add(Map.of("id", s(c.getId()), "set", n(c.getExpansionSetCode()), "number", n(c.getCardNumber())));
                }
                looked.add(Map.of("name", n(r.getName()), "cards", ids));
            }
        }
        m.put("lookedAt", looked);
        List<Map<String, Object>> combat = new ArrayList<>();
        if (g.getCombat() != null) {
            for (CombatGroupView c : g.getCombat()) {
                Map<String, Object> x = new LinkedHashMap<>();
                x.put("defenderId", s(c.getDefenderId()));
                x.put("defenderName", c.getDefenderName());
                x.put("blocked", c.isBlocked());
                x.put("attackers", new ArrayList<>(ids(c.getAttackers())));
                x.put("blockers", new ArrayList<>(ids(c.getBlockers())));
                combat.add(x);
            }
        }
        m.put("combat", combat);
        Map<String, Object> playable = new LinkedHashMap<>();
        PlayableObjectsList pl = g.getCanPlayObjects();
        if (pl != null && pl.getObjects() != null) {
            for (Map.Entry<UUID, PlayableObjectStats> e : pl.getObjects().entrySet()) {
                Map<String, Object> x = new LinkedHashMap<>();
                List<String> abilityIds = new ArrayList<>();
                for (UUID a : e.getValue().getPlayableAbilityIds()) {
                    abilityIds.add(s(a));
                }
                x.put("abilityIds", abilityIds);
                x.put("names", e.getValue().getPlayableAbilityNames());
                playable.put(s(e.getKey()), x);
            }
        }
        m.put("playable", playable);
        return m;
    }

    static List<String> ids(CardsView cv) {
        List<String> l = new ArrayList<>();
        if (cv != null) {
            for (UUID id : cv.keySet()) {
                l.add(id.toString());
            }
        }
        return l;
    }

    static Map<String, Object> player(PlayerView p) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("id", s(p.getPlayerId()));
        m.put("name", p.getName());
        m.put("life", p.getLife());
        m.put("libraryCount", p.getLibraryCount());
        m.put("handCount", p.getHandCount());
        m.put("wins", p.getWins());
        m.put("winsNeeded", p.getWinsNeeded());
        m.put("active", p.isActive());
        m.put("hasPriority", p.hasPriority());
        m.put("hasLeft", p.hasLeft());
        m.put("monarch", p.isMonarch());
        m.put("initiative", p.isInitiative());
        m.put("counters", counters(p.getCounters()));
        ManaPoolView mp = p.getManaPool();
        Map<String, Object> pool = new LinkedHashMap<>();
        if (mp != null) {
            pool.put("W", mp.getWhite());
            pool.put("U", mp.getBlue());
            pool.put("B", mp.getBlack());
            pool.put("R", mp.getRed());
            pool.put("G", mp.getGreen());
            pool.put("C", mp.getColorless());
        }
        m.put("manaPool", pool);
        m.put("graveyard", cards(p.getGraveyard()));
        m.put("exile", cards(p.getExile()));
        List<Map<String, Object>> bf = new ArrayList<>();
        if (p.getBattlefield() != null) {
            for (PermanentView pv : p.getBattlefield().values()) {
                bf.add(card(pv));
            }
        }
        m.put("battlefield", bf);
        List<Map<String, Object>> command = new ArrayList<>();
        if (p.getCommandObjectList() != null) {
            for (CommandObjectView c : p.getCommandObjectList()) {
                Map<String, Object> x = new LinkedHashMap<>();
                x.put("id", s(c.getId()));
                x.put("name", c.getName());
                x.put("set", c.getExpansionSetCode());
                x.put("rules", c.getRules());
                x.put("kind", c.getClass().getSimpleName());
                command.add(x);
            }
        }
        m.put("command", command);
        m.put("designations", p.getDesignationNames());
        return m;
    }

    static List<Map<String, Object>> counters(List<CounterView> cs) {
        List<Map<String, Object>> l = new ArrayList<>();
        if (cs != null) {
            for (CounterView c : cs) {
                l.add(Map.of("name", n(c.getName()), "count", c.getCount()));
            }
        }
        return l;
    }

    static List<Map<String, Object>> cards(Map<UUID, ? extends CardView> cv) {
        List<Map<String, Object>> l = new ArrayList<>();
        if (cv != null) {
            for (CardView c : cv.values()) {
                l.add(card(c));
            }
        }
        return l;
    }

    static Map<String, Object> stackItem(CardView c) {
        Map<String, Object> m = card(c);
        if (c instanceof StackAbilityView) {
            StackAbilityView a = (StackAbilityView) c;
            CardView src = a.getSourceCard();
            if (src != null) {
                m.put("sourceId", s(src.getId()));
                m.put("sourceName", src.getDisplayName());
                m.put("sourceSet", src.getExpansionSetCode());
                m.put("sourceNumber", src.getCardNumber());
            }
            m.put("abilityType", a.getAbilityType() == null ? null : a.getAbilityType().name());
            m.put("stackAbility", true);
        }
        return m;
    }

    static Map<String, Object> card(CardView c) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("id", s(c.getId()));
        m.put("name", c.getDisplayName());
        m.put("fullName", c.getDisplayFullName());
        m.put("set", c.getExpansionSetCode());
        m.put("number", c.getCardNumber());
        m.put("cost", c.getManaCostStr());
        m.put("mv", c.getManaValue());
        m.put("types", c.getTypeText());
        m.put("rules", c.getRules());
        m.put("power", c.getPower());
        m.put("toughness", c.getToughness());
        m.put("loyalty", c.getLoyalty());
        m.put("defense", c.getDefense());
        m.put("faceDown", c.isFaceDown());
        m.put("token", c.isToken());
        m.put("ability", c.isAbility());
        m.put("transformed", c.isTransformed());
        m.put("creature", c.isCreature());
        m.put("land", c.isLand());
        m.put("planeswalker", c.isPlaneswalker());
        m.put("counters", counters(c.getCounters()));
        m.put("zone", c.getZone() == null ? null : c.getZone().name());
        List<String> targets = new ArrayList<>();
        if (c.getTargets() != null) {
            for (UUID t : c.getTargets()) {
                targets.add(t.toString());
            }
        }
        m.put("targets", targets);
        m.put("parentId", s(c.getParentId()));
        m.put("color", c.getColorText());
        if (c instanceof PermanentView) {
            PermanentView p = (PermanentView) c;
            m.put("tapped", p.isTapped());
            m.put("damage", p.getDamage());
            m.put("sick", p.hasSummoningSickness());
            m.put("attachedTo", s(p.getAttachedTo()));
            m.put("phasedIn", p.isPhasedIn());
            m.put("controllerName", p.getNameController());
            m.put("ownerName", p.getNameOwner());
            m.put("copy", p.isCopy());
            m.put("flipped", p.isFlipped());
        }
        if (c.getSecondCardFace() != null) {
            CardView b = c.getSecondCardFace();
            Map<String, Object> back = new LinkedHashMap<>();
            back.put("name", b.getDisplayName());
            back.put("types", b.getTypeText());
            back.put("rules", b.getRules());
            back.put("power", b.getPower());
            back.put("toughness", b.getToughness());
            m.put("back", back);
        }
        return m;
    }
}
