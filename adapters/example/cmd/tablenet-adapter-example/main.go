// tablenet-adapter-example：参考适配器的可执行文件。
//
// 环境变量：TABLENET_NODE_URL（节点代管时自动给），TABLENET_ADAPTER_KEY（私钥文件，默认 example-adapter.key）。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"tablenet/adapters/example"
	"tablenet/pkg/proto"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("adapter", example.PlatformID)
	url := os.Getenv("TABLENET_NODE_URL")
	if url == "" {
		url = "ws://127.0.0.1:8080/tablenet/1"
	}
	keyPath := os.Getenv("TABLENET_ADAPTER_KEY")
	if keyPath == "" {
		keyPath = "example-adapter.key"
	}
	key, err := proto.LoadOrCreateKey(keyPath)
	if err != nil {
		log.Error("key", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := example.Run(ctx, url, key, log); err != nil && ctx.Err() == nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}
