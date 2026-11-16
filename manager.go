package douyinkit

// 多账号管理
//
// 每个抖音账号一个独立的 Client，数据完全隔离（各自的浏览器 profile / 登录态 /
// 观察名单 / 日志 / 截图），目录结构：
//
//	<Root>/账号1/{session,state,logs,shots}
//	<Root>/账号2/{session,state,logs,shots}
//
// 典型用法：
//
//	m := douyinkit.NewManager("/data/douyin", douyinkit.Options{
//	    Headless:  &on,
//	    AutoReply: &on,
//	    OnQR:      func(qr *douyinkit.QRCode) { ... },
//	})
//	c1, _ := m.Account("shop1")   // 没有就创建，有就复用
//	c2, _ := m.Account("shop2")
//	c1.EnsureLogin(ctx, nil)
//	c1.Run(ctx, false)
//
// 注意：每个账号会启动一个独立浏览器进程，机器内存要够（每个约 300~500MB）。

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

// Manager 多账号管理器。Options 作为模板：Account/BaseDir/Config/AutoReply 之外的字段
// （Headless、回调等）会原样套到每个账号上；Config 会深拷贝，避免账号间互相影响。
type Manager struct {
	root string
	tpl  Options

	mu   sync.Mutex
	accs map[string]*Client
}

var accNameRe = regexp.MustCompile(`^[\w\x{4e00}-\x{9fa5}-]{1,32}$`)

// NewManager 创建多账号管理器。root 是所有账号数据目录的父目录。
func NewManager(root string, tpl Options) (*Manager, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Manager{root: abs, tpl: tpl, accs: map[string]*Client{}}, nil
}

// Account 取（或创建）指定账号的 Client。账号名只允许字母/数字/中文/下划线/横线，
// 同时用作数据目录名。
func (m *Manager) Account(name string) (*Client, error) {
	if !accNameRe.MatchString(name) {
		return nil, fmt.Errorf("账号名不合法（只允许字母/数字/中文/下划线/横线，最长 32）：%q", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.accs[name]; ok {
		return c, nil
	}
	opt := m.tpl
	opt.Account = name
	opt.BaseDir = filepath.Join(m.root, name)
	if m.tpl.Config != nil {
		opt.Config = m.tpl.Config.clone() // 各账号一份独立配置
	}
	c, err := New(opt)
	if err != nil {
		return nil, err
	}
	m.accs[name] = c
	return c, nil
}

// SetAutoReply 按账号开关自动回复（账号不存在返回 false）。
// 关闭后该账号不发送任何消息，二次消息截图照常。
func (m *Manager) SetAutoReply(name string, on bool) bool {
	m.mu.Lock()
	c, ok := m.accs[name]
	m.mu.Unlock()
	if !ok {
		return false
	}
	c.SetAutoReply(on)
	return true
}

// AutoReply 查询某账号当前的自动回复开关。
func (m *Manager) AutoReply(name string) (bool, bool) {
	m.mu.Lock()
	c, ok := m.accs[name]
	m.mu.Unlock()
	if !ok {
		return false, false
	}
	return c.cfg.AutoReply, true
}

// Names 已创建的账号名（按字典序）。
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.accs))
	for name := range m.accs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Close 关闭指定账号的浏览器（账号数据保留，下次 Account(name) 可继续用）。
func (m *Manager) Close(name string) {
	m.mu.Lock()
	c, ok := m.accs[name]
	m.mu.Unlock()
	if ok {
		c.Close()
	}
}

// CloseAll 关闭所有账号的浏览器。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	accs := make([]*Client, 0, len(m.accs))
	for _, c := range m.accs {
		accs = append(accs, c)
	}
	m.mu.Unlock()
	for _, c := range accs {
		c.Close()
	}
}
