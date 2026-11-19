package douyinkit

// 二维码抓取 —— 从登录页二维码元素（canvas 渲染，无 src）直接截图转 base64。
// 实测选择器：#douyin_login_comp_scan_code_qrcode（180x180），兜底 #animate_qrcode_container。
// 二维码过期时页面出现「点击刷新」，优先点刷新按钮，找不到就整页重载。

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"path/filepath"
	"regexp"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// errAuthPending 表示「已检测到登录 cookie，但页面上没有二维码」——
// 典型出现在刚扫码确认、页面正在跳转的瞬间。调用方必须按「正在登录」处理：
// 等页面就绪即可，绝不能当未登录去清 cookie（那样会把用户刚建立的登录态当场抹掉）。
var errAuthPending = errors.New("检测到登录 cookie，页面正在切换（保留 cookie）")

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
	if c.page == nil {
		return nil, fmt.Errorf("页面未连接，请先 Connect()")
	}

	// 0) 刚抓过就复用：并发入口（/qr 轮询 + EnsureLogin + 重复点击）不再重复抓。
	//    否则第二次抓取时页面可能正在重载，扑空后会走「清 cookie 自愈」，
	//    把用户刚扫码取得的登录态一起清掉（服务器上最常见的「扫码后没登录」）。
	if c.lastQR != nil && time.Since(c.lastQR.CapturedAt) < qrFetchReuse && !c.loggedInLocked() {
		c.lg.Log("复用 %.0f 秒前的登录二维码（避免并发重复抓取）", time.Since(c.lastQR.CapturedAt).Seconds())
		return c.lastQR, nil
	}

	// 1) 页面上已经有二维码 → 直接截
	if qr := c.grabQR(1500 * time.Millisecond); qr != nil {
		return c.cacheQR(qr), nil
	}

	// 2) 二维码过期 → 先点「点击刷新」
	if v, ok := evalRaw(c.page, refreshJS); ok && !v.Nil() && !v.Get("x").Nil() {
		mouseClick(c.page, v.Get("x").Num(), v.Get("y").Num())
		if qr := c.grabQR(6 * time.Second); qr != nil {
			return c.cacheQR(qr), nil
		}
	}

	// 3) 检测到登录 cookie：可能只是页面还没渲染完，先软重载（不动 cookie，避免误删有效登录态）
	if c.browser != nil && hasAuthCookie(c.browser) {
		c.lg.Log("页面没有二维码，但检测到登录 cookie，重载等待渲染 ...")
		_ = c.page.Navigate(chatURL)
		if qr := c.grabQR(25 * time.Second); qr != nil {
			return c.cacheQR(qr), nil
		}
		// 25 秒都没有二维码，但已经不在登录页（登录浮层消失/进了私信页）：
		// 说明登录其实成功了，只是这个页面本来就不该有二维码 —— 按「正在登录」返回。
		if c.loggedInLocked() {
			c.lg.Log("登录态已生效（页面已进入私信页），保留 cookie，不再抓码")
			return nil, errAuthPending
		}
	}

	// 4) 清 cookie 前最后一道闸：只要还有登录 cookie 就绝不清。
	//    清 cookie 是不可逆的：用户刚扫码、页面还没跳完时清掉，登录结果当场作废，
	//    之后无论怎么轮询都拿不到登录态（服务器上「扫码点了登录却没登录」的主因）。
	if c.browser != nil && hasAuthCookie(c.browser) {
		c.lg.Log("仍有登录 cookie：按「正在登录」处理，保留 cookie（不清）")
		return nil, errAuthPending
	}

	// 5) 无登录态（或 cookie 已失效）→ 清 cookie 回游客态。
	//    典型故障：profile 里只剩 ttwid/odin_tt 这类匿名 cookie，前端路由以为已登录，
	//    停在私信页骨架屏，既不显示二维码也没有登录浮层，此时只能清干净重新来。
	c.resetGuestLocked()
	if qr := c.grabQR(30 * time.Second); qr != nil {
		return c.cacheQR(qr), nil
	}

	// 6) 兜底：整页再重载一次
	_ = c.page.Navigate(chatURL)
	if qr := c.grabQR(20 * time.Second); qr != nil {
		return c.cacheQR(qr), nil
	}

	saveShot(c.page, filepath.Join(c.base, "shots", "qr_fail.png"))
	return nil, fmt.Errorf("没找到二维码元素（页面可能不是登录页，已存截图 shots/qr_fail.png）")
}

// cacheQR 记录本轮抓到的二维码，供并发入口复用 + LatestQR 查询。
// 调用方必须已持有 c.mu（fetchQRLocked / QRStatus 内部）。
func (c *Client) cacheQR(qr *QRCode) *QRCode {
	c.lastQR = qr
	return qr
}

// grabQR 在 timeout 内轮询二维码元素，出现且可见就截图返回；超时返回 nil。
// 轮询而不是 Element 阻塞等待，是因为登录页渲染慢时元素会先出现后填图。
func (c *Client) grabQR(timeout time.Duration) *QRCode {
	deadline := time.Now().Add(timeout)
	for {
		for _, sel := range []string{qrSelPrimary, qrSelFallback} {
			el, err := c.page.Timeout(1200 * time.Millisecond).Element(sel)
			if err != nil || el == nil {
				continue
			}
			if ok, e := el.Visible(); e != nil || !ok {
				continue
			}
			data, err := el.Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
			if err != nil || !looksLikeQR(data) {
				continue // 空白 / 只画了 logo 的半成品，继续等它渲染完
			}
			b64 := base64.StdEncoding.EncodeToString(data)
			return &QRCode{
				Base64:     b64,
				DataURL:    "data:image/png;base64," + b64,
				CapturedAt: time.Now(),
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		wait(600)
	}
}

// looksLikeQR 判断截图是不是「画完了」的二维码。
// 抖音的二维码是 canvas 异步绘制的：先出白色圆角底板 + 抖音 logo，几百毫秒后才填上黑格子。
// 只判断字节数会把「只有 logo 的半成品」当成二维码发给前端，用户扫半天没反应。
// 这里解码 PNG 统计深色像素占比：真二维码约 25%~55%，只有 logo 时不足 8%。
func looksLikeQR(data []byte) bool {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return false
	}
	b := img.Bounds()
	if b.Dx() < 80 || b.Dy() < 80 {
		return false
	}
	var total, dark int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 < 110 && g>>8 < 110 && bl>>8 < 110 {
				dark++
			}
			total++
		}
	}
	if total == 0 {
		return false
	}
	ratio := float64(dark) / float64(total)
	return ratio > 0.12 && ratio < 0.75
}

