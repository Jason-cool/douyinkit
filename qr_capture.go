package douyinkit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

var errQRNotFound = errors.New("未找到二维码元素")

// Read rendered pixels without scrolling or capturing the whole viewport.
// Cross-origin images/tainted canvases fall back to a browser screenshot.
const qrImageDataJS = `() => {
  const nodes = [this, ...this.querySelectorAll('canvas,img')];
  const out = [];
  for (const el of nodes) {
    try {
      if (el.tagName === 'CANVAS' && el.width >= 80 && el.height >= 80) {
        out.push(el.toDataURL('image/png'));
      } else if (el.tagName === 'IMG' && el.complete && el.naturalWidth >= 80 && el.naturalHeight >= 80) {
        const canvas = document.createElement('canvas');
        canvas.width = el.naturalWidth; canvas.height = el.naturalHeight;
        canvas.getContext('2d').drawImage(el, 0, 0);
        out.push(canvas.toDataURL('image/png'));
      }
    } catch (_) { /* screenshot fallback below */ }
  }
  return out;
}`

func (c *Client) captureQR(ctx context.Context, selector string) (*QRCode, error) {
	started := time.Now()
	lookupCtx, lookupCancel := context.WithTimeout(ctx, 2*time.Second)
	elements, err := c.page.Context(lookupCtx).Elements(selector)
	lookupCancel()
	if err != nil {
		return nil, fmt.Errorf("定位 %s 失败：%w", selector, err)
	}
	if len(elements) == 0 {
		return nil, fmt.Errorf("%s：%w", selector, errQRNotFound)
	}
	var issues []string
	for _, element := range elements {
		captureCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		el := element.Context(captureCtx)
		visible, err := el.Visible()
		if err != nil || !visible {
			issues = append(issues, fmt.Sprintf("元素不可见或检测失败：%v", err))
			cancel()
			continue
		}
		var dataURLs []string
		result, err := el.Eval(qrImageDataJS)
		if err == nil {
			err = result.Value.Unmarshal(&dataURLs)
		}
		if err != nil {
			issues = append(issues, fmt.Sprintf("直接读取 canvas/img 失败：%v", err))
		}
		for _, dataURL := range dataURLs {
			_, encoded, ok := strings.Cut(dataURL, ",")
			if !ok {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				err = validateQRImage(data)
			}
			if err == nil {
				cancel()
				c.lg.Log("二维码提取成功（canvas/img，%v）", time.Since(started).Round(time.Millisecond))
				return qrFromPNG(data), nil
			}
			issues = append(issues, "canvas/img："+err.Error())
		}
		// This has its own budget. Previously lookup + visibility + a whole-page
		// screenshot/crop all shared 1.2s, which is too tight on a loaded server.
		cancel()
		shotCtx, shotCancel := context.WithTimeout(ctx, 8*time.Second)
		data, err := element.Context(shotCtx).Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
		shotCancel()
		if err == nil {
			err = validateQRImage(data)
		}
		if err == nil {
			c.lg.Log("二维码提取成功（元素截图，%v）", time.Since(started).Round(time.Millisecond))
			return qrFromPNG(data), nil
		}
		issues = append(issues, "元素截图："+err.Error())
	}
	return nil, fmt.Errorf("%s：%s", selector, strings.Join(issues, "；"))
}

func qrFromPNG(data []byte) *QRCode {
	b64 := base64.StdEncoding.EncodeToString(data)
	return &QRCode{Base64: b64, DataURL: "data:image/png;base64," + b64, CapturedAt: time.Now()}
}
