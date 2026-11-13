package douyinkit

// Client —— 对外主 API。
//
//	用法（作为库被引用）：
//	  c, _ := douyinkit.New(douyinkit.Options{BaseDir: "/data/douyin"})
//	  defer c.Close()
//	  err := c.EnsureLogin(ctx, func(qr *douyinkit.QRCode) {
//	      // 把 qr.DataURL 交给前端渲染；二维码刷新时会重复回调
//	  })
//	  c.Run(ctx, false) // 开始自动回复
//
// 登录态持久化在 BaseDir/session/profile（浏览器用户目录），
// 冷却记录在 BaseDir/state/replied.json，日志在 BaseDir/logs/。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/go-rod/rod"
)

// Options 创建 Client 的配置项
type Options struct {
	BaseDir   string                               // 数据目录（profile/logs/state/config）。默认 ./douyinkit-data
	Channel   string                               // 浏览器选择：auto / chrome / chromium / msedge / bundled。默认 auto
	Headless  *bool                                // nil → Linux 下 true（服务器无头），其他平台 false（本地调试弹窗）
	Logf      func(format string, args ...any)     // 日志钩子；nil 则输出到 stdout
	OnQR      func(qr *QRCode)                     // 全局二维码回调（每次刷新都会调用，可为 nil）
	OnReply   func(name, result string, sent bool) // 每处理一个会话后的回调（可为 nil）
	QRRefresh time.Duration                        // 等扫码期间二维码重抓周期，默认 30 秒
}

// Client 抖音私信助手客户端
type Client struct {
	opt      Options
	cfg      *Config
	hist     map[string]string
	lg       *Logger
	base     string
	headless bool

	mu      sync.Mutex
	browser *rod.Browser
	page    *rod.Page
	closeFn func()
	lastQR  *QRCode
}

// New 创建客户端（不会立刻启动浏览器；Connect/EnsureLogin 时才启动）
func New(opt Options) (*Client, error) {
	base := opt.BaseDir
	if base == "" {
		base = "./douyinkit-data"
	}
	abs, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"", "state", "logs", "shots"} {
		if err := os.MkdirAll(filepath.Join(abs, d), 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败：%w", err)
		}
	}
	if opt.Channel == "" {
		opt.Channel = "auto"
	}
	headless := runtime.GOOS == "linux"
	if opt.Headless != nil {
		headless = *opt.Headless
	}
	lg := newLogger(abs)
	lg.Hook = opt.OnReply
	return &Client{
		opt:      opt,
		cfg:      loadConfig(filepath.Join(abs, "config.json")),
		hist:     loadHistory(historyPath(abs)),
		lg:       lg,
		base:     abs,
		headless: headless,
	}, nil
}

// Connect 启动（或复用）浏览器并打开私信页。已连接时是幂等的。
// 注意：未登录不算错误 —— 用 IsLoggedIn/EnsureLogin 处理登录。
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked()
}

func (c *Client) connectLocked() error {
	if c.page != nil && pageAlive(c.page) {
		return nil
	}
	if c.closeFn != nil {
		c.closeFn()
		c.closeFn, c.page, c.browser = nil, nil, nil
	}
	c.lg.Log("启动浏览器（headless=%v）...", c.headless)
	b, page, closeFn, err := openPage(c.opt.Channel, c.base, c.headless)
	if err != nil {
		return err
	}
	c.browser, c.page, c.closeFn = b, page, closeFn
	// 若 profile 里已有登录态，等 SSO 自动登录完成（实测 15-20 秒）；未登录则不阻塞
	wait(5000)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if hasAuthCookie(b) && !loginPageShown(page) {
			break
		}
		if !hasAuthCookie(b) {
			break // 未登录 → 交给 EnsureLogin 走二维码流程
		}
		if time.Now().After(deadline) {
			break
		}
		wait(2000)
	}
	wait(3000)
	c.lg.Log("浏览器就绪")
	return nil
}

// Close 关闭浏览器并释放资源
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeFn != nil {
		c.closeFn()
		c.closeFn, c.page, c.browser = nil, nil, nil
	}
}

// IsLoggedIn 当前是否已登录（会按需 Connect）
func (c *Client) IsLoggedIn() bool {
	if err := c.Connect(); err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loggedInLocked()
}

func (c *Client) loggedInLocked() bool {
	if c.browser == nil || c.page == nil {
		return false
	}
	return hasAuthCookie(c.browser) && !loginPageShown(c.page)
}

