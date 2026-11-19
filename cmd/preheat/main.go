// demo 7 —— 预热/常驻演示（CentOS 扫码慢的方案 B：把冷启动挪到扫码之前）。
//
// 做的事：创建客户端 → 立即 Preheat（后台启动浏览器 + 预取登录二维码）→
// 保持进程存活。之后不管走 /qr 轮询、EnsureLogin 还是 Run，都不再经历冷启动：
//
//	未登录的账号：用户要扫码时二维码已经提前抓好，秒出；
//	已登录的账号：浏览器常驻内存，轮询/发送直接干活。
//
// 用法：
//
//	go run ./cmd/preheat                       # 单账号预热，Ctrl+C 退出
//	go run ./cmd/preheat -account shop1        # 指定账号（数据目录 <base>/<账号名>）
//	go run ./cmd/preheat -addr :6063           # 顺带起 /qr 接口看预热效果
//
// 预热只是「提前干活」，不改变任何主逻辑；不调用 Preheat 的程序行为与之前完全一致。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"

	common "github.com/Jason-cool/douyinkit/cmd/internal/common"
)

func main() {
	addr := flag.String("addr", "", "可选：起一个 /qr 接口观察预热效果（如 :6063；留空不起服务）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	c, err := common.Client()
	if err != nil {
		fmt.Println("创建客户端失败：", err)
		os.Exit(1)
	}
	defer c.Close()

	// 核心：一行预热。异步立即返回，失败只打日志，不影响任何后续操作。
	c.Preheat()
	fmt.Println("已发起后台预热（启动浏览器 + 预取登录二维码），日志稍后输出预热结果")

	if *addr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			st, err := c.QRStatus()
			if err != nil {
				w.WriteHeader(500)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(st)
		})
		go func() {
			fmt.Printf("观察接口：http://127.0.0.1%s/qr（预热完成后应秒返回）\n", *addr)
			if err := http.ListenAndServe(*addr, mux); err != nil {
				fmt.Println("HTTP 服务退出：", err)
			}
		}()
	}

	fmt.Println("预热进程保持存活中，按 Ctrl+C 退出（退出即关闭浏览器）")
	<-ctx.Done()
	fmt.Println("已退出")
}
