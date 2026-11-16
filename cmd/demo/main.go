// douyinkit 演示程序 —— 展示库的完整用法：
//  1. 启动 HTTP 服务（默认 :6060）：GET /qr 返回 JSON（qr_base64/data_url/logged_in），
//     GET / 返回一个自动刷新的二维码渲染页（前端接入示例）
//  2. EnsureLogin：未登录则抓二维码（通过 OnQR 回调打印提示），扫码成功后继续
//  3. 默认干跑一轮；-send -loop 则进入持续自动回复
//
// 作为命令行运行：
//
//	go run ./cmd/demo                  # 干跑一轮
//	go run ./cmd/demo -send -loop      # 持续自动回复
//	go run ./cmd/demo -base /data/dy   # 指定数据目录
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	douyinkit "douyinkit"
)

func main() {
	base := flag.String("base", "./douyinkit-data", "数据目录（profile/logs/state）")
	addr := flag.String("addr", ":6060", "二维码 HTTP 服务监听地址（留空则不启动）")
	send := flag.Bool("send", false, "真正发送（默认干跑）")
	loop := flag.Bool("loop", false, "持续轮询，不退出")
	channel := flag.String("channel", "auto", "auto/chrome/chromium/msedge/bundled")
	to := flag.String("to", "", "只处理指定昵称的会话（测试发送链路用，配合 -send 才真发）")
	noHeadless := flag.Bool("no-headless", false, "弹浏览器窗口（本地调试用；Linux 服务器默认无头）")
	forceHeadless := flag.Bool("headless", false, "强制无头模式（在 Windows 上模拟 Linux 服务器环境）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var headless *bool
	switch {
	case *forceHeadless:
		f := true
		headless = &f
	case *noHeadless:
		f := false
		headless = &f
	}

	c, err := douyinkit.New(douyinkit.Options{
		BaseDir:  *base,
		Channel:  *channel,
		Headless: headless,
		OnReply: func(name, result string, sent bool) {
			fmt.Printf("【回调】会话=%s 结果=%s 已发送=%v\n", name, result, sent)
		},
	})
	if err != nil {
		fmt.Println("创建客户端失败：", err)
		os.Exit(1)
	}
	defer c.Close()

	// ---- 1) HTTP 服务：把二维码给到前端 ----
	if *addr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			out := map[string]any{"logged_in": c.IsLoggedInCached()}
			if qr, ok := c.LatestQR(); ok {
				out["qr_base64"] = qr.Base64
				out["data_url"] = qr.DataURL
				out["captured_at"] = qr.CapturedAt.Format(time.RFC3339)
			}
			_ = json.NewEncoder(w).Encode(out)
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8">
<title>抖音扫码登录</title>
<style>body{font-family:system-ui;background:#161823;color:#eee;text-align:center;padding-top:40px}
img{width:260px;height:260px;background:#fff;padding:12px;border-radius:12px;object-fit:contain}
p{color:#aaa;min-height:1.4em}</style>
<h2>抖音扫码登录</h2>
<img id="qr" alt="正在获取二维码...">
<p id="tip">正在获取二维码，请稍候…</p>
<p>二维码每 3 秒自动刷新；用「抖音 APP」左上角扫一扫</p>
<script>
let n = 0;
async function tick(){
  const img = document.getElementById('qr'), tip = document.getElementById('tip');
  try{
    const j = await (await fetch('/qr', {cache:'no-store'})).json();
    if(j.logged_in){ document.body.innerHTML = '<h2>✅ 登录成功，可以关掉本页</h2>'; return; }
    if(j.data_url && img.getAttribute('src') !== j.data_url){
      img.src = j.data_url;
      tip.textContent = '二维码已就绪，请用抖音 APP 扫码';
    } else if(!j.data_url){
      n++;
      tip.textContent = '正在获取二维码…（第 ' + n + ' 次，程序在重置页面/等待登录框）';
    }
  }catch(e){
    tip.textContent = '连接不上服务，正在重试…（确认程序还在运行）';
  }
  setTimeout(tick, 3000);
}
tick();
</script>`)
		})
		go func() {
			fmt.Printf("二维码服务：http://127.0.0.1%s/ （局域网用服务器 IP 访问）\n", *addr)
			if err := http.ListenAndServe(*addr, mux); err != nil {
				fmt.Println("HTTP 服务退出：", err)
			}
		}()
	}

	// ---- 2) 登录：未登录则推二维码 ----
	fmt.Println("等待登录（未登录会自动抓取二维码，通过 /qr 接口或页面展示）...")
	for {
		err := c.EnsureLogin(ctx, func(qr *douyinkit.QRCode) {
			fmt.Printf("【二维码】已更新（%s），可访问 %s/qr 获取 base64，或打开页面扫码\n",
				qr.CapturedAt.Format("15:04:05"), *addr)
		})
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			fmt.Println("已取消，退出")
			os.Exit(1)
		}
		// 不直接退出：一次抓码失败（页面未渲染完/网络抖动）不该让 HTTP 服务和常驻进程一起挂掉
		fmt.Println("登录流程出错：", err, "—— 10 秒后重新尝试（二维码服务保持可用）")
		select {
		case <-ctx.Done():
			os.Exit(1)
		case <-time.After(10 * time.Second):
		}
	}

	// ---- 3) 业务：干跑一轮 / 定向单个会话 / 持续自动回复 ----
	if *to != "" {
		n, err := c.SendTo(*to, !*send)
		if err != nil {
			fmt.Println("执行失败：", err)
			os.Exit(1)
		}
		fmt.Printf("定向处理完成，真实发送 %d 条\n", n)
		return
	}
	if *loop {
		fmt.Printf("进入持续轮询模式（真实发送=%v）...\n", *send)
		if err := c.Run(ctx, !*send); err != nil {
			fmt.Println("运行退出：", err)
			os.Exit(1)
		}
		return
	}
	n, err := c.RunOnce(!*send)
	if err != nil {
		fmt.Println("执行失败：", err)
		os.Exit(1)
	}
	fmt.Printf("本轮结束，真实发送 %d 条\n", n)
}
