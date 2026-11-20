package douyinkit

// 二维码抓取 —— 从登录页二维码元素（canvas 渲染，无 src）直接截图转 base64。
// 实测选择器：#douyin_login_comp_scan_code_qrcode（180x180），兜底 #animate_qrcode_container。
// 二维码过期时页面出现「点击刷新」，优先点刷新按钮，找不到就整页重载。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"path/filepath"
	"regexp"
	"time"
)

// errAuthPending 表示「已检测到登录 cookie，但页面上没有二维码」——
// 典型出现在刚扫码确认、页面正在跳转的瞬间。调用方必须按「正在登录」处理：
// 等页面就绪即可，绝不能当未登录去清 cookie（那样会把用户刚建立的登录态当场抹掉）。
var errAuthPending = errors.New("页面正在加载或确认登录（保留现有会话）")

var errVerificationRequired = errors.New("抖音要求额外身份验证，请由账号本人完成短信验证后继续")

// qrFetchReuse 同一个二维码的复用窗口。
// 多个入口并发抓码（/qr 轮询、EnsureLogin、用户重复点击）时，后来者会等锁；
// 等到的常常是「页面刚被前一次抓取重载过」的瞬间，扑空后走清 cookie 自愈，
// 于是把用户刚扫成功的登录态清掉。窗口内直接复用缓存，既不重复抓也不误清。
const qrFetchReuse = 15 * time.Second

// QRCode 一次抓取的登录二维码。前端直接用 DataURL 渲染：
//
//	<img src="data:image/png;base64,..." />
type QRCode struct {
	Base64     string    `json:"qr_base64"` // 纯 base64（PNG 字节）
	DataURL    string    `json:"data_url"`  // data:image/png;base64,...
	CapturedAt time.Time `json:"captured_at"`
}

const (
	qrSelPrimary  = `#douyin_login_comp_scan_code_qrcode`
	qrSelFallback = `#animate_qrcode_container`
)

var qrExpiredRe = regexp.MustCompile(`已失效|已过期|点击刷新`)

// refreshJS：二维码过期则返回刷新按钮坐标，否则返回 null
const refreshJS = `() => {
	const t = document.body ? document.body.innerText : '';
	if (!/已失效|已过期|点击刷新/.test(t)) return null;
	const els = document.querySelectorAll('div, button, span, a');
	for (const e of els) {
		const s = (e.innerText || '').trim();
		if (s && s.length <= 8 && /刷新/.test(s)) {
			const r = e.getBoundingClientRect();
			if (r.width > 0 && r.height > 0) {
				return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) };
			}
		}
	}
	return null;
}`

// FetchQR 抓取当前登录二维码（阻塞，约 1-5 秒）。未打开登录页时返回错误。
func (c *Client) FetchQR() (*QRCode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetchQRLocked()
}

func (c *Client) fetchQRLocked() (*QRCode, error) {
	return c.fetchQRContextLocked(context.Background())
}

