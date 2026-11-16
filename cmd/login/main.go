// demo 1 —— 只登录：扫码登录成功后立即退出，不回复、不轮询消息。
//
//	本地弹窗扫码：  go run ./cmd/login
//	无头/服务器：    go run ./cmd/login -headless yes   （然后浏览器打开 qr.html 扫码）
//	多账号：        go run ./cmd/login -account 店铺A
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	douyinkit "github.com/Jason-cool/douyinkit"
	common "github.com/Jason-cool/douyinkit/cmd/internal/common"
)

func main() {
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opt := common.Options()
	opt.OnQR = func(qr *douyinkit.QRCode) {
		// 每次二维码刷新都把最新码写成 qr.html，浏览器打开即可扫码
		html := `<!doctype html><meta charset="utf-8"><title>抖音扫码登录</title>` +
			`<body style="background:#161823;text-align:center;padding-top:60px">` +
			`<img src="` + qr.DataURL + `" style="width:300px;height:300px;background:#fff;padding:14px;border-radius:12px">` +
			`<p style="color:#eee">用抖音 APP 左上角「扫一扫」</p>` +
			`<script>setTimeout(()=>location.reload(),3000)</script></body>`
		_ = os.WriteFile("qr.html", []byte(html), 0o644)
		fmt.Printf("【%s】二维码已更新 → 请用浏览器打开 qr.html 扫码\n", time.Now().Format("15:04:05"))
	}

	c, err := common.ClientWith(opt)
	if err != nil {
		fmt.Println("创建客户端失败：", err)
		os.Exit(1)
	}
	defer c.Close()

	if c.IsLoggedIn() {
		fmt.Println("✅ 已登录，无需扫码")
		return
	}

	fmt.Println("等待扫码（二维码每 30 秒自动刷新，Ctrl+C 取消）...")
	if err := c.EnsureLogin(ctx, nil); err != nil {
		fmt.Println("登录失败：", err)
		os.Exit(1)
	}
	fmt.Println("✅ 登录成功，登录态已保存，后续启动不需要再扫")
}
