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
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
)

// Options 创建 Client 的配置项
type Options struct {
	BaseDir    string                               // 数据目录（profile/logs/state/config）。默认 ./douyinkit-data；多账号时传根目录，用 Manager 分账号
	Channel    string                               // 浏览器选择：auto / chrome / chromium / msedge / bundled。默认 auto
	Headless   *bool                                // nil → Linux 下 true（服务器无头），其他平台 false（本地调试弹窗）
	Logf       func(format string, args ...any)     // 日志钩子；nil 则输出到 stdout
	OnQR       func(qr *QRCode)                     // 全局二维码回调（每次刷新都会调用，可为 nil）
	OnReply    func(name, result string, sent bool) // 每处理一个会话后的回调（可为 nil）
	OnFollowUp func(f FollowUp)                     // 自动回复过之后客户再来消息：截图完成回调（可为 nil）
	QRRefresh  time.Duration                        // 等扫码期间二维码重抓周期，默认 30 秒

	Account        string  // 账号名：日志前缀；用 Manager 管理多账号时会自动填写
	Config         *Config // 配置传参（推荐 douyinkit.DefaultConfig() 改好传入）；nil → 读 BaseDir/config.json，没有则默认值
	AutoReply      *bool   // 该账号是否自动回复；nil = 不覆盖。false 时只做二次消息截图/观察，不发送任何消息
	Preheat        bool    // 创建后自动后台预热（常驻模式）：提前启动浏览器并预取登录二维码。也可手动调 Client.Preheat / Manager.Preheat
	AutoReceiveSMS bool    // 额外身份验证时自动选择一次“接收短信验证码”；重发始终需显式调用 SendSMSCode
}

// Client 抖音私信助手客户端
type Client struct {
	opt      Options
	cfg      *Config
	hist     map[string]string
	fu       *followStore
	lg       *Logger
	base     string
	headless bool
	cfgMu    sync.RWMutex

	mu      sync.Mutex
	browser *rod.Browser
	page    *rod.Page
	closeFn func()
	// Snapshots have their own lock; HTTP reads never wait for browser work.
	snapshotMu sync.RWMutex
	lastQR     *QRCode
	loggedIn   atomic.Bool
	loginStage string
	loginError string
	// Guard the SSO transition after the QR disappears. No Cookie yet does
	// not mean the browser is a guest or that its storage may be cleared.
	loginHadQR          bool
	loginUncertainSince time.Time
	lastLoginRecovery   time.Time
	verification        *SMSVerification // snapshotMu protects this snapshot
	smsID               string
	smsResendAt         time.Time
	smsSubmittingUntil  time.Time

	// 启动失败退避：避免 HTTP 轮询（/qr）每 3 秒触发一次浏览器启动，失败后空转
	bootAt  time.Time
	bootErr error

	// 预热去重：Preheat 异步跑，多个 goroutine 同时调只跑一次
	preheatMu     sync.Mutex
	preheating    bool
	preheatCancel context.CancelFunc
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
	lg.UserLog = opt.Logf
	lg.Hook = opt.OnReply
	lg.FollowHook = opt.OnFollowUp
	// 多账号时给日志加账号前缀，方便区分不同账号的输出
	if opt.Account != "" {
		if inner := lg.UserLog; inner != nil {
			name := opt.Account
			lg.UserLog = func(format string, args ...any) {
				inner("[%s] %s", name, fmt.Sprintf(format, args...))
			}
		} else {
			name := opt.Account
			lg.UserLog = func(format string, args ...any) {
				fmt.Printf("[%s] %s\n", name, fmt.Sprintf(format, args...))
			}
		}
	}
	// 配置优先级：Options.Config（传参）> BaseDir/config.json（文件）> 默认值；
	// Options.AutoReply 是最后一级覆盖，方便只开关自动回复而不用动整份配置
	cfg := loadConfig(filepath.Join(abs, "config.json"))
	if opt.Config != nil {
		cfg = opt.Config.clone()
	}
	if opt.AutoReply != nil {
		cfg.AutoReply = *opt.AutoReply
	}
	hist := loadHistory(historyPath(abs))
	fu, fuErr := loadFollowStore(abs)
	if fuErr != nil {
		lg.Log("! 二次消息名单读取失败（按空名单继续）：%v", fuErr)
	}
	// 升级前已经回复过的会话也纳入观察名单，避免升级后对同一个人再发一遍
	if cfg.FollowUpNoReply {
		if n := fu.seedFromHistory(hist, cfg.ReplyText); n > 0 {
			lg.Log("二次消息观察名单：从历史回复记录补入 %d 个会话（最近 %d 天）", n, followSeedDays)
		}
	}
	c := &Client{
		opt:      opt,
		cfg:      cfg,
		hist:     hist,
		fu:       fu,
		lg:       lg,
		base:     abs,
		headless: headless,
	}
	if opt.Preheat { // 显式开启常驻模式：创建即后台预热
		c.Preheat()
	}
	return c, nil
}

