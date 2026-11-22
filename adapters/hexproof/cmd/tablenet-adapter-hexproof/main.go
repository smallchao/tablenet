// SPDX-License-Identifier: GPL-3.0-or-later

// tablenet-adapter-hexproof：把一个 Hexproof hub 接进 Tablenet 节点。
//
// 环境变量：
//
//	TABLENET_NODE_URL      节点地址（节点代管时自动给）
//	TABLENET_ADAPTER_KEY   适配器私钥文件，默认 hexproof-adapter.key
//	HEXPROOF_HUB_URL       适配器连 hub 的地址，默认 ws://127.0.0.1:57320/ws
//	HEXPROOF_PUBLIC_URL    交接时告诉玩家的 hub 地址，默认同上
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"tablenet/pkg/proto"

	adapter "tablenet-adapter-hexproof"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("adapter", adapter.PlatformID)
	key, err := proto.LoadOrCreateKey(env("TABLENET_ADAPTER_KEY", "hexproof-adapter.key"))
	if err != nil {
		log.Error("key", "err", err)
		os.Exit(1)
	}
	cfg := adapter.Config{
		NodeURL:      env("TABLENET_NODE_URL", "ws://127.0.0.1:8080/tablenet/1"),
		HubURL:       env("HEXPROOF_HUB_URL", "ws://127.0.0.1:57320/ws"),
		PublicServer: os.Getenv("HEXPROOF_PUBLIC_URL"),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := adapter.Run(ctx, cfg, key, log); err != nil && ctx.Err() == nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}