// EnsureLogin 确保已登录：已登录直接返回 nil；
// 未登录则抓取二维码回调给 onQR（连同 Options.OnQR），之后持续监控：
// 二维码自动刷新时会再次回调，直到扫码成功或 ctx 取消。
func (c *Client) EnsureLogin(ctx context.Context, onQR func(qr *QRCode)) error {
	if err := c.Connect(); err != nil {
		return err
	}
	c.mu.Lock()
	logged := c.loggedInLocked()
	c.mu.Unlock()
	if logged {
		c.lg.Log("已有有效登录态，无需扫码")
		return nil
	}

	push := func(qr *QRCode) {
		c.setLastQR(qr)
		if onQR != nil {
			onQR(qr)
		}
		if c.opt.OnQR != nil {
			c.opt.OnQR(qr)
		}
	}

	qr, err := c.FetchQR()
	if err != nil {
		return fmt.Errorf("抓取登录二维码失败：%w", err)
	}
	push(qr)
	c.lg.Log("已抓取登录二维码，等待扫码（最长等到 ctx 取消）...")

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	qrRefresh := c.opt.QRRefresh
	if qrRefresh <= 0 {
		qrRefresh = 30 * time.Second
	}
	nextRefresh := time.Now().Add(qrRefresh)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.mu.Lock()
			logged := c.loggedInLocked()
			c.mu.Unlock()
			if logged {
				c.lg.Log("检测到登录成功！会话已保存到 session/profile")
				// 从登录态切到工作页面：重载私信页，并等会话列表渲染完，
				// 否则紧接着的 RunOnce 会「没扫到会话」
				c.mu.Lock()
				_ = c.page.Navigate(chatURL)
				c.mu.Unlock()
				c.waitForConvs(30 * time.Second)
				return nil
			}
			if time.Now().After(nextRefresh) {
				if qr2, e := c.FetchQR(); e == nil {
					if qr2.Base64 != qr.Base64 {
						qr = qr2
						push(qr)
						c.lg.Log("二维码已刷新")
					}
				}
				nextRefresh = time.Now().Add(qrRefresh)
			}
		}
	}
}

// waitForConvs 轮询等会话列表渲染出来（最多 timeout），返回是否扫到
func (c *Client) waitForConvs(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if n := len(evalConvList(c.page)); n > 0 {
			c.lg.Log("会话列表已就绪（%d 项）", n)
			return true
		}
		if time.Now().After(deadline) {
			c.lg.Log("等待会话列表超时（%v），页面可能还没加载完", timeout)
			return false
		}
		wait(2000)
	}
}

// RunOnce 执行一轮扫描回复。dry=true 只判定不发送。返回真实发送条数。
func (c *Client) RunOnce(dry bool) (int, error) {
	if err := c.Connect(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loggedInLocked() {
		return 0, errors.New("未登录：请先调用 EnsureLogin 完成扫码")
	}
	c.waitForConvs(30 * time.Second) // 列表没渲染完就扫会扑空
	return runOnce(c.page, c.cfg, dry, c.hist, c.lg, c.base, false, false), nil
}

// SendTo 定向回复指定会话（按昵称匹配，忽略未读标记）。dry=true 只判定不发送。
// 用于上线前小范围验证发送链路，避免一次性给所有未读会话发消息。
func (c *Client) SendTo(name string, dry bool) (int, error) {
	if err := c.Connect(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loggedInLocked() {
		return 0, errors.New("未登录：请先调用 EnsureLogin 完成扫码")
	}
	c.waitForConvs(30 * time.Second)
	return replyToNamed(c.page, name, c.cfg, dry, c.hist, c.lg, c.base), nil
}

// Run 持续轮询（等价 Windows 版 -send -loop）。页面断开会自动重连（各 3 次重试）。
func (c *Client) Run(ctx context.Context, dry bool) error {
	if err := c.Connect(); err != nil {
		return err
	}
	if !c.IsLoggedIn() {
		return errors.New("未登录：请先调用 EnsureLogin 完成扫码")
	}
	interval := c.cfg.PollIntervalSeconds
	if interval <= 0 {
		interval = 25
	}
	c.mu.Lock()
	c.waitForConvs(30 * time.Second)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		n := runOnce(c.page, c.cfg, dry, c.hist, c.lg, c.base, false, false)
		page := c.page
		c.mu.Unlock()
		_ = n
		c.lg.Log("本轮结束，共处理 %d 条", n)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(interval) * time.Second):
		}

		// 页面健康检查：瞬时不可用重试 3 次；真断了重连浏览器（最多 3 次）
		c.mu.Lock()
		alive := pageAlive(page)
		c.mu.Unlock()
		if !alive {
			alive = false
			for i := 0; i < 3; i++ {
				wait(2000)
				c.mu.Lock()
				alive = pageAlive(c.page)
				c.mu.Unlock()
				if alive {
					c.lg.Log("  页面短暂不可用，已恢复")
					break
				}
			}
			if !alive {
				c.lg.Log("! 页面被关闭或浏览器已退出，尝试重新拉起浏览器 ...")
				ok := false
				for attempt := 1; attempt <= 3; attempt++ {
					c.mu.Lock()
					err := c.connectLocked()
					c.mu.Unlock()
					if err == nil {
						ok = true
						break
					}
					c.lg.Log("  第 %d 次重连失败：%v（30 秒后重试）", attempt, err)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(30 * time.Second):
					}
				}
				if !ok {
					return errors.New("重连 3 次均失败，退出")
				}
				c.lg.Log("浏览器已重新就绪，继续轮询")
			}
		}
	}
}

// Config 运行配置（可在 EnsureLogin 前修改）
func (c *Client) Config() *Config { return c.cfg }
