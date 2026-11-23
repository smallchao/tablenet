// SPDX-License-Identifier: MIT

package xmage

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Probe 只读地检查清单里的每台 XMage 服务器：连得上吗、客户端版本对不对、要不要注册、
// 主房间里有哪些桌、其中多少张认得出赛制。不建桌、不入座，看完就退出。上线前在跑适配器的那台机器上跑一次。
func Probe(ctx context.Context, cfg Config, out io.Writer) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	drivers := map[string]*Driver{}
	defer func() {
		for _, d := range drivers {
			d.Close()
		}
	}()
	bad := 0
	for _, sc := range cfg.Servers {
		fmt.Fprintf(out, "== %s（%s）\n", sc.ID, sc.Address)
		drv := drivers[sc.Lib]
		if drv == nil {
			dc := cfg.Driver
			dc.XMageLib = sc.Lib
			d, err := StartDriver(dc)
			if err != nil {
				return err
			}
			drivers[sc.Lib], drv = d, d
		}
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		hello, err := drv.Call(cctx, "hello", nil)
		cancel()
		if err != nil {
			return fmt.Errorf("Java 驱动没应：%w", err)
		}
		fmt.Fprintf(out, "  本机 XMage 客户端版本：%v（来自 %s）\n", hello["clientVersion"], sc.Lib)

		host, port := splitHostPort(sc.Address)
		// 先不带密码试一次：看这台要不要注册（被拒的原文里会说）。
		anon := sc.ID + ".probe-anon"
		cctx, cancel = context.WithTimeout(ctx, 45*time.Second)
		_, aerr := drv.Call(cctx, "connect", map[string]any{"s": anon, "host": host, "port": port, "user": xmageUser("tablenet")})
		_, _ = drv.Call(cctx, "disconnect", map[string]any{"s": anon})
		cancel()
		switch {
		case aerr == nil:
			fmt.Fprintln(out, "  不注册也能登录：能做桥接（accounts 可写 optional）")
		case isAuthError(aerr):
			fmt.Fprintf(out, "  要注册账号：只能做交接（accounts 写 required，机器人要配注册过的账号）\n    服务器原话：%v\n", aerr)
		default:
			bad++
			fmt.Fprintf(out, "  连不上：%v\n", aerr)
			if strings.Contains(aerr.Error(), "protocol") || strings.Contains(strings.ToLower(aerr.Error()), "version") {
				fmt.Fprintln(out, "    多半是版本不对（换一份和服务器同版本的 XMage lib），或者这台机器到它的网络不通（代理、防火墙）")
			}
			continue
		}
		if sc.Password == "" && aerr != nil {
			fmt.Fprintln(out, "  没配机器人密码，桌子表看不了；给机器人注册账号、写进清单后再跑一次")
			continue
		}

		// 用机器人账号登录、取一次桌子表。
		lobby := sc.ID + ".probe"
		body := map[string]any{"s": lobby, "host": host, "port": port, "user": sc.User}
		if sc.Password != "" {
			body["password"] = sc.Password
		}
		cctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		conn, err := drv.Call(cctx, "connect", body)
		if err != nil {
			cancel()
			bad++
			fmt.Fprintf(out, "  机器人账号 %s 登录失败：%v\n", sc.User, err)
			continue
		}
		fmt.Fprintf(out, "  机器人 %s 已登录，服务器版本：%v\n", sc.User, conn["serverVersion"])
		res, err := drv.Call(cctx, "tables", map[string]any{"s": lobby})
		_, _ = drv.Call(cctx, "disconnect", map[string]any{"s": lobby})
		cancel()
		if err != nil {
			bad++
			fmt.Fprintf(out, "  取桌子表失败：%v\n", err)
			continue
		}
		var tables []Table
		_ = decodeAs(res, "tables", &tables)
		known, waiting := 0, 0
		kinds := map[string]int{}
		for _, t := range tables {
			if t.State == "WAITING" {
				waiting++
			}
			f, _ := tablenetFormat(t.GameType, t.DeckType)
			if f != "" && !t.Tournament && !t.Limited {
				known++
			}
			k := t.GameType + " / " + t.DeckType
			if f == "" {
				k += "（认不出）"
			}
			kinds[k]++
		}
		fmt.Fprintf(out, "  主房间 %d 张桌，等人的 %d 张，认得出赛制的 %d 张：\n", len(tables), waiting, known)
		keys := make([]string, 0, len(kinds))
		for k := range kinds {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return kinds[keys[i]] > kinds[keys[j]] })
		for i, k := range keys {
			if i == 15 {
				fmt.Fprintf(out, "    ……还有 %d 种\n", len(keys)-15)
				break
			}
			fmt.Fprintf(out, "    %3d  %s\n", kinds[k], k)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d 台没通过", bad)
	}
	return nil
}

func splitHostPort(addr string) (string, int) {
	i := strings.LastIndexByte(addr, ':')
	port := 0
	fmt.Sscanf(addr[i+1:], "%d", &port)
	return addr[:i], port
}