// Preheat 后台预热（常驻模式，异步、立即返回、可重复调用）。
//
// 做两件事：
//  1. 提前启动浏览器并打开私信页（Connect）；
//  2. 未登录时提前抓好登录二维码放进缓存，之后的 QRStatus/EnsureLogin 秒级返回。
//
// 典型场景（CentOS 扫码慢）：服务一启动就预热，把「浏览器冷启动 + 登录页加载 +
// 风控挑战」全部挪到用户扫码之前，用户要扫码时二维码已经是现成的。
// 已登录的账号预热后浏览器常驻内存，后续轮询/发送不再经历冷启动。
//
// 预热失败不影响主流程：只打日志，主逻辑用到时按原有路径自动重试。
// 与主逻辑并发安全：内部走同一把锁，主逻辑先到就先处理，预热本轮自动让路。
func (c *Client) Preheat() {
	c.preheatMu.Lock()
	if c.preheating {
		c.preheatMu.Unlock()
		return
	}
	c.preheating = true
	ctx, cancel := context.WithCancel(context.Background())
	c.preheatCancel = cancel
	c.preheatMu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				c.lg.Log("! 预热异常（不影响主流程）：%v", r)
			}
			c.preheatMu.Lock()
			c.preheating = false
			c.preheatCancel = nil
			c.preheatMu.Unlock()
		}()
		if err := c.preheat(ctx); err != nil && ctx.Err() == nil {
			c.lg.Log("! 预热未完成（不影响主流程，用时自动重试）：%v", err)
		}
	}()
}

func (c *Client) preheat(ctx context.Context) error {
	t0 := time.Now()
	// QRStatus 内部：带退避的 Connect → 登录检测 → 未登录则抓码缓存（含骨架屏自愈）。
	// 全程复用主逻辑的锁与状态，预热完的主逻辑零改动受益。
	st, err := c.qrStatusContext(ctx)
	if err != nil {
		return err
	}
	d := time.Since(t0).Round(time.Second)
	switch {
	case st.LoggedIn:
		c.lg.Log("预热完成（%v）：已有登录态，浏览器常驻就绪", d)
	case st.DataURL != "":
		c.lg.Log("预热完成（%v）：登录二维码已提前抓好，扫码时秒出", d)
	default:
		c.lg.Log("预热让路：主流程正在使用浏览器（本轮跳过，不影响任何操作）")
	}
	return nil
}

// Connect 启动（或复用）浏览器并打开私信页。已连接时是幂等的。
// 注意：未登录不算错误 —— 用 IsLoggedIn/EnsureLogin 处理登录。
func (c *Client) Connect() error {
	return c.connectContext(context.Background())
}

func (c *Client) connectContext(ctx context.Context) error {
	if err := c.lockContext(ctx); err != nil {
		return err
	}
	defer c.mu.Unlock()
	return c.connectContextLocked(ctx)
}

