// demo 5 —— 只管理「二次消息观察名单」：查看谁在观察期、把人工接手的客户移出名单。
// 不启动浏览器，秒完成。
//
//	查看名单：              go run ./cmd/followups
//	解除某客户观察（恢复自动回复）：go run ./cmd/followups -unwatch 某昵称
//	多账号：                go run ./cmd/followups -account 店铺A
//
// 名单内的会话：客户再来消息只截图（shots/followup/），不再自动回复；
// 解除后恢复普通自动回复。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	common "github.com/Jason-cool/douyinkit/cmd/internal/common"
)

func main() {
	unwatch := flag.String("unwatch", "", "把指定昵称的会话移出观察名单（支持部分匹配，取第一个命中的）")
	flag.Parse()

	c, err := common.Client()
	if err != nil {
		fmt.Println("创建客户端失败：", err)
		os.Exit(1)
	}
	defer c.Close()

	if *unwatch != "" {
		var hit string
		for _, f := range c.FollowUps() {
			if strings.Contains(f.Name, *unwatch) {
				hit = f.Name
				break
			}
		}
		if hit == "" {
			fmt.Printf("观察名单里没有匹配 %q 的会话\n", *unwatch)
			os.Exit(1)
		}
		if c.Unwatch(hit) {
			fmt.Printf("✔ 已解除观察：%s（之后再来消息恢复正常自动回复）\n", hit)
		}
		return
	}

	list := c.FollowUps()
	if len(list) == 0 {
		fmt.Println("观察名单为空（还没有自动回复过任何会话）")
		return
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	fmt.Printf("观察名单 %d 条：\n%s\n", len(list), b)
}