// resetGuestLocked 把页面强制打回「游客态」：清 cookie + 前端 storage，再重载私信页。
// 实测（douyinkit-data 无登录态）：清完 3 秒内抖音会自动弹出登录框并渲染二维码。
func (c *Client) resetGuestLocked() {
	c.lg.Log("页面不是登录页（疑似残留登录态/骨架屏），清理 cookie 后重载 ...")
	_ = proto.NetworkClearBrowserCookies{}.Call(c.page)
	_, _ = c.page.Eval(`() => { try{localStorage.clear()}catch(e){} try{sessionStorage.clear()}catch(e){} return 1 }`)
	_ = c.page.Navigate(chatURL)
	wait(2500)
}

// settleLogin 检测到登录 cookie 后，等页面真正进入私信页（切页 + 等会话列表）。
// 返回 true 表示登录已生效。全程不动 cookie —— 与「清 cookie 自愈」相反，
// 专门用于「用户已经扫码点了登录，但页面还在跳转/浮层没消失」的收尾。
func (c *Client) settleLogin(timeout time.Duration) bool {
	c.mu.Lock()
	page := c.page
	c.mu.Unlock()
	if page == nil {
		return false
	}
	_ = page.Navigate(chatURL)
	wait(2000)
	if c.waitForConvs(timeout) {
		return true
	}
	c.mu.Lock()
	ok := c.loggedInLocked()
	c.mu.Unlock()
	return ok
}

// forceResetGuest 主动清 cookie 回游客态并清掉二维码缓存（仅在确认登录 cookie 已失效时用）。
func (c *Client) forceResetGuest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastQR = nil
	if c.page != nil {
		c.resetGuestLocked()
	}
}

// LatestQR 返回最近一次抓取的二维码（没有则返回 nil, false）
func (c *Client) LatestQR() (*QRCode, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastQR == nil {
		return nil, false
	}
	return c.lastQR, true
}

func (c *Client) setLastQR(qr *QRCode) {
	c.mu.Lock()
	c.lastQR = qr
	c.mu.Unlock()
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
	Busy  bool   `json:"busy,omitempty"`
	Stage string `json:"stage,omitempty"` // booting / fetching / logging_in / reset
}

// Text 可直接展示的中文状态
func (s LoginStatus) Text() string {
	switch {
	case s.LoggedIn:
		return "已登录"
	case s.Stage == "logging_in":
		return "已检测到登录状态，正在确认（请稍候，勿关闭页面）"
	case s.Stage == "reset":
		return "登录态失效，正在重新获取二维码"
	case s.Busy || s.Stage != "":
		return "程序正在启动浏览器并获取二维码（首次约 20~30 秒），请稍候"
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
	if !c.mu.TryLock() {
		// 浏览器正被别的流程占用（首次启动 / EnsureLogin 正在抓码 / 正在处理会话）。
		// 明确告诉前端「在忙」，否则前端只会无限显示「正在获取二维码…」。
		return &LoginStatus{Busy: true, Stage: "fetching"}, nil
	}
	defer c.mu.Unlock()

	if err := c.connectWithBackoffLocked(); err != nil {
		return nil, err
	}
	if c.loggedInLocked() {
		c.authPendingAt = time.Time{}
		return &LoginStatus{LoggedIn: true}, nil
	}
	// 未登录：90 秒内的缓存直接复用，避免每次轮询都重新截图
	if c.lastQR != nil && time.Since(c.lastQR.CapturedAt) < qrCacheTTL {
		return statusFromQR(c.lastQR), nil
	}
	qr, err := c.fetchQRLocked()
	if err != nil {
		if errors.Is(err, errAuthPending) {
			// 正在登录（cookie 已到、页面还在跳转）：给前端一个「确认中」状态，下次轮询再问，
			// 不要报错 —— 报错会让前端弹「获取二维码失败」，而实际上用户已经扫成功了。
			// 但如果是「过期 sessionid 赖着不走」，这里会永远等不到登录页 —— 超过 90 秒就清干净重出码。
			if c.authPendingAt.IsZero() {
				c.authPendingAt = time.Now()
				return &LoginStatus{Busy: true, Stage: "logging_in"}, nil
			}
			if time.Since(c.authPendingAt) > 90*time.Second {
				c.lg.Log("登录 cookie 长期无法进入私信页（>90 秒，疑似已失效），清除后重新出码 ...")
				c.authPendingAt = time.Time{}
				c.lastQR = nil
				c.resetGuestLocked()
				return &LoginStatus{Busy: true, Stage: "reset"}, nil
			}
			return &LoginStatus{Busy: true, Stage: "logging_in"}, nil
		}
		if c.lastQR != nil { // 抓不到新码就退回旧码，前端至少还有图可显示
			return statusFromQR(c.lastQR), nil
		}
		return nil, err
	}
	c.authPendingAt = time.Time{}
	c.lastQR = qr
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