func (c *Client) connectContextLocked(ctx context.Context) error {
	if c.page != nil && pageAlive(c.page) {
		return nil
	}
	if c.closeFn != nil {
		c.closeFn()
		c.closeFn, c.page, c.browser = nil, nil, nil
	}
	c.lg.Log("启动浏览器（headless=%v）...", c.headless)
	started := time.Now()
	c.loggedIn.Store(false)
	c.loginHadQR = false
	c.loginUncertainSince = time.Time{}
	c.lastLoginRecovery = time.Time{}
	c.clearVerificationLocked()
	c.setLastQR(nil)
	c.setLoginProgress("booting", nil)
	b, page, closeFn, err := openPage(ctx, c.opt.Channel, c.base, c.headless, c.lg.Log)
	if err != nil {
		c.setLoginProgress("error", err)
		return err
	}
	c.browser, c.page, c.closeFn = b, page, closeFn
	// Browser readiness is separate from login readiness. EnsureLogin waits for
	// actual page state; an old cookie must not add a fixed 68-second delay here.
	c.lg.Log("浏览器连接就绪（%v），登录状态由页面检测确认", time.Since(started).Round(time.Millisecond))
	return nil
}

// connectWithBackoffLocked 与 connectLocked 相同，但启动失败后 10 秒内不再重试。
// 前端每 3 秒轮询 /qr（QRStatus）时会走到这里：一次启动失败要十几秒，
// 不设退避就会变成「失败→重试→再失败」的空转，CPU 和日志都受不了。
func (c *Client) connectWithBackoffContextLocked(ctx context.Context) error {
	if c.page != nil && pageAlive(c.page) {
		return nil
	}
	if c.bootErr != nil && time.Since(c.bootAt) < 10*time.Second {
		return c.bootErr
	}
	if err := c.connectContextLocked(ctx); err != nil {
		c.bootAt, c.bootErr = time.Now(), err
		return err
	}
	c.bootAt, c.bootErr = time.Time{}, nil
	return nil
}

// Close 关闭浏览器并释放资源
func (c *Client) Close() {
	c.preheatMu.Lock()
	if c.preheatCancel != nil {
		c.preheatCancel()
	}
	c.preheatMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeFn != nil {
		c.closeFn()
		c.closeFn, c.page, c.browser = nil, nil, nil
	}
	c.loggedIn.Store(false)
	c.clearVerificationLocked()
	c.setLastQR(nil)
	c.setLoginProgress("", nil)
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
	state, err := c.observeLoginLocked(context.Background())
	return err == nil && state.ready()
}

// IsLoggedInCached 读取最近一次后台确认的登录状态，不加浏览器锁、不调用 CDP。
func (c *Client) IsLoggedInCached() bool {
	return c.loggedIn.Load()
}

