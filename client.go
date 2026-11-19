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
	BaseDir    string                               // 数据目录（profile/logs/state/config）。默认 ./douyinkit-data；多账号时传根目录，用 Manager 分账号
	Channel    string                               // 浏览器选择：auto / chrome / chromium / msedge / bundled。默认 auto
	Headless   *bool                                // nil → Linux 下 true（服务器无头），其他平台 false（本地调试弹窗）
	Logf       func(format string, args ...any)     // 日志钩子；nil 则输出到 stdout
	OnQR       func(qr *QRCode)                     // 全局二维码回调（每次刷新都会调用，可为 nil）
	OnReply    func(name, result string, sent bool) // 每处理一个会话后的回调（可为 nil）
	OnFollowUp func(f FollowUp)                     // 自动回复过之后客户再来消息：截图完成回调（可为 nil）
	QRRefresh  time.Duration                        // 等扫码期间二维码重抓周期，默认 30 秒

	Account   string  // 账号名：日志前缀；用 Manager 管理多账号时会自动填写
	Config    *Config // 配置传参（推荐 douyinkit.DefaultConfig() 改好传入）；nil → 读 BaseDir/config.json，没有则默认值
	AutoReply *bool   // 该账号是否自动回复；nil = 不覆盖。false 时只做二次消息截图/观察，不发送任何消息
	Preheat   bool    // 创建后自动后台预热（常驻模式）：提前启动浏览器并预取登录二维码。也可手动调 Client.Preheat / Manager.Preheat
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

	mu      sync.Mutex
	browser *rod.Browser
	page    *rod.Page
	closeFn func()
	lastQR  *QRCode

	// 启动失败退避：避免 HTTP 轮询（/qr）每 3 秒触发一次浏览器启动，失败后空转
	bootAt  time.Time
	bootErr error

	// 预热去重：Preheat 异步跑，多个 goroutine 同时调只跑一次
	preheatMu  sync.Mutex
	preheating bool
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
	c.preheatMu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.lg.Log("! 预热异常（不影响主流程）：%v", r)
			}
			c.preheatMu.Lock()
			c.preheating = false
			c.preheatMu.Unlock()
		}()
		if err := c.preheat(); err != nil {
			c.lg.Log("! 预热未完成（不影响主流程，用时自动重试）：%v", err)
		}
	}()
}

func (c *Client) preheat() error {
	t0 := time.Now()
	// QRStatus 内部：带退避的 Connect → 登录检测 → 未登录则抓码缓存（含骨架屏自愈）。
	// 全程复用主逻辑的锁与状态，预热完的主逻辑零改动受益。
	st, err := c.QRStatus()
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

// connectWithBackoffLocked 与 connectLocked 相同，但启动失败后 10 秒内不再重试。
// 前端每 3 秒轮询 /qr（QRStatus）时会走到这里：一次启动失败要十几秒，
// 不设退避就会变成「失败→重试→再失败」的空转，CPU 和日志都受不了。
func (c *Client) connectWithBackoffLocked() error {
	if c.page != nil && pageAlive(c.page) {
		return nil
	}
	if c.bootErr != nil && time.Since(c.bootAt) < 10*time.Second {
		return c.bootErr
	}
	if err := c.connectLocked(); err != nil {
		c.bootAt, c.bootErr = time.Now(), err
		return err
	}
	c.bootAt, c.bootErr = time.Time{}, nil
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

// IsLoggedInCached 只读当前登录状态：不启动浏览器、不排队等锁。
// 给 HTTP 轮询（/qr）用 —— IsLoggedIn 每次都 Connect 并抢锁，抓二维码时页面会被拖住。
func (c *Client) IsLoggedInCached() bool {
	if !c.mu.TryLock() {
		return false // 正在抓二维码/处理会话，本次按未登录返回，前端稍后再问
	}
	defer c.mu.Unlock()
	return c.loggedInLocked()
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
	if errors.Is(err, errAuthPending) {
		// 刚扫码/已有有效 cookie：按登录成功收尾，绝不重新抓码
		//（重抓一旦扑空就会触发清 cookie，把刚建立的登录态抹掉）
		c.lg.Log("检测到登录 cookie，等待页面切换（不重新抓码、不清 cookie）...")
		if c.settleLogin(45 * time.Second) {
			c.lg.Log("检测到登录成功！会话已保存到 session/profile")
			return nil
		}
		c.lg.Log("登录 cookie 无法进入私信页（可能已失效），清理后重新出码 ...")
		c.forceResetGuest()
		qr, err = c.FetchQR()
	}
	if err != nil {
		// 首次抓取失败通常是页面还在渲染/残留登录态，重试而不是直接退出（否则 HTTP 服务跟着挂）
		for attempt := 2; attempt <= 3; attempt++ {
			c.lg.Log("! 抓取二维码失败（第 %d 次）：%v，3 秒后重试 ...", attempt-1, err)
			wait(3000)
			if qr, err = c.FetchQR(); err == nil {
				break
			}
		}
	}
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
	// 扫码后 cookie 已到、登录浮层还没消失时的收尾尝试计数（跳转慢，别无限重试）
	settleTries := 0
	lastSettle := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.mu.Lock()
			logged := c.loggedInLocked()
			authCooked := c.browser != nil && hasAuthCookie(c.browser)
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
			// 已拿到登录 cookie，但页面仍停在登录浮层（扫码成功后跳转慢/浮层没关）：
			// 主动切到私信页确认。不这样的话就会「用户明明扫码点了登录，程序一直说没登录」。
			if authCooked && settleTries < 5 && time.Since(lastSettle) > 8*time.Second {
				settleTries++
				lastSettle = time.Now()
				if settleTries == 1 {
					c.lg.Log("检测到登录 cookie，但页面还停在登录态，切换私信页确认登录 ...")
				}
				if c.settleLogin(15 * time.Second) {
					c.lg.Log("检测到登录成功！会话已保存到 session/profile")
					return nil
				}
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
	return runOnce(c.page, c.cfg, dry, c.hist, c.lg, c.base, c.fu, false, false), nil
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
	if !c.cfg.AutoReply {
		return 0, errors.New("该账号已关闭自动回复（SetAutoReply(false) / Options.AutoReply=false）")
	}
	return replyToNamed(c.page, name, c.cfg, dry, c.hist, c.lg, c.base, c.fu), nil
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
		n := runOnce(c.page, c.cfg, dry, c.hist, c.lg, c.base, c.fu, false, false)
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

// Account 当前账号名（Manager 创建时填写；单账号直连时为空）
func (c *Client) Account() string { return c.opt.Account }

// SetAutoReply 运行时开关该账号的自动回复，下一次轮询生效。
// 关掉后：不再发送任何消息，但二次消息截图/观察照常。
func (c *Client) SetAutoReply(on bool) {
	c.cfg.AutoReply = on
	if c.lg != nil {
		if on {
			c.lg.Log("已开启自动回复")
		} else {
			c.lg.Log("已关闭自动回复（不再发送任何消息，二次消息照常截图）")
		}
	}
}

// FollowUps 返回「已自动回复、正在观察客户二次消息」的会话（最近的排前面）。
// 记录里带最近一次截图路径，可直接给前端/后台展示。
func (c *Client) FollowUps() []FollowUp { return c.fu.list() }

// Unwatch 把某个会话移出观察名单（比如你人工接手了这个客户），
// 之后它再来消息会重新按普通会话自动回复。返回是否真的移除了。
func (c *Client) Unwatch(name string) bool { return c.fu.remove(name) }
