package douyinkit

// 二维码抓取 —— 从登录页二维码元素（canvas 渲染，无 src）直接截图转 base64。
// 实测选择器：#douyin_login_comp_scan_code_qrcode（180x180），兜底 #animate_qrcode_container。
// 二维码过期时页面出现「点击刷新」，优先点刷新按钮，找不到就整页重载。

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

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

	// 1) 页面上已经有二维码 → 直接截
	if qr := c.grabQR(1500 * time.Millisecond); qr != nil {
		return qr, nil
	}

	// 2) 二维码过期 → 先点「点击刷新」
	if v, ok := evalRaw(c.page, refreshJS); ok && !v.Nil() && !v.Get("x").Nil() {
		mouseClick(c.page, v.Get("x").Num(), v.Get("y").Num())
		if qr := c.grabQR(6 * time.Second); qr != nil {
			return qr, nil
		}
	}

	// 3) 检测到登录 cookie：可能只是页面还没渲染完，先软重载（不动 cookie，避免误删有效登录态）
	if c.browser != nil && hasAuthCookie(c.browser) {
		c.lg.Log("页面没有二维码，但检测到登录 cookie，重载等待渲染 ...")
		_ = c.page.Navigate(chatURL)
		if qr := c.grabQR(25 * time.Second); qr != nil {
			return qr, nil
		}
	}

	// 4) 无登录态（或 cookie 已失效）→ 清 cookie 回游客态。
	//    典型故障：profile 里只剩 ttwid/odin_tt 这类匿名 cookie，前端路由以为已登录，
	//    停在私信页骨架屏，既不显示二维码也没有登录浮层，此时只能清干净重新来。
	c.resetGuestLocked()
	if qr := c.grabQR(30 * time.Second); qr != nil {
		return qr, nil
	}

	// 5) 兜底：整页再重载一次
	_ = c.page.Navigate(chatURL)
	if qr := c.grabQR(20 * time.Second); qr != nil {
		return qr, nil
	}

	saveShot(c.page, filepath.Join(c.base, "shots", "qr_fail.png"))
	return nil, fmt.Errorf("没找到二维码元素（页面可能不是登录页，已存截图 shots/qr_fail.png）")
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
			if err != nil || len(data) < 1200 { // 太小 = 空白占位图
				continue
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

// resetGuestLocked 把页面强制打回「游客态」：清 cookie + 前端 storage，再重载私信页。
// 实测（douyinkit-data 无登录态）：清完 3 秒内抖音会自动弹出登录框并渲染二维码。
func (c *Client) resetGuestLocked() {
	c.lg.Log("页面不是登录页（疑似残留登录态/骨架屏），清理 cookie 后重载 ...")
	_ = proto.NetworkClearBrowserCookies{}.Call(c.page)
	_, _ = c.page.Eval(`() => { try{localStorage.clear()}catch(e){} try{sessionStorage.clear()}catch(e){} return 1 }`)
	_ = c.page.Navigate(chatURL)
	wait(2500)
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
