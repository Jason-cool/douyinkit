// Package common 各独立 demo 共用的命令行参数与 Client 构造。
//
// 所有 demo 都支持统一的基础参数：
//
//	-base    数据目录（profile/logs/state/config），默认 ./douyinkit-data
//	-account 多账号模式：账号名，数据目录变为 <base>/<账号名>（各账号完全隔离）
//	-headless auto / yes / no（auto = Linux 无头、其他平台弹窗，方便本地调试）
//	-channel auto / chrome / chromium / msedge / bundled
package common

import (
	"flag"

	douyinkit "github.com/Jason-cool/douyinkit"
)

var (
	Base     = flag.String("base", "./douyinkit-data", "数据目录（profile/logs/state/config）")
	Account  = flag.String("account", "", "多账号模式：账号名，数据目录变为 <base>/<账号名>")
	Headless = flag.String("headless", "auto", "auto / yes / no")
	Channel  = flag.String("channel", "auto", "auto / chrome / chromium / msedge / bundled")
)

// Options 基础 Options（base/channel/headless/account 已填好），
// 回调（OnQR/OnReply/OnFollowUp）、Config、AutoReply 等由各 demo 自行补充。
func Options() douyinkit.Options {
	var headless *bool
	switch *Headless {
	case "yes", "true", "1":
		v := true
		headless = &v
	case "no", "false", "0":
		v := false
		headless = &v
	}
	return douyinkit.Options{
		Channel:  *Channel,
		Headless: headless,
	}
}

// ClientWith 用给定 Options 创建客户端；传了 -account 就走 Manager（多账号目录隔离）。
func ClientWith(opt douyinkit.Options) (*douyinkit.Client, error) {
	if *Account != "" {
		m, err := douyinkit.NewManager(*Base, opt)
		if err != nil {
			return nil, err
		}
		return m.Account(*Account)
	}
	opt.BaseDir = *Base
	return douyinkit.New(opt)
}

// Client 按命令行参数创建客户端（不需要自定义回调时用这个）。
func Client() (*douyinkit.Client, error) { return ClientWith(Options()) }
