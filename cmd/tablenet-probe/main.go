// tablenet-probe：独立探测点（spec/09）。跑在某个探测区（第一版是海外 intl）的一台小云主机上：
// 定期向名录要全部节点名片，逐个连一次，把签了名的探测结果报回名录。不存任何东西。
//
//	tablenet-probe --dir https://dir-hk.example.org --zone intl [--key probe.key] [--every 5m]
//
// 探测点的公钥要写进名录清单的 probes 里，名录才认它报的结果。
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"tablenet/internal/directory"
	"tablenet/pkg/discovery"
	"tablenet/pkg/proto"
)

func main() {
	dir := flag.String("dir", "", "名录地址")
	zone := flag.String("zone", "intl", "本探测点所在的探测区：cn／hk／intl")
	keyPath := flag.String("key", "probe.key", "探测点私钥（没有就生成）")
	every := flag.Duration("every", 5*time.Minute, "多久探测一轮")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "要 --dir")
		os.Exit(2)
	}
	key, err := proto.LoadOrCreateKey(*keyPath)
	if err != nil {
		log.Error("key", "err", err)
		os.Exit(1)
	}
	log.Info("probe.start", "id", proto.IDOfKey(key), "zone", *zone, "dir", *dir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for {
		if err := round(ctx, *dir, *zone, key, log); err != nil {
			log.Warn("probe.round", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*every):
		}
	}
}

func round(ctx context.Context, dir, zone string, key ed25519.PrivateKey, log *slog.Logger) error {
	// 要全部名片（不按地区、信任档过滤）：all=1。
	resp, err := http.Get(strings.TrimRight(dir, "/") + "/dir/v1/nodes?all=1")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body struct {
		Nodes []discovery.Listing `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	var mu sync.Mutex
	obs := []proto.SignedBy{}
	cards := []proto.NodeCard{}
	for _, l := range body.Nodes {
		if c, err := proto.VerifyCard(l.Card, time.Now()); err == nil {
			cards = append(cards, c)
		}
	}
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, c := range cards {
		c := c
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s, err := directory.ProbeNode(ctx, c, zone, key)
			if err == nil {
				mu.Lock()
				obs = append(obs, s)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	b, _ := json.Marshal(map[string]any{"observations": obs})
	presp, err := http.Post(strings.TrimRight(dir, "/")+"/dir/v1/observations", "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	presp.Body.Close()
	log.Info("probe.round", "nodes", len(cards), "reported", len(obs))
	return nil
}
