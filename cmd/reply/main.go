// demo 3 —— 只跑自动回复：登录（必要时自动出码）后按配置回复，别的不做。
//
//	一轮干跑（不发消息，只看会扫到谁）：  go run ./cmd/reply
//	一轮真发：                           go run ./cmd/reply -send
//	持续轮询真发：                       go run ./cmd/reply -send -loop
//	定向回复某昵称（测试发送链路）：      go run ./cmd/reply -send -to 某昵称
//	临时关掉该账号自动回复（只截图观察）：go run ./cmd/reply -send -loop -no-reply
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	douyinkit "github.com/Jason-cool/douyinkit"
	common "github.com/Jason-cool/douyinkit/cmd/internal/common"
)

func main() {
	send := flag.Bool("send", false, "真正发送（默认干跑）")
	loop := flag.Bool("loop", false, "持续轮询，不退出")
	to := flag.String("to", "", "只回复指定昵称的会话（需配合 -send 才真发）")
	noReply := flag.Bool("no-reply", false, "关闭该账号的自动回复（只做二次消息截图/观察）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opt := common.Options()
	if *noReply {
		off := false
		opt.AutoReply = &off
	}
	opt.OnReply = func(name, result string, sent bool) {
		fmt.Printf("【回调】%s → %s（已发送=%v）\n", name, result, sent)
	}
	opt.OnFollowUp = func(f douyinkit.FollowUp) {
		fmt.Printf("【二次消息】%s 又发来消息 %q → 截图 %s\n", f.Name, f.LastText, f.LastPath)
	}

	c, err := common.ClientWith(opt)
	if err != nil {
		fmt.Println("创建客户端失败：", err)
		os.Exit(1)
	}
	defer c.Close()

	// 未登录则先走扫码（复用 login demo 的 OnQR 由 Options 传入；这里没有 HTTP 页面，
	// 需要扫码时用 cmd/login 或 cmd/demo 更方便）
	if !c.IsLoggedIn() {
		fmt.Println("该账号未登录：请先 go run ./cmd/login 扫码，或直接运行 cmd/demo（带 /qr 页面）")
		os.Exit(2)
	}

	if *to != "" {
		n, err := c.SendTo(*to, !*send)
		fmt.Printf("定向回复 %q：发出 %d 条（err=%v）\n", *to, n, err)
		return
	}

	if *loop {
		fmt.Println("进入持续轮询（Ctrl+C 退出）...")
		if err := c.Run(ctx, !*send); err != nil {
			fmt.Println("轮询结束：", err)
		}
		return
	}

	n, err := c.RunOnce(!*send)
	if err != nil {
		fmt.Println("执行失败：", err)
		os.Exit(1)
	}
	fmt.Printf("本轮结束，共发送 %d 条\n", n)
}
