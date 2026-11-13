package douyinkit

// 二维码抓取 —— 从登录页二维码元素（canvas 渲染，无 src）直接截图转 base64。
// 实测选择器：#douyin_login_comp_scan_code_qrcode（180x180），兜底 #animate_qrcode_container。
// 二维码过期时页面出现「点击刷新」，优先点刷新按钮，找不到就整页重载。

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// QRCode 一次抓取的登录二维码。前端直接用 DataURL 渲染：
//
//	<img src="data:image/png;base64,..." />
type QRCode struct {
	Base64     string    `json:"qr_base64"`  // 纯 base64（PNG 字节）
	DataURL    string    `json:"data_url"`   // data:image/png;base64,...
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
	// 1) 过期处理：页面出现「点击刷新」时，优先点刷新按钮；按钮也找不到则检查二维码元素，
	//    元素也没了就整页重载
	needReload := true
	if v, ok := evalRaw(c.page, refreshJS); ok && v != nil && v.Get("x").Int() != 0 {
		mouseClick(c.page, v.Get("x").Num(), v.Get("y").Num())
		wait(2500)
		needReload = false
	}
	if needReload {
		if _, e1 := c.page.Timeout(3 * time.Second).Element(qrSelPrimary); e1 != nil {
			if _, e2 := c.page.Timeout(2 * time.Second).Element(qrSelFallback); e2 != nil {
				_ = c.page.Navigate(chatURL)
				wait(5000)
			}
		}
	}

	// 2) 定位二维码元素并截图
	var data []byte
	var err error
	for _, sel := range []string{qrSelPrimary, qrSelFallback} {
		el, e := c.page.Timeout(5 * time.Second).Element(sel)
		if e != nil || el == nil {
			err = e
			continue
		}
		data, err = el.Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
		if err == nil && len(data) > 0 {
			break
		}
	}
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("没找到二维码元素（页面可能不是登录页）：%v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	return &QRCode{
		Base64:     b64,
		DataURL:    "data:image/png;base64," + b64,
		CapturedAt: time.Now(),
	}, nil
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
