// demo 4 —— 只设置自动回复开关：不启动浏览器，秒完成（写 config.json 的 auto_reply 字段）。
//
//	查看：          go run ./cmd/autoreply
//	开启：          go run ./cmd/autoreply -on
//	关闭：          go run ./cmd/autoreply -off
//	多账号：        go run ./cmd/autoreply -account 店铺A -off
//	指定数据目录：  go run ./cmd/autoreply -base /data/douyin -account 店铺B -on
//
// 关闭后该账号不再发送任何消息，但二次消息截图/观察照常。
// 注意：正在运行的实例不会自动感知文件改动，改完需重启该账号的程序（或调用 SetAutoReply）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Jason-cool/douyinkit/cmd/internal/common"
)

func main() {
	on := flag.Bool("on", false, "开启自动回复")
	off := flag.Bool("off", false, "关闭自动回复")
	flag.Parse()

	if *on == *off {
		fmt.Println("请指定 -on 或 -off（不带参数则只查看当前状态）")
	}

	dir := *common.Base
	if *common.Account != "" {
		dir = filepath.Join(dir, *common.Account)
	}
	path := filepath.Join(dir, "config.json")

	// 读现有配置（没有就用空表，避免覆盖其他字段）
	cfg := map[string]any{}
	exists := true
	if data, err := os.ReadFile(path); err != nil {
		exists = false
	} else if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Printf("config.json 解析失败（%v），请先手工确认\n", err)
		os.Exit(1)
	}

	cur, _ := cfg["auto_reply"].(bool)
	curStr := fmt.Sprintf("%v", cur)
	if !exists && !cur {
		curStr = "未配置（运行时默认开启）"
	}
	label := *common.Account
	if label == "" {
		label = "(默认账号)"
	}
	fmt.Printf("账号 %s  config: %s\n", label, path)

	if *on == *off { // 没给开关：只查看
		fmt.Printf("自动回复当前状态：%s\n", curStr)
		return
	}

	cfg["auto_reply"] = *on
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Println("序列化失败：", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Println("创建目录失败：", err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		fmt.Println("写入失败：", err)
		os.Exit(1)
	}
	action := "已开启"
	if *off {
		action = "已关闭（不再发送任何消息，二次消息照常截图）"
	}
	fmt.Printf("✔ 自动回复%s\n", action)
}
