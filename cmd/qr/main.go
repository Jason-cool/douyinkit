// demo 2 —— 只取二维码 / 登录状态：适合前端对接，不进入回复流程。
//
//	查一次并打印 JSON：  go run ./cmd/qr
//	起 HTTP 接口常驻：    go run ./cmd/qr -serve :6060   （GET /qr 返回 JSON）
//	多账号：             go run ./cmd/qr -account 店铺A
//
// 返回 JSON 与库的 QRStatus() 结构一致：
//
//	已登录：  {"logged_in":true}
//	未登录：  {"logged_in":false,"qr_base64":"...","data_url":"data:image/png;base64,...","captured_at":"..."}
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

	common "github.com/Jason-cool/douyinkit/cmd/internal/common"
)

func main() {
	serve := flag.String("serve", "", "起 HTTP 服务并监听该地址（如 :6060）；留空则查一次就退出")
	flag.Parse()

	c, err := common.Client()
	if err != nil {
		fmt.Println("创建客户端失败：", err)
		os.Exit(1)
	}
	defer c.Close()

	status := func() ([]byte, int) {
		st, err := c.QRStatus()
		if err != nil {
			return []byte(fmt.Sprintf(`{"error":%q}`, err.Error())), 500
		}
		b, _ := json.Marshal(st)
		return b, 200
	}

	if *serve == "" {
		b, code := status()
		fmt.Println(string(b))
		os.Exit(code)
		return
	}

	http.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		b, code := status()
		w.WriteHeader(code)
		_, _ = w.Write(b)
	})
	fmt.Println("二维码接口已启动：http://127.0.0.1" + *serve + "/qr")
	if err := http.ListenAndServe(*serve, nil); err != nil {
		fmt.Println("HTTP 服务退出：", err)
		os.Exit(1)
	}
}
