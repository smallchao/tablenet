// SPDX-License-Identifier: MIT

// tablenet-adapter-xmage：把别人运营的 XMage 服务器（一台或多台）接进 Tablenet 节点。
//
// 环境变量：
//
//	TABLENET_NODE_URL     节点地址（节点代管时自动给），默认 ws://127.0.0.1:8080/tablenet/1
//	TABLENET_ADAPTER_KEY  适配器私钥文件，默认 xmage-adapter.key（没有就生成）
//	XMAGE_SERVERS         要接入的 XMage 服务器清单（JSON 文件，格式见 servers.example.json；机器人密码也写在里面，别进仓库）
//	XMAGE_SERVER          只接一台时的简写：它的 host:port（XMAGE_SERVERS 没给时用）
//	XMAGE_PUBLIC_SERVER   同上，交接时告诉玩家的地址
//	XMAGE_USER / XMAGE_PASSWORD  同上，大厅机器人的账号
//	XMAGE_LIB             和目标服务器同一版本的 XMage lib 目录（XMage 客户端或服务器发布包里的 lib 都行），必填
//	XMAGE_DRIVER          Java 驱动的 jar，默认可执行文件旁边的 tablenet-xmage-driver.jar
//	XMAGE_DRIVER_DIR      驱动的工作目录（本地牌库建在这里），默认 xmage-driver
//	JAVA                  java 可执行文件，默认 PATH 里的 java（要 17 或更新）
//
// 子命令 probe：只读检查清单里的每台服务器，上线前跑一次（tablenet-adapter-xmage probe）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"tablenet/adapters/xmage"
	"tablenet/pkg/proto"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("adapter", xmage.PlatformID)
	key, err := proto.LoadOrCreateKey(env("TABLENET_ADAPTER_KEY", "xmage-adapter.key"))
	if err != nil {
		log.Error("key", "err", err)
		os.Exit(1)
	}
	jar := os.Getenv("XMAGE_DRIVER")
	if jar == "" {
		exe, _ := os.Executable()
		jar = filepath.Join(filepath.Dir(exe), "tablenet-xmage-driver.jar")
	}
	cfg := xmage.Config{
		NodeURL:      env("TABLENET_NODE_URL", "ws://127.0.0.1:8080/tablenet/1"),
		Server:       env("XMAGE_SERVER", "127.0.0.1:17171"),
		LobbyUser:    os.Getenv("XMAGE_USER"),
		PublicServer: os.Getenv("XMAGE_PUBLIC_SERVER"),
		Driver: xmage.DriverConfig{
			Java: os.Getenv("JAVA"), Jar: jar, XMageLib: os.Getenv("XMAGE_LIB"),
			WorkDir: env("XMAGE_DRIVER_DIR", "xmage-driver"), Stderr: os.Stderr,
		},
	}
	if p := os.Getenv("XMAGE_SERVERS"); p != "" {
		b, err := os.ReadFile(p)
		if err == nil {
			err = json.Unmarshal(b, &cfg.Servers)
		}
		if err != nil {
			log.Error("servers", "file", p, "err", err)
			os.Exit(1)
		}
	} else if u := os.Getenv("XMAGE_USER"); u != "" || os.Getenv("XMAGE_PASSWORD") != "" {
		cfg.Servers = []xmage.ServerConfig{{ID: "x", Address: cfg.Server, Public: cfg.PublicServer, User: u, Password: os.Getenv("XMAGE_PASSWORD")}}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// probe：只读地检查清单里的每台服务器（连通、版本、要不要注册、桌子表），不连节点、不建桌。
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		if err := xmage.Probe(ctx, cfg, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := xmage.Run(ctx, cfg, key, log); err != nil && ctx.Err() == nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}
