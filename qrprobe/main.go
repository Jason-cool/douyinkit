// 二维码探测程序：用一次性临时 profile 打开 douyin.com/chat 登录页，
// dump 二维码候选元素（img/canvas/qr 相关 class），并观察二维码是否自动刷新。
// 输出写到 qrprobe_out.txt，整页截图存 probe_qr.png。
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

const chatURL = "https://www.douyin.com/chat"

func wait(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func main() {
	f, _ := os.Create("qrprobe_out.txt")
	defer f.Close()
	log := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Println(line)
		fmt.Fprintln(f, line)
	}

	l := launcher.New().Headless(false).Leakless(false)
	for _, p := range []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			l = l.Bin(p)
			break
		}
	}
	l = l.Set(flags.UserDataDir, "qrprobe_profile").
		Set("window-size", "1200,900").
		Set("disable-blink-features", "AutomationControlled")
	u, err := l.Launch()
	if err != nil {
		log("launch err: %v", err)
		os.Exit(1)
	}
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		log("connect err: %v", err)
		os.Exit(1)
	}
	defer b.Close()
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		log("page err: %v", err)
		os.Exit(1)
	}
	if err := page.Navigate(chatURL); err != nil {
		log("nav err: %v", err)
		os.Exit(1)
	}
	wait(5000)

	candidatesJS := `() => {
		const out = [];
		const els = document.querySelectorAll('img, canvas, [class*="qr" i], [id*="qr" i]');
		for (const e of els) {
			const r = e.getBoundingClientRect();
			if (r.width < 60 || r.height < 60) continue;
			let cls = '';
			if (typeof e.className === 'string') cls = e.className;
			else if (e.className && e.className.baseVal !== undefined) cls = e.className.baseVal;
			out.push({
				tag: e.tagName,
				cls: cls.slice(0, 140),
				id: e.id || '',
				x: Math.round(r.x), y: Math.round(r.y),
				w: Math.round(r.width), h: Math.round(r.height),
				src: (e.src || e.getAttribute('src') || '').slice(0, 100),
			});
		}
		const body = document.body ? document.body.innerText.slice(0, 400) : '';
		return { n: out.length, list: out, body };
	}`

	var lastSrc string
	for i := 0; i < 20; i++ {
		res, err := page.Eval(candidatesJS)
		if err == nil && res != nil {
			log("---- 第 %d 次采样 (%s) ----", i+1, time.Now().Format("15:04:05"))
			log("%s", res.Value.String())
			if s := res.Value.Get("list").Get("#0").Get("src").String(); s != "" {
				lastSrc = s
			}
			// 找到候选就再采第二次看刷新
			if res.Value.Get("n").Int() > 0 && i >= 2 {
				break
			}
		} else {
			log("eval err: %v", err)
		}
		wait(2000)
	}

	// 观察 25 秒看二维码是否自动刷新
	log("==== 观察 25 秒检查二维码刷新 ====")
	for i := 0; i < 5; i++ {
		wait(5000)
		res, err := page.Eval(candidatesJS)
		if err == nil && res != nil {
			src := res.Value.Get("list").Get("#0").Get("src").String()
			log("t+%ds src变了吗: %v  src前缀: %s", (i+1)*5, src != lastSrc, src)
		}
	}

	if data, err := page.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng}); err == nil {
		_ = os.WriteFile("probe_qr.png", data, 0o644)
		log("截图已存 probe_qr.png")
	}
	log("URL: %s", func() string {
		if r, e := page.Eval("() => location.href"); e == nil {
			return r.Value.String()
		}
		return "?"
	}())
}