// EnsureLogin 确保已登录：已登录直接返回 nil；
// 未登录则抓取二维码回调给 onQR（连同 Options.OnQR），之后持续监控：
// 二维码自动刷新时会再次回调，直到扫码成功或 ctx 取消。
func (c *Client) EnsureLogin(ctx context.Context, onQR func(qr *QRCode)) error {
	if err := c.connectContext(ctx); err != nil {
		return err
	}
	started := time.Now()
	var lastCode string
	var lastFetch time.Time
	var lastNotice time.Time
	probeFailures := 0
	for {
		if err := c.lockContext(ctx); err != nil {
			return err
		}
		state, probeErr := c.observeLoginLocked(ctx)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if probeErr != nil {
			probeFailures++
			if probeFailures >= 3 {
				err := fmt.Errorf("连续 3 次登录检测失败，需要恢复浏览器：%w", probeErr)
				c.setLoginProgress("error", err)
				return err
			}
		} else {
			probeFailures = 0
		}
		if probeErr == nil && state.ready() {
			c.lg.Log("登录已确认（%v）：Cookie 有效，私信页面就绪且无登录弹窗", time.Since(started).Round(time.Millisecond))
			return nil
		}
		if lastNotice.IsZero() || time.Since(lastNotice) >= 15*time.Second {
			lastNotice = time.Now()
			c.lg.Log("登录检测：cookie=%v 私信结构=%v 登录弹窗=%v 二维码=%v 身份验证=%v 页面=%s 检测错误=%v", state.Authenticated, state.ChatReady, state.LoginVisible, state.QRVisible, state.VerificationRequired, state.PageURL, probeErr)
		}
		if probeErr == nil && (state.VerificationRequired || !state.QRVisible) {
			if err := c.lockContext(ctx); err != nil {
				return err
			}
			err := c.handleLoginPendingLocked(ctx, state)
			c.mu.Unlock()
			if err != nil && !errors.Is(err, errAuthPending) && !errors.Is(err, errVerificationRequired) {
				return err
			}
		} else if probeErr == nil {
			refresh := c.opt.QRRefresh
			if refresh <= 0 {
				refresh = 30 * time.Second
			}
			if lastFetch.IsZero() || time.Since(lastFetch) >= refresh {
				if err := c.lockContext(ctx); err != nil {
					return err
				}
				qr, err := c.fetchQRContextLocked(ctx)
				c.mu.Unlock()
				lastFetch = time.Now()
				if err == nil && qr != nil && qr.Base64 != lastCode {
					lastCode = qr.Base64
					if onQR != nil {
						onQR(qr)
					}
					if c.opt.OnQR != nil {
						c.opt.OnQR(qr)
					}
					c.lg.Log("登录二维码已更新，等待扫码...")
				} else if err != nil && !errors.Is(err, errAuthPending) && !errors.Is(err, errVerificationRequired) && ctx.Err() == nil {
					err = fmt.Errorf("抓取登录二维码失败：%w", err)
					c.setLoginProgress("error", err)
					c.lg.Log("! %v", err)
					return err
				}
			}
		}
		if err := waitContext(ctx, time.Second); err != nil {
			return err
		}
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
	return runOnce(c.page, c.runtimeConfig(), dry, c.hist, c.lg, c.base, c.fu, false, false), nil
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
	cfg := c.runtimeConfig()
	if !cfg.AutoReply {
		return 0, errors.New("该账号已关闭自动回复（SetAutoReply(false) / Options.AutoReply=false）")
	}
	return replyToNamed(c.page, name, cfg, dry, c.hist, c.lg, c.base, c.fu), nil
}

// Run 持续轮询（等价 Windows 版 -send -loop）。页面断开会自动重连（各 3 次重试）。
func (c *Client) Run(ctx context.Context, dry bool) error {
	if err := c.connectContext(ctx); err != nil {
		return err
	}
	if !c.settleLogin(ctx, 15*time.Second) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("未登录：请先调用 EnsureLogin 完成扫码")
	}
	interval := c.runtimeConfig().PollIntervalSeconds
	if interval <= 0 {
		interval = 25
	}
	for {
		if err := c.lockContext(ctx); err != nil {
			return err
		}
		state, err := c.observeLoginLocked(ctx)
		if err != nil || !state.ready() {
			c.mu.Unlock()
			return fmt.Errorf("登录状态暂不可用（cookie=%v 私信结构=%v 登录弹窗=%v 检测错误=%v），请重新确认登录后恢复轮询", state.Authenticated, state.ChatReady, state.LoginVisible, err)
		}
		n := runOnce(c.page.Context(ctx), c.runtimeConfig(), dry, c.hist, c.lg, c.base, c.fu, false, false)
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
				if err := waitContext(ctx, 2*time.Second); err != nil {
					return err
				}
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
					err := c.connectContextLocked(ctx)
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

// Account 当前账号名（Manager 创建时填写；单账号直连时为空）
func (c *Client) Account() string { return c.opt.Account }

// SetAutoReply 运行时开关该账号的自动回复，下一次轮询生效。
// 关掉后：不再发送任何消息，但二次消息截图/观察照常。
func (c *Client) SetAutoReply(on bool) {
	c.cfgMu.Lock()
	c.cfg.AutoReply = on
	c.cfgMu.Unlock()
	if c.lg != nil {
		if on {
			c.lg.Log("已开启自动回复")
		} else {
			c.lg.Log("已关闭自动回复（不再发送任何消息，二次消息照常截图）")
		}
	}
}

func (c *Client) runtimeConfig() *Config {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cfg.clone()
}

// FollowUps 返回「已自动回复、正在观察客户二次消息」的会话（最近的排前面）。
// 记录里带最近一次截图路径，可直接给前端/后台展示。
func (c *Client) FollowUps() []FollowUp { return c.fu.list() }

// Unwatch 把某个会话移出观察名单（比如你人工接手了这个客户），
// 之后它再来消息会重新按普通会话自动回复。返回是否真的移除了。
func (c *Client) Unwatch(name string) bool { return c.fu.remove(name) }