func (c *Client) fetchQRContextLocked(ctx context.Context) (*QRCode, error) {
	if c.page == nil {
		return nil, fmt.Errorf("页面未连接，请先 Connect()")
	}
	state, err := c.observeLoginLocked(ctx)
	if err != nil {
		return nil, fmt.Errorf("检测登录页面失败：%w", err)
	}
	if state.ready() {
		return nil, errAuthPending
	}
	if state.VerificationRequired || !state.QRVisible {
		return nil, c.handleLoginPendingLocked(ctx, state)
	}
	if qr, ok := c.LatestQR(); ok && time.Since(qr.CapturedAt) < qrFetchReuse {
		return qr, nil
	}
	c.setLoginProgress("fetching", nil)
	qr, captureErr := c.grabQR(ctx, 15*time.Second)
	if qr != nil {
		return c.cacheQR(qr), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pageCtx, cancel := context.WithTimeout(ctx, loginProbeTimeout)
	v, evalErr := c.page.Context(pageCtx).Eval(refreshJS)
	cancel()
	if evalErr == nil && !v.Value.Nil() && !v.Value.Get("x").Nil() {
		// DOM event fallback also works when CDP mouse events are not accepted.
		clickCtx, clickCancel := context.WithTimeout(ctx, loginProbeTimeout)
		_, _ = c.page.Context(clickCtx).Eval(JS_POINTER_CLICK_JS, v.Value.Get("x").Num(), v.Value.Get("y").Num())
		clickCancel()
		if qr, err := c.grabQR(ctx, 8*time.Second); qr != nil {
			return c.cacheQR(qr), nil
		} else {
			captureErr = err
		}
	}
	// Recheck after capture: the QR may have disappeared because scanning has
	// completed. An incomplete transition must never erase browser storage.
	state, err = c.observeLoginLocked(ctx)
	if err != nil {
		return nil, err
	}
	if state.ready() {
		return nil, errAuthPending
	}
	if state.VerificationRequired || !state.QRVisible {
		return nil, c.handleLoginPendingLocked(ctx, state)
	}
	// A visible QR with an existing Cookie still needs to be delivered. Failure
	// here is a capture error, not proof that the user is completing login.
	path := filepath.Join(c.base, "shots", "qr_fail.png")
	saveShot(c.page.Context(ctx), path)
	err = fmt.Errorf("二维码元素可见但图片提取失败（保留 Cookie）：%w；页面截图 %s", captureErr, path)
	c.setLoginProgress("error", err)
	c.lg.Log("! %v", err)
	return nil, err
}

// cacheQR must be called with c.mu held. Snapshot reads use a separate lock.
func (c *Client) cacheQR(qr *QRCode) *QRCode {
	c.clearVerificationLocked()
	c.setLastQR(qr)
	c.setLoginProgress("waiting_scan", nil)
	return qr
}

// grabQR 在 timeout 内轮询二维码元素，出现且可见就截图返回；超时返回 nil。
// 轮询而不是 Element 阻塞等待，是因为登录页渲染慢时元素会先出现后填图。
func (c *Client) grabQR(ctx context.Context, timeout time.Duration) (*QRCode, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for ctx.Err() == nil {
		for _, sel := range []string{qrSelPrimary, qrSelFallback} {
			if ctx.Err() != nil {
				break
			}
			qr, err := c.captureQR(ctx, sel)
			if err == nil {
				return qr, nil
			}
			// A final fallback lookup timing out must not erase the useful image
			// validation/capture error already observed on the primary element.
			deadlineError := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
			if lastErr == nil || (!errors.Is(err, errQRNotFound) && !deadlineError) || errors.Is(lastErr, errQRNotFound) {
				lastErr = err
			}
		}
		if waitContext(ctx, 400*time.Millisecond) != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, fmt.Errorf("二维码提取未完成：%w", lastErr)
}

// looksLikeQR 判断截图是不是「画完了」的二维码。
// 抖音的二维码是 canvas 异步绘制的：先出白色圆角底板 + 抖音 logo，几百毫秒后才填上黑格子。
// 只判断字节数会把「只有 logo 的半成品」当成二维码发给前端，用户扫半天没反应。
// 这里解码 PNG 统计深色像素占比：真二维码约 25%~55%，只有 logo 时不足 8%。
func looksLikeQR(data []byte) bool { return validateQRImage(data) == nil }

func validateQRImage(data []byte) error {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("二维码 PNG 解码失败：%w", err)
	}
	b := img.Bounds()
	if b.Dx() < 80 || b.Dy() < 80 {
		return fmt.Errorf("图片尺寸过小：%dx%d", b.Dx(), b.Dy())
	}
	var total, dark int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			// Composite transparency onto white before counting dark pixels.
			r, g, bl = r+0xffff-a, g+0xffff-a, bl+0xffff-a
			if r>>8 < 110 && g>>8 < 110 && bl>>8 < 110 {
				dark++
			}
			total++
		}
	}
	if total == 0 {
		return fmt.Errorf("二维码图片为空")
	}
	ratio := float64(dark) / float64(total)
	if ratio <= 0.12 || ratio >= 0.75 {
		return fmt.Errorf("图片不像完整二维码：%dx%d，深色像素 %.1f%%", b.Dx(), b.Dy(), ratio*100)
	}
	return nil
}

