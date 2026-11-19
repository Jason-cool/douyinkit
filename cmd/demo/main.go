// douyinkit 演示程序 —— 展示库的完整用法：
//  1. 启动 HTTP 服务（默认 :6060）：GET /qr 返回 JSON（qr_base64/data_url/logged_in），
//     GET /followups 返回「已自动回复、正在等客户二次消息」的会话（含截图路径），
//     GET / 返回一个自动刷新的二维码渲染页（前端接入示例）
//  2. EnsureLogin：未登录则抓二维码（通过 OnQR 回调打印提示），扫码成功后继续
//  3. 默认干跑一轮；-send -loop 则进入持续自动回复
//     （自动回复过一次的会话，客户再来消息只截图不回复，见 followup.go）
//  4. 多账号：-account shop1 -base /data/douyin → 数据落在 /data/douyin/shop1，
//     一个进程管一个账号；要同时跑多个账号就起多个进程（每个账号一个独立浏览器）
//
// 作为命令行运行：
//
//	go run ./cmd/demo                  # 干跑一轮
//	go run ./cmd/demo -send -loop      # 持续自动回复
//	go run ./cmd/demo -base /data/dy   # 指定数据目录
//	go run ./cmd/demo -account shop1 -no-reply   # 指定账号 + 关闭自动回复（只截图/观察）
//	go run ./cmd/demo -preheat -loop             # 常驻+预热：启动即备好浏览器和二维码（服务器推荐）
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

	douyinkit "github.com/Jason-cool/douyinkit"
)

func main() {
	base := flag.String("base", "./douyinkit-data", "数据目录（多账号时是各账号目录的父目录）")
	addr := flag.String("addr", ":6060", "二维码 HTTP 服务监听地址（留空则不启动）")
	send := flag.Bool("send", false, "真正发送（默认干跑）")
	loop := flag.Bool("loop", false, "持续轮询，不退出")
	channel := flag.String("channel", "auto", "auto/chrome/chromium/msedge/bundled")
	to := flag.String("to", "", "只处理指定昵称的会话（测试发送链路用，配合 -send 才真发）")
	account := flag.String("account", "", "账号名（多账号管理：数据落在 <base>/<account>，日志带账号前缀）")
	noReply := flag.Bool("no-reply", false, "关闭该账号的自动回复（只做二次消息截图/观察，不发送任何消息）")
	noHeadless := flag.Bool("no-headless", false, "弹浏览器窗口（本地调试用；Linux 服务器默认无头）")
	forceHeadless := flag.Bool("headless", false, "强制无头模式（在 Windows 上模拟 Linux 服务器环境）")
	preheat := flag.Bool("preheat", false, "启动即后台预热（常驻模式）：提前启动浏览器+预取登录二维码，扫码时秒出")
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

	autoReply := !*noReply
	opt := douyinkit.Options{
		Channel:  *channel,
		Headless: headless,
		OnReply: func(name, result string, sent bool) {
			fmt.Printf("【回调】会话=%s 结果=%s 已发送=%v\n", name, result, sent)
		},
		OnFollowUp: func(f douyinkit.FollowUp) {
			fmt.Printf("【二次消息】%s 回复过 %q，客户又来消息 → 截图 %s\n", f.Name, f.ReplyText, f.LastPath)
		},
		AutoReply: &autoReply,
	}

	var c *douyinkit.Client
	var err error
	if *account != "" {
		// 多账号：用 Manager 按账号名隔离数据目录（<base>/<account>）
		m, mgrErr := douyinkit.NewManager(*base, opt)
		if mgrErr != nil {
			fmt.Println("创建多账号管理器失败：", mgrErr)
			os.Exit(1)
		}
		c, err = m.Account(*account)
		if err != nil {
			fmt.Println("创建客户端失败：", err)
			os.Exit(1)
		}
		fmt.Printf("多账号模式：账号=%s 数据目录=%s\n", *account, c.Account())
	} else {
		opt.BaseDir = *base
		c, err = douyinkit.New(opt)
		if err != nil {
			fmt.Println("创建客户端失败：", err)
			os.Exit(1)
		}
	}
	defer c.Close()

	// ---- 0) 可选：常驻预热（异步，不阻塞登录/HTTP 流程；冷启动成本挪到扫码之前） ----
	if *preheat {
		c.Preheat()
		fmt.Println("已开始后台预热（启动浏览器 + 预取登录二维码）...")
	}

	// ---- 1) HTTP 服务：把二维码给到前端 ----
	if *addr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			// 接口内部就是调用库方法，保证示例与库行为完全一致
			st, err := c.QRStatus()
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(st)
		})
		mux.HandleFunc("/followups", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			list := c.FollowUps()
			if list == nil {
				list = []douyinkit.FollowUp{}
			}
			_ = json.NewEncoder(w).Encode(list)
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
