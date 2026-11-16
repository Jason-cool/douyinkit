package douyinkit

// 配置 / 历史记录 / 免打扰 / 限流 —— 与 Python 版 load_config、load_history、
// in_quiet_hours、hour_count、cooldown_ok 逐一对应（复刻，不改逻辑）

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config 对应 Python 版 DEFAULT_CONFIG + config.json（_comment 字段自动忽略）
type Config struct {
	ReplyText             string    `json:"reply_text"`
	SameUserCooldownHours float64   `json:"same_user_cooldown_hours"`
	MaxRepliesPerHour     int       `json:"max_replies_per_hour"`
	PollIntervalSeconds   int       `json:"poll_interval_seconds"`
	QuietHours            []string  `json:"quiet_hours"`
	SkipIfLastMsgIsMine   bool      `json:"skip_if_last_msg_is_mine"`
	TypingDelaySeconds    []float64 `json:"typing_delay_seconds"`
	ReplyGroups           bool      `json:"reply_groups"`       // 群聊一律不自动回复
	CheckRecentConvs      bool      `json:"check_recent_convs"` // 对「刚刚/N分钟前」的会话主动核对

	// 二次消息跟进：自动回复过一次之后，客户再发消息 → 只截图、不再回复
	FollowUpScreenshot bool    `json:"followup_screenshot"`  // 是否给客户的二次消息截图
	FollowUpNoReply    bool    `json:"followup_no_reply"`    // 二次消息不再自动回复
	FollowUpWatchHours float64 `json:"followup_watch_hours"` // 观察期（小时），0 = 永久
}

func defaultConfig() *Config {
	return &Config{
		ReplyText:             "晚点回复您",
		SameUserCooldownHours: 0,
		MaxRepliesPerHour:     20,
		PollIntervalSeconds:   25,
		QuietHours:            []string{"00:30-07:30"},
		SkipIfLastMsgIsMine:   true,
		TypingDelaySeconds:    []float64{1.5, 3.5},
		ReplyGroups:           false,
		CheckRecentConvs:      true,
		FollowUpScreenshot:    true,
		FollowUpNoReply:       true,
		FollowUpWatchHours:    0,
	}
}

func loadConfig(path string) *Config {
	cfg := defaultConfig()
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			fmt.Printf("! 配置文件读取失败，使用默认值：%v\n", err)
		}
	}
	return cfg
}

// loadHistory / saveHistory：state/replied.json 格式与 Python 版完全一致（昵称 → ISO 时间）
func loadHistory(path string) map[string]string {
	h := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &h)
	}
	return h
}

func saveHistory(path string, h map[string]string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.MarshalIndent(h, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

var tsLayouts = []string{"2006-01-02T15:04:05", time.RFC3339, "2006-01-02 15:04:05"}

func parseTs(s string) (time.Time, bool) {
	for _, layout := range tsLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// inQuietHours 对应 Python in_quiet_hours（支持跨零点区间）
func inQuietHours(ranges []string) bool {
	now := time.Now()
	cur := now.Hour()*60 + now.Minute()
	for _, rng := range ranges {
		var ah, am, bh, bm int
		if _, err := fmt.Sscanf(rng, "%d:%d-%d:%d", &ah, &am, &bh, &bm); err != nil {
			continue
		}
		start, end := ah*60+am, bh*60+bm
		if start <= end {
			if start <= cur && cur < end {
				return true
			}
		} else { // 跨零点
			if cur >= start || cur < end {
				return true
			}
		}
	}
	return false
}

// hourCount：最近 60 分钟内回复了几个会话
func hourCount(hist map[string]string) int {
	cutoff := time.Now().Add(-time.Hour)
	n := 0
	for _, ts := range hist {
		if t, ok := parseTs(ts); ok && t.After(cutoff) {
			n++
		}
	}
	return n
}

func cooldownOK(hist map[string]string, name string, hours float64) bool {
	ts, ok := hist[name]
	if !ok || ts == "" {
		return true
	}
	t, ok := parseTs(ts)
	if !ok {
		return true
	}
	return time.Since(t) > time.Duration(hours*float64(time.Hour))
}