// settleLogin uses the same readiness test as Run/QRStatus. A positional list
// match is never sufficient, and a slow page never causes Cookie deletion.
func (c *Client) settleLogin(ctx context.Context, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	navigated := false
	for ctx.Err() == nil {
		if c.lockContext(ctx) != nil {
			return false
		}
		state, err := c.observeLoginLocked(ctx)
		if err == nil && state.ready() {
			c.mu.Unlock()
			return true
		}
		if err == nil && state.VerificationRequired {
			c.mu.Unlock()
			return false
		}
		if err == nil && !state.Authenticated {
			c.mu.Unlock()
			return false
		}
		if err == nil && !navigated {
			navigated = true
			_ = c.navigateLocked(ctx)
		}
		c.mu.Unlock()
		if waitContext(ctx, 500*time.Millisecond) != nil {
			return false
		}
	}
	return false
}

// LatestQR reads only a snapshot. It does not acquire the browser lock or call CDP.
// Expired QR codes and QR codes from a confirmed login are not returned.
func (c *Client) LatestQR() (*QRCode, bool) {
	if c.loggedIn.Load() {
		return nil, false
	}
	c.snapshotMu.RLock()
	defer c.snapshotMu.RUnlock()
	if c.loginStage == "verification_required" || c.loginStage == "logging_in" || c.lastQR == nil || time.Since(c.lastQR.CapturedAt) >= qrCacheTTL {
		return nil, false
	}
	qr := *c.lastQR
	return &qr, true
}

func (c *Client) setLastQR(qr *QRCode) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	if qr == nil {
		c.lastQR = nil
		return
	}
	copy := *qr
	c.lastQR = &copy
}

// ---------- 一站式登录状态：等价于演示程序的 GET /qr，但不需要起 HTTP 接口 ----------

// LoginStatus 一次登录状态查询的结果，字段与 GET /qr 的 JSON 完全一致：
// 可以直接 json.Marshal 返回给前端，也可以取 DataURL 塞进 <img src>。
type LoginStatus struct {
	LoggedIn   bool       `json:"logged_in"`
	QRBase64   string     `json:"qr_base64,omitempty"` // 纯 base64（PNG 字节）
	DataURL    string     `json:"data_url,omitempty"`  // data:image/png;base64,...
	CapturedAt *time.Time `json:"captured_at,omitempty"`

	// 浏览器正忙着（正在启动 / 正在抓码 / 正在确认登录态），本次没拿到二维码。
	// 前端据此显示「正在准备中，请稍候」，而不是把空状态当成故障一直问。
	Busy         bool             `json:"busy,omitempty"`
	Stage        string           `json:"stage,omitempty"` // booting / fetching / logging_in / waiting_scan / verification_required / error
	Error        string           `json:"error,omitempty"` // latest preparation/capture error, cleared after success
	Verification *SMSVerification `json:"verification,omitempty"`
}

// CachedLoginStatus never calls CDP or acquires the browser operation lock.
func (c *Client) CachedLoginStatus() *LoginStatus {
	c.snapshotMu.RLock()
	defer c.snapshotMu.RUnlock()
	if c.loggedIn.Load() {
		return &LoginStatus{LoggedIn: true}
	}
	st := &LoginStatus{Stage: c.loginStage, Error: c.loginError, Verification: cloneVerification(c.verification)}
	if st.Stage == "verification_required" {
		if st.Verification != nil {
			if st.Verification.Step == "code_sent" || st.Verification.Step == "verifying" {
				st.Error = st.Verification.Error
			}
			if st.Verification.Step == "verifying" {
				st.Busy = true
			}
		}
		return st
	}
	if st.Stage == "logging_in" {
		st.Busy = true
		return st
	}
	if c.lastQR != nil && time.Since(c.lastQR.CapturedAt) < qrCacheTTL {
		qr := statusFromQR(c.lastQR)
		qr.Stage = "waiting_scan"
		return qr
	}
	if st.Stage == "" || st.Stage == "waiting_scan" {
		st.Stage = "fetching"
	}
	st.Busy = st.Stage != "error"
	return st
}

