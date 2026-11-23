// SPDX-License-Identifier: MIT

// tablenet-adapter-xmage：把一台 XMage 服务器接进 Tablenet 节点。
//
// 环境变量：
//
//	TABLENET_NODE_URL     节点地址（节点代管时自动给），默认 ws://127.0.0.1:8080/tablenet/1
//	TABLENET_ADAPTER_KEY  适配器私钥文件，默认 xmage-adapter.key（没有就生成）
//	XMAGE_SERVER          适配器连 XMage 的地址，默认 127.0.0.1:17171
//	XMAGE_PUBLIC_SERVER   交接时告诉玩家的地址，默认同上
//	XMAGE_LIB             XMage 服务器的 lib 目录（驱动要用里面的 mage-common 等 jar），必填
//	XMAGE_DRIVER          Java 驱动的 jar，默认可执行文件旁边的 tablenet-xmage-driver.jar
//	XMAGE_DRIVER_DIR      驱动的工作目录（本地牌库建在这里），默认 xmage-driver
//	JAVA                  java 可执行文件，默认 PATH 里的 java（要 17 或更新）
package main

import (
	"context"
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
		PublicServer: os.Getenv("XMAGE_PUBLIC_SERVER"),
		Driver: xmage.DriverConfig{
			Java: os.Getenv("JAVA"), Jar: jar, XMageLib: os.Getenv("XMAGE_LIB"),
			WorkDir: env("XMAGE_DRIVER_DIR", "xmage-driver"), Stderr: os.Stderr,
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := xmage.Run(ctx, cfg, key, log); err != nil && ctx.Err() == nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}
