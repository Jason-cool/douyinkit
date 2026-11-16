package douyinkit

import (
	"os"
	"path/filepath"
	"testing"
)

// 配置传参 / 多账号管理（不依赖浏览器）

func TestDefaultConfigAndClone(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.AutoReply || cfg.ReplyText == "" {
		t.Fatalf("默认配置不对：%+v", cfg)
	}
	cp := cfg.clone()
	cp.QuietHours[0] = "08:00-09:00"
	if cfg.QuietHours[0] == "08:00-09:00" {
		t.Fatal("clone 应深拷贝切片字段")
	}
	var nilCfg *Config
	if nilCfg.clone() != nil {
		t.Fatal("nil clone 应返回 nil")
	}
}

func TestNewConfigOverrides(t *testing.T) {
	// 传参配置优先于 config.json；AutoReply 覆盖最后生效
	base := t.TempDir()
	cfgJSON := `{"reply_text":"来自文件","auto_reply":true}`
	if err := os.WriteFile(filepath.Join(base, "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	off := false
	passed := DefaultConfig()
	passed.ReplyText = "来自传参"
	c, err := New(Options{BaseDir: base, Config: passed, AutoReply: &off})
	if err != nil {
		t.Fatal(err)
	}
	defer c.lg.Close()
	if c.Config().ReplyText != "来自传参" {
		t.Fatalf("Options.Config 应优先于 config.json，实际 %q", c.Config().ReplyText)
	}
	if c.Config().AutoReply {
		t.Fatal("Options.AutoReply=false 应覆盖配置")
	}

	// 不传 Config：读 config.json
	c2, err := New(Options{BaseDir: base})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.lg.Close()
	if c2.Config().ReplyText != "来自文件" {
		t.Fatalf("应读取 config.json，实际 %q", c2.Config().ReplyText)
	}
	if !c2.Config().AutoReply {
		t.Fatal("config.json 里 auto_reply=true 应生效")
	}

	// SetAutoReply 运行时开关
	c2.SetAutoReply(false)
	if c2.Config().AutoReply {
		t.Fatal("SetAutoReply(false) 应生效")
	}
}

func TestManagerAccounts(t *testing.T) {
	root := t.TempDir()
	tpl := DefaultConfig()
	tpl.ReplyText = "店铺话术"
	m, err := NewManager(root, Options{Config: tpl})
	if err != nil {
		t.Fatal(err)
	}

	c1, err := m.Account("店铺A")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, cl := range m.accs {
			cl.lg.Close()
		}
	}()
	if c1.Account() != "店铺A" {
		t.Fatalf("账号名没填上：%q", c1.Account())
	}
	if c1.Config().ReplyText != "店铺话术" {
		t.Fatalf("模板配置没传下去：%q", c1.Config().ReplyText)
	}
	// 数据目录按账号隔离
	if _, err := os.Stat(filepath.Join(root, "店铺A", "state")); err != nil {
		t.Fatalf("账号数据目录没创建：%v", err)
	}

	// 同名复用同一实例
	c1b, err := m.Account("店铺A")
	if err != nil || c1b != c1 {
		t.Fatal("Account 应复用已创建的 Client")
	}

	// 第二个账号：配置深拷贝，互不影响
	c2, err := m.Account("shop-b")
	if err != nil {
		t.Fatal(err)
	}
	c1.Config().ReplyText = "改坏了"
	if c2.Config().ReplyText != "店铺话术" {
		t.Fatal("账号间配置应隔离（深拷贝）")
	}
	if got := m.Names(); len(got) != 2 || got[0] != "shop-b" || got[1] != "店铺A" {
		t.Fatalf("Names 不对：%v", got)
	}

	// 按账号开关自动回复
	if !m.SetAutoReply("shop-b", false) {
		t.Fatal("SetAutoReply 应返回 true")
	}
	if on, ok := m.AutoReply("shop-b"); !ok || on {
		t.Fatal("SetAutoReply(shop-b,false) 应生效")
	}
	if on, ok := m.AutoReply("店铺A"); !ok || !on {
		t.Fatal("未改动的账号应保持开启")
	}
	if m.SetAutoReply("不存在", true) {
		t.Fatal("不存在的账号应返回 false")
	}

	// 非法账号名
	if _, err := m.Account("../逃逸"); err == nil {
		t.Fatal("非法账号名应报错")
	}
	if _, err := m.Account(""); err == nil {
		t.Fatal("空账号名应报错")
	}
}
