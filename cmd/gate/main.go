// demo 6 —— 是否启动 Run 的开关演示（自动回复的「闸门」到底怎么控制）。
//
// 核心观念：Run() 才是真正在轮询/发送的「水泵」；AutoReply 只是闸门。
//
//	· 闸门关着 → 本 demo 根本不调用 Run()，进程不轮询、不发送（只留控制接口）
//	· 闸门开着 → 启动 Run()，按配置回复
//	· 运行中还能用 SetAutoReply 随时开/关（不需要重启进程）
//
// 用法：
//
//	go run ./cmd/gate                      # 干跑（不发消息），开接口 :6062
//	go run ./cmd/gate -send                # 真发
//	go run ./cmd/gate -no-reply            # 启动时闸门就是关的 → 不启动 Run
//	go run ./cmd/gate -send -addr :7000    # 自定义接口端口
//
// 控制接口（浏览器或 curl 都行）：
//
//	GET /status   当前状态：Run 是否在跑、自动回复是否开启
//	GET /toggle   切换自动回复开关（运行中立即生效，下一轮轮询按新值走）
//	GET /start    启动 Run（如果还没启动）
//	GET /stop     停止 Run（进程不退出，浏览器保持登录）
//	GET /qr       二维码/登录状态 JSON（未登录时可直接拿去扫码）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	douyinkit "github.com/Jason-cool/douyinkit"
	common "github.com/Jason-cool/douyinkit/cmd/internal/common"
)

func main() {
	send := flag.Bool("send", false, "真正发送（默认干跑，只打印会回谁）")
	noReply := flag.Bool("no-reply", false, "启动时就把自动回复关掉（演示「不启动 Run」）")
	addr := flag.String("addr", ":6062", "控制接口监听地址")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opt := common.Options()
	if *noReply {
		off := false
		opt.AutoReply = &off
	}
	opt.OnQR = func(qr *douyinkit.QRCode) {
		html := `<!doctype html><meta charset="utf-8"><title>扫码登录</title>` +
			`<body style="background:#161823;text-align:center;padding-top:60px">` +
			`<img src="` + qr.DataURL + `" style="width:300px;height:300px;background:#fff;padding:14px;border-radius:12px">` +
			`<p style="color:#eee">用抖音 APP 扫一扫</p></body>`
		_ = os.WriteFile("qr.html", []byte(html), 0o644)
		fmt.Println("【二维码】已写入 qr.html，浏览器打开扫码")
	}

	c, err := common.ClientWith(opt)
	if err != nil {
		fmt.Println("创建客户端失败：", err)
		os.Exit(1)
	}
	defer c.Close()

	// 未登录先扫码，否则 Run 也会因为页面不对而空转
	if !c.IsLoggedIn() {
		fmt.Println("未登录，等待扫码...")
		if err := c.EnsureLogin(ctx, nil); err != nil {
			fmt.Println("登录失败：", err)
			os.Exit(1)
		}
	}

	// ---- Run 的启动/停止控制 ----
	var (
		mu      sync.Mutex
		running bool
		cancel  context.CancelFunc
	)

	startRun := func() {
		mu.Lock()
		defer mu.Unlock()
		if running {
			return
		}
		rctx, cf := context.WithCancel(ctx)
		cancel = cf
		running = true
		fmt.Printf("▶ 启动 Run（dry=%v）\n", !*send)
		go func() {
			err := c.Run(rctx, !*send)
			mu.Lock()
			running = false
			mu.Unlock()
			if err != nil && rctx.Err() == nil {
				fmt.Println("Run 退出：", err)
			} else {
				fmt.Println("■ Run 已停止")
			}
		}()
	}

	stopRun := func() {
		mu.Lock()
		cf := cancel
		cancel = nil
		mu.Unlock()
		if cf != nil {
			cf()
		}
	}

	// ---- 关键：闸门决定要不要启动 Run ----
	if c.Config().AutoReply {
		fmt.Println("自动回复已开启 → 启动 Run 开始轮询")
		startRun()
	} else {
		fmt.Println("自动回复已关闭 → 不启动 Run：不轮询、不发送；可用 /toggle 打开后再 /start")
	}

	// ---- 控制接口 ----
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		run := running
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"run_running": run,
			"auto_reply":  c.Config().AutoReply,
			"dry":         !*send,
		})
	})
	mux.HandleFunc("/toggle", func(w http.ResponseWriter, r *http.Request) {
		cur := c.Config().AutoReply
		c.SetAutoReply(!cur) // 运行中立即生效：下一轮轮询就按新值走
		fmt.Printf("闸门切换：%v → %v\n", cur, !cur)
		_, _ = fmt.Fprintf(w, "auto_reply=%v\n", !cur)
	})
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		startRun()
		_, _ = fmt.Fprintln(w, "started")
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		stopRun()
		_, _ = fmt.Fprintln(w, "stopped")
	})
	mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		st, err := c.QRStatus()
		if err != nil {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(st)
	})

	go func() {
		fmt.Printf("控制接口：http://127.0.0.1%s/status\n", *addr)
		if err := http.ListenAndServe(*addr, mux); err != nil {
			fmt.Println("HTTP 服务退出：", err)
		}
	}()

	<-ctx.Done()
	stopRun()
	time.Sleep(500 * time.Millisecond)
	fmt.Println("已退出")
}
