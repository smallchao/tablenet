// tablenet-dir：名录服务和根密钥工具（spec/09）。名录只由项目方运营。
//
//	tablenet-dir run   [--config dir.toml]                    启动名录
//	tablenet-dir init  [--config dir.toml]                    写示例配置
//	tablenet-dir admin <status|nodes|delist|undelist|endorse> [参数] [--config dir.toml]
//
//	离线（在不联网的机器上做）：
//	tablenet-dir root keygen  --out root.key                  生成根密钥，打印公钥
//	tablenet-dir root sign-list --key root.key --in list.json --out directories.json [--days 90]
//	tablenet-dir root endorse --key root.key --node <编号> [--days 365] [--note 说明] --out endorse.json
package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/BurntSushi/toml"

	"tablenet/internal/directory"
	"tablenet/pkg/discovery"
	"tablenet/pkg/proto"
)

type config struct {
	Directory struct {
		Region     string   `toml:"region"`
		KeyFile    string   `toml:"key_file"`
		DataDir    string   `toml:"data_dir"`
		ListFile   string   `toml:"list_file"`
		Roots      []string `toml:"roots"`
		MinVersion string   `toml:"min_version"`
		MinNote    string   `toml:"min_note"`
	} `toml:"directory"`
	Listen struct {
		HTTP              string `toml:"http"`
		TLSCert           string `toml:"tls_cert"`
		TLSKey            string `toml:"tls_key"`
		Admin             string `toml:"admin"`
		TrustForwardedFor bool   `toml:"trust_forwarded_for"`
	} `toml:"listen"`
}

const example = `# Tablenet 名录配置（spec/09）。名录只由项目方运营。相对路径按本文件所在目录解析。

[directory]
region = "cn-east"            # 名录所在地区；它兼任这个地区所属探测区（cn／hk／intl）的探测点
key_file = "data/dir.key"     # 第一次启动自动生成；把公钥（启动日志里的 id）写进名录清单
data_dir = "data"
list_file = "directories.json" # 根密钥签的名录清单（tablenet-dir root sign-list 生成）
roots = []                    # 根公钥；空则用代码里写死的
min_version = ""              # 节点软件低于它时提醒升级
min_note = ""

[listen]
http = "0.0.0.0:9443"
# tls_cert = "/etc/letsencrypt/live/dir.example.org/fullchain.pem"
# tls_key  = "/etc/letsencrypt/live/dir.example.org/privkey.pem"
admin = "127.0.0.1:9070"
trust_forwarded_for = false
`

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = run(flagConfig(args))
	case "init":
		err = writeExample(flagConfig(args))
	case "admin":
		err = admin(args)
	case "root":
		err = root(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "出错：", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `用法：
  tablenet-dir run   [--config dir.toml]
  tablenet-dir init  [--config dir.toml]
  tablenet-dir admin status|nodes [--config dir.toml]
  tablenet-dir admin delist <节点编号> [--zones cn,hk] [--reason 原因] [--config dir.toml]
  tablenet-dir admin undelist <节点编号> [--config dir.toml]
  tablenet-dir admin endorse <endorse.json> [--config dir.toml]
  tablenet-dir root keygen --out root.key
  tablenet-dir root sign-list --key root.key --in list.json --out directories.json [--days 90]
  tablenet-dir root endorse --key root.key --node <编号> [--days 365] [--note 说明] --out endorse.json`)
	os.Exit(2)
}

func flagConfig(args []string) string {
	fs := flag.NewFlagSet("", flag.ExitOnError)
	p := fs.String("config", "dir.toml", "配置文件")
	_ = fs.Parse(args)
	return *p
}

func loadConfig(path string) (config, error) {
	var c config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return c, err
	}
	base := filepath.Dir(path)
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.Directory.KeyFile, c.Directory.DataDir, c.Directory.ListFile = abs(c.Directory.KeyFile), abs(c.Directory.DataDir), abs(c.Directory.ListFile)
	c.Listen.TLSCert, c.Listen.TLSKey = abs(c.Listen.TLSCert), abs(c.Listen.TLSKey)
	if len(c.Directory.Roots) == 0 {
		c.Directory.Roots = discovery.DefaultRoots
	}
	if len(c.Directory.Roots) == 0 {
		return c, errors.New("没有根公钥：在 directory.roots 里填，或构建时注入")
	}
	return c, nil
}

func writeExample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s 已存在，不覆盖", path)
	}
	return os.WriteFile(path, []byte(example), 0o600)
}

func readSignedList(path string) (proto.SignedList, error) {
	var s proto.SignedList
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err
}

func adminToken(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "admin.token")
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b)), nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	tok := fmt.Sprintf("%x", proto.RandomBytes(24))
	return tok, os.WriteFile(p, []byte(tok+"\n"), 0o600)
}

