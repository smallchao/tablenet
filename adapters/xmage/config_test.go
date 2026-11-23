// SPDX-License-Identifier: MIT

package xmage

import (
	"errors"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	c := Config{Server: "xmage.de:17171"}
	if err := c.defaults(); err != nil || len(c.Servers) != 1 || c.Servers[0].ID != "x" || c.Servers[0].Accounts != "optional" || c.Servers[0].User != "tablenet" {
		t.Fatal("只给一台的简写要拼成一台", c.Servers, err)
	}
	bad := []Config{
		{Servers: []ServerConfig{{ID: "DE", Address: "a:1"}}},
		{Servers: []ServerConfig{{ID: "de", Address: "a:1"}, {ID: "de", Address: "b:1"}}},
		{Servers: []ServerConfig{{ID: "de", Address: "nohost"}}},
		{Servers: []ServerConfig{{ID: "us", Address: "a:1", Accounts: "required"}}},
	}
	for i, b := range bad {
		if err := b.defaults(); err == nil {
			t.Fatalf("第 %d 份配置应当报错", i)
		}
	}
}

func TestQueueServerAndAuthError(t *testing.T) {
	a := &Adapter{servers: []*server{{cfg: ServerConfig{ID: "a"}}, {cfg: ServerConfig{ID: "b", Queue: true}}}}
	if a.queueServer().cfg.ID != "b" {
		t.Fatal("排队开桌要在设了 queue 的那台")
	}
	if !isAuthError(errors.New("Wrong username or password")) || isAuthError(errors.New("Server is likely offline")) {
		t.Fatal("认不准「要注册」")
	}
}