func (c *Client) setLoginProgress(stage string, err error) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	previous := c.loginStage
	c.loginStage = stage
	if err != nil {
		c.loginError = err.Error()
	} else if stage == "" || stage == "waiting_scan" || stage == "booting" || previous == "verification_required" {
		c.loginError = ""
	}
}

// Text 可直接展示的中文状态
func (s LoginStatus) Text() string {
	switch {
	case s.LoggedIn:
		return "已登录"
	case s.Stage == "verification_required":
		return "抖音要求身份验证，请由账号本人完成短信验证"
	case s.Stage == "logging_in":
		return "已检测到登录状态，正在确认（请稍候，勿关闭页面）"
	case s.Stage == "reset":
		return "登录态失效，正在重新获取二维码"
	case s.Busy || s.Stage != "":
		return "程序正在准备浏览器和二维码，请稍候"
	}
	return "未登录，请用抖音 App 扫码"
}

// qrCacheTTL 二维码缓存有效期：抖音二维码约 2 分钟失效，提前到 90 秒重抓
const qrCacheTTL = 90 * time.Second

// QRStatus 一次拿到「是否已登录 / 未登录时的二维码」，不需要自己起 HTTP 接口：
//   - 已登录：返回 LoggedIn=true，不带二维码；
//   - 未登录：返回可直接渲染的二维码（DataURL），必要时现场抓取（含骨架屏自愈）；
//   - 浏览器没连接会自动 Connect（首次调用会多花十来秒启动）。
//
// 导包后直接调用：
//
//	st, err := c.QRStatus()
//	if err != nil {
//	    log.Println("取二维码失败：", err)
//	} else if st.LoggedIn {
//	    fmt.Println(st.Text())   // 已登录
//	} else {
//	    fmt.Println(st.DataURL)  // data:image/png;base64,... → 前端 <img src>
//	}
//
// 建议前端每 3 秒调一次：未登录期间二维码换新会自动返回新码，扫码成功后 LoggedIn 变 true。
// 若另一个 goroutine 正在抓码（例如 EnsureLogin 正在跑），本次会立即返回空状态
// （LoggedIn=false、无二维码），下次轮询即可拿到，不会阻塞。
func (c *Client) QRStatus() (*LoginStatus, error) {
	return c.qrStatusContext(context.Background())
}

func (c *Client) qrStatusContext(ctx context.Context) (*LoginStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.mu.TryLock() {
		return c.CachedLoginStatus(), nil
	}
	defer c.mu.Unlock()
	if err := c.connectWithBackoffContextLocked(ctx); err != nil {
		return nil, err
	}
	state, err := c.observeLoginLocked(ctx)
	if err != nil {
		return &LoginStatus{Busy: true, Stage: "logging_in"}, nil
	}
	if state.ready() {
		return &LoginStatus{LoggedIn: true}, nil
	}
	if state.VerificationRequired || !state.QRVisible {
		err := c.handleLoginPendingLocked(ctx, state)
		if err != nil && !errors.Is(err, errAuthPending) && !errors.Is(err, errVerificationRequired) {
			return nil, err
		}
		return c.CachedLoginStatus(), nil
	}
	if qr, ok := c.LatestQR(); ok {
		return statusFromQR(qr), nil
	}
	qr, err := c.fetchQRContextLocked(ctx)
	if errors.Is(err, errAuthPending) || errors.Is(err, errVerificationRequired) {
		return c.CachedLoginStatus(), nil
	}
	if err != nil {
		return nil, err
	}
	return statusFromQR(qr), nil
}

func statusFromQR(qr *QRCode) *LoginStatus {
	t := qr.CapturedAt
	return &LoginStatus{
		QRBase64:   qr.Base64,
		DataURL:    qr.DataURL,
		CapturedAt: &t,
	}
}