func run(path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	key, err := proto.LoadOrCreateKey(cfg.Directory.KeyFile)
	if err != nil {
		return err
	}
	list, err := readSignedList(cfg.Directory.ListFile)
	if err != nil {
		return fmt.Errorf("读名录清单：%w", err)
	}
	directory.TrustForwardedFor = cfg.Listen.TrustForwardedFor
	d, err := directory.New(directory.Config{Region: cfg.Directory.Region, MinVersion: cfg.Directory.MinVersion,
		MinNote: cfg.Directory.MinNote, Roots: cfg.Directory.Roots}, key, list, log)
	if err != nil {
		return err
	}
	if err := d.Load(cfg.Directory.DataDir); err != nil {
		log.Warn("dir.load", "err", err)
	}
	tok, err := adminToken(cfg.Directory.DataDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pub := &http.Server{Addr: cfg.Listen.HTTP, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	adm := &http.Server{Addr: cfg.Listen.Admin, Handler: d.AdminHandler(tok), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() {
		var err error
		if cfg.Listen.TLSCert != "" {
			err = pub.ListenAndServeTLS(cfg.Listen.TLSCert, cfg.Listen.TLSKey)
		} else {
			err = pub.ListenAndServe()
		}
		errs <- err
	}()
	if cfg.Listen.Admin != "" {
		go func() { errs <- adm.ListenAndServe() }()
	}
	go d.Run(ctx)
	log.Info("dir.start", "id", d.ID, "region", cfg.Directory.Region, "http", cfg.Listen.HTTP)
	saver := time.NewTicker(30 * time.Second)
	defer saver.Stop()
	for {
		select {
		case <-saver.C:
			if err := d.Save(cfg.Directory.DataDir); err != nil {
				log.Warn("dir.save", "err", err)
			}
		case err := <-errs:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		case <-ctx.Done():
			_ = d.Save(cfg.Directory.DataDir)
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = pub.Shutdown(sctx)
			_ = adm.Shutdown(sctx)
			return nil
		}
	}
}

// ---- 管理 ----

func admin(args []string) error {
	if len(args) == 0 {
		usage()
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	cfgPath := fs.String("config", "dir.toml", "配置文件")
	zones := fs.String("zones", "", "只在这些探测区下架（逗号分隔），空表示整台")
	reason := fs.String("reason", "", "原因")
	positional := []string{}
	for len(rest) > 0 {
		_ = fs.Parse(rest)
		rest = fs.Args()
		if len(rest) > 0 {
			positional, rest = append(positional, rest[0]), rest[1:]
		}
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	tok, err := adminToken(cfg.Directory.DataDir)
	if err != nil {
		return err
	}
	call := func(method, route string, body io.Reader) error {
		req, _ := http.NewRequest(method, "http://"+cfg.Listen.Admin+route, body)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("连不上管理接口（名录在跑吗？）：%w", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return errors.New(strings.TrimSpace(string(b)))
		}
		fmt.Print(string(b))
		return nil
	}
	need := func() (string, error) {
		if len(positional) == 0 {
			return "", fmt.Errorf("%s 需要一个参数", sub)
		}
		return positional[0], nil
	}
	switch sub {
	case "status", "nodes":
		return call(http.MethodGet, "/"+sub, nil)
	case "delist", "undelist":
		node, err := need()
		if err != nil {
			return err
		}
		q := url.Values{"node": {node}, "zones": {*zones}, "reason": {*reason}}
		if sub == "undelist" {
			q.Set("lift", "1")
		}
		return call(http.MethodPost, "/delist?"+q.Encode(), nil)
	case "endorse":
		file, err := need()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		return call(http.MethodPost, "/endorse", bytes.NewReader(b))
	}
	return fmt.Errorf("不认识的管理命令 %q", sub)
}

// ---- 根密钥（离线） ----

func root(args []string) error {
	if len(args) == 0 {
		usage()
	}
	sub := args[0]
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	out := fs.String("out", "", "输出文件")
	keyPath := fs.String("key", "", "根密钥文件")
	in := fs.String("in", "", "清单原文（JSON：seq、directories、probes）")
	days := fs.Int("days", 0, "有效天数")
	node := fs.String("node", "", "节点编号")
	note := fs.String("note", "", "说明")
	_ = fs.Parse(args[1:])
	switch sub {
	case "keygen":
		if *out == "" {
			return errors.New("要 --out")
		}
		if _, err := os.Stat(*out); err == nil {
			return fmt.Errorf("%s 已存在，不覆盖", *out)
		}
		k, err := proto.LoadOrCreateKey(*out)
		if err != nil {
			return err
		}
		fmt.Println("根公钥：", proto.IDOfKey(k))
		fmt.Println("私钥在", *out, "——离线保存，不要放在任何联网的机器上。主钥和备用钥各生成一把。")
		return nil
	case "sign-list":
		k, err := proto.LoadKey(*keyPath)
		if err != nil {
			return err
		}
		var l proto.DirectoryList
		b, err := os.ReadFile(*in)
		if err == nil {
			err = json.Unmarshal(b, &l)
		}
		if err != nil {
			return fmt.Errorf("读清单原文：%w", err)
		}
		if *days == 0 {
			*days = 90
		}
		l.IssuedAt = time.Now().UTC().Truncate(time.Second)
		l.ExpiresAt = l.IssuedAt.AddDate(0, 0, *days)
		if l.Seq == 0 {
			l.Seq = l.IssuedAt.Unix()
		}
		s, err := proto.SignDirectoryList(k, l)
		if err != nil {
			return err
		}
		return writeOut(*out, s)
	case "endorse":
		k, err := proto.LoadKey(*keyPath)
		if err != nil {
			return err
		}
		if proto.KeyOf(*node) == nil {
			return errors.New("--node 不是合法的节点编号")
		}
		if *days == 0 {
			*days = 365
		}
		now := time.Now().UTC().Truncate(time.Second)
		s, err := proto.SignBy(proto.CtxEndorsement, k, proto.Endorsement{NodeID: *node, IssuedAt: now, ExpiresAt: now.AddDate(0, 0, *days), Note: *note})
		if err != nil {
			return err
		}
		return writeOut(*out, s)
	}
	return fmt.Errorf("不认识的 root 子命令 %q", sub)
}

func writeOut(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if path == "" {
		fmt.Println(string(b))
		return nil
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
