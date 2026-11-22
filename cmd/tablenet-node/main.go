// tablenet-node：Tablenet 节点。纯后端，没有界面，用配置文件和命令行运维（见 spec/06-部署与运维.md）。
//
//	tablenet-node [run] [--config node.toml]
//	tablenet-node init  [--config node.toml]      写一份示例配置
//	tablenet-node admin <命令> [参数] [--config node.toml]
//	tablenet-node version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tablenet/internal/node"
	"tablenet/pkg/proto"
)

const usage = `用法：
  tablenet-node [run] [--config node.toml]   启动节点
  tablenet-node init [--config node.toml]    写一份示例配置（已存在就不覆盖）
  tablenet-node admin <命令> [--config node.toml]
      status                     在线人数、房间、排队、适配器
      rooms                      全部房间
      bans                       黑名单
      adapters                   在线的外部平台
      close <roomId>             强制关房
      ban <playerId> [原因]      拉黑
      unban <playerId>           解除拉黑
      adapter-key <平台> <公钥>  换适配器公钥
      directory                  名片和各名录的登记结果
      drain                      排空：不接新局、名录不再推荐，等现有对局打完再停机
      undrain                    取消排空
  tablenet-node version
`

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	rest := []string{}
	// 允许 --config 写在子命令参数后面。
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" || args[i] == "-config" {
			if i+1 < len(args) {
				*cfgPath = args[i+1]
				i++
			}
			continue
		}
		if v, ok := strings.CutPrefix(args[i], "--config="); ok {
			*cfgPath = v
			continue
		}
		rest = append(rest, args[i])
	}
	_ = fs.Parse(nil)

	var err error
	switch cmd {
	case "run":
		err = run(*cfgPath)
	case "init":
		err = writeExample(*cfgPath, rest)
	case "admin":
		err = admin(*cfgPath, rest)
	case "version":
		fmt.Println("tablenet-node", node.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "出错：", err)
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	if p := os.Getenv("TABLENET_CONFIG"); p != "" {
		return p
	}
	return "node.toml"
}

func run(path string) error {
	cfg, err := node.LoadConfig(path)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, err := node.NewServer(cfg, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}

// writeExample：不带参数写一份本机自测用的配置；带 --region 和 --public-host 写一份公网节点配置，
// 并生成自签证书（spec/09）。
func writeExample(path string, args []string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s 已存在，不覆盖", path)
	}
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	region := fs.String("region", "", "地区（并入自组网时要填）："+strings.Join(proto.Regions, " "))
	host := fs.String("public-host", "", "对外的 IP 或域名")
	_ = fs.Parse(args)
	if *region == "" {
		if err := os.WriteFile(path, []byte(node.ExampleConfig), 0o600); err != nil {
			return err
		}
		fmt.Println("已写入", path, "（本机自测用；要并入自组网，用 --region 和 --public-host 重新生成）")
		return nil
	}
	if !proto.ValidRegion(*region) || *host == "" {
		return fmt.Errorf("--region 要取 %s 之一，--public-host 要填对外的 IP 或域名", strings.Join(proto.Regions, " "))
	}
	dir := filepath.Dir(path)
	if _, _, err := node.GenerateSelfSigned(filepath.Join(dir, "data", "tls"), []string{*host}); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(node.PublicConfig(*region, *host)), 0o600); err != nil {
		return err
	}
	fp, _ := node.CertFingerprint(filepath.Join(dir, "data", "tls", "cert.pem"))
	fmt.Println("已写入", path, "，并生成自签证书（指纹", fp, "）")
	fmt.Println()
	fmt.Println(node.ComplianceNotice)
	return nil
}

func admin(path string, args []string) error {
	if len(args) == 0 {
		return errors.New("缺管理命令；tablenet-node help 看用法")
	}
	cfg, err := node.LoadConfig(path)
	if err != nil {
		return err
	}
	if cfg.Listen.Admin == "" {
		return errors.New("配置里没开管理接口（listen.admin）")
	}
	tok, err := node.AdminToken(cfg)
	if err != nil {
		return err
	}
	method, route, q := http.MethodGet, "", url.Values{}
	need := func(n int) error {
		if len(args) < n+1 {
			return fmt.Errorf("%s 需要 %d 个参数", args[0], n)
		}
		return nil
	}
	switch args[0] {
	case "status", "rooms", "bans", "adapters", "directory":
		route = "/" + args[0]
	case "drain", "undrain":
		method, route = http.MethodPost, "/"+args[0]
	case "close":
		if err := need(1); err != nil {
			return err
		}
		method, route = http.MethodPost, "/close"
		q.Set("room", args[1])
	case "ban":
		if err := need(1); err != nil {
			return err
		}
		method, route = http.MethodPost, "/ban"
		q.Set("player", args[1])
		q.Set("reason", strings.Join(args[2:], " "))
	case "unban":
		if err := need(1); err != nil {
			return err
		}
		method, route = http.MethodPost, "/unban"
		q.Set("player", args[1])
	case "adapter-key":
		if err := need(2); err != nil {
			return err
		}
		method, route = http.MethodPost, "/adapter-key"
		q.Set("platform", args[1])
		q.Set("key", args[2])
	default:
		return fmt.Errorf("不认识的管理命令 %q", args[0])
	}
	u := "http://" + cfg.Listen.Admin + route
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequest(method, u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("连不上管理接口（节点在跑吗？）：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	fmt.Print(string(body))
	return nil
}
