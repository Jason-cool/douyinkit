package douyinkit

// 二次消息跟进
//
// 规则：我们给某个会话自动回复过一次之后，把它记进「观察名单」；
// 之后客户再发消息时，**不再自动回复**，改为把聊天页截图存到
// BaseDir/shots/followup/，并通过 Options.OnFollowUp 回调出去。
//
// 状态持久化在 BaseDir/state/followup.json，进程重启不丢：重启后客户再来消息，
// 依旧只截图不回复。同一条消息只截一次（按最后一条消息的文字去重）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// FollowUp 一条「已自动回复、正在观察客户二次消息」的会话记录
type FollowUp struct {
	Name       string     `json:"name"`                // 会话昵称
	ReplyText  string     `json:"reply_text"`          // 当时自动回复的内容
	RepliedAt  time.Time  `json:"replied_at"`          // 自动回复时间
	Shots      int        `json:"shots"`               // 已截图次数
	LastText   string     `json:"last_text,omitempty"` // 最近一次截图对应的客户消息（去重用）
	LastPath   string     `json:"last_path,omitempty"` // 最近一次截图文件
	LastShotAt *time.Time `json:"last_shot_at,omitempty"`
}

// followStore 观察名单（带文件持久化）
type followStore struct {
	mu      sync.Mutex
	path    string
	m       map[string]*FollowUp
	checked map[string]time.Time // 本次进程内的核对时间，避免无红点会话被反复点开
}

func followStatePath(base string) string {
	return filepath.Join(base, "state", "followup.json")
}

func loadFollowStore(base string) (*followStore, error) {
	s := &followStore{path: followStatePath(base), m: map[string]*FollowUp{}, checked: map[string]time.Time{}}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(data, &s.m); err != nil {
		// 文件被手改坏（比如时间没带时区）时不要把整份名单静默丢掉：报错并让调用方决定
		s.m = map[string]*FollowUp{}
		return s, fmt.Errorf("解析 %s 失败：%w", s.path, err)
	}
	if s.m == nil {
		s.m = map[string]*FollowUp{}
	}
	return s, nil
}

// seedFromHistory 把「升级前已经自动回复过」的会话也纳入观察名单，
// 否则升级后客户再回一句，程序还会再发一条「晚点回复您」（与「只回一次」的预期相反）。
// 只回溯最近 followSeedDays 天，避免把几个月前的历史会话全部点开核对。
func (s *followStore) seedFromHistory(hist map[string]string, replyText string) int {
	if s == nil || len(hist) == 0 {
		return 0
	}
	cutoff := time.Now().Add(-followSeedDays * 24 * time.Hour)
	n := 0
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, ts := range hist {
		if name == "" {
			continue
		}
		if _, ok := s.m[name]; ok {
			continue
		}
		t, ok := parseTs(ts)
		if !ok || t.Before(cutoff) {
			continue
		}
		s.m[name] = &FollowUp{Name: name, ReplyText: replyText, RepliedAt: t}
		n++
	}
	if n > 0 {
		s.saveLocked()
	}
	return n
}

// followSeedDays 升级时从 replied.json 回溯多少天的历史回复
const followSeedDays = 7

func (s *followStore) saveLocked() {
	data, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	_ = os.WriteFile(s.path, data, 0o644)
}

// markReplied 自动回复成功后调用：进入观察名单（或刷新时间/话术）
func (s *followStore) markReplied(name, replyText string) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.m[name]
	if f == nil {
		f = &FollowUp{Name: name}
		s.m[name] = f
	}
	f.ReplyText = replyText
	f.RepliedAt = time.Now()
	s.saveLocked()
}

func (s *followStore) get(name string) (FollowUp, bool) {
	if s == nil {
		return FollowUp{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.m[name]
	if !ok {
		return FollowUp{}, false
	}
	return *f, true
}

// watching 该会话是否还在观察期。hours <= 0 表示永久（默认：回复过一次就不再自动回复）
func (s *followStore) watching(name string, hours float64) bool {
	f, ok := s.get(name)
	if !ok {
		return false
	}
	if hours > 0 && time.Since(f.RepliedAt) > time.Duration(hours*float64(time.Hour)) {
		return false
	}
	return true
}

func (s *followStore) recordShot(name, text, path string) FollowUp {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.m[name]
	if f == nil {
		f = &FollowUp{Name: name}
		s.m[name] = f
	}
	now := time.Now()
	f.Shots++
	f.LastText = text
	f.LastPath = path
	f.LastShotAt = &now
	s.saveLocked()
	return *f
}

func (s *followStore) list() []FollowUp {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FollowUp, 0, len(s.m))
	for _, f := range s.m {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].RepliedAt, out[j].RepliedAt
		if out[i].LastShotAt != nil {
			ti = *out[i].LastShotAt
		}
		if out[j].LastShotAt != nil {
			tj = *out[j].LastShotAt
		}
		return ti.After(tj)
	})
	return out
}

func (s *followStore) recentlyChecked(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.checked[name]
	return ok && time.Since(t) < followCheckGap
}

func (s *followStore) markChecked(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked[name] = time.Now()
}

// followCheckGap 同一会话两次「点开核对」的最小间隔（有红点时不受限制）。
// 没有这道闸，一个预览里看不到我们回复的会话（列表截断、非文字消息等）会被每轮轮询反复点开。
const followCheckGap = 90 * time.Second

func (s *followStore) remove(name string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[name]; !ok {
		return false
	}
	delete(s.m, name)
	delete(s.checked, name)
	s.saveLocked()
	return true
}

// ---------------------------------------------------------------- 扫描

// needFollowCheck 判断这个会话值不值得点开核对。
// 列表预览最便宜也最可靠：只要预览里还是我们那条回复，就说明客户没再说话；
// 预览变了（客户回了「好的」/发了图片/发来新消息）或出现红点，才点开确认。
// 这里故意不用 isRecent —— 它对「24分钟前」也会匹配（正则取最后一位数字），
// 会把一批早就回复过的会话每轮都点开，白白清掉别人的未读。
func needFollowCheck(c ConvItem, f FollowUp) bool {
	if c.Unread {
		return true
	}
	if f.ReplyText == "" {
		return true
	}
	return !strings.Contains(c.Preview, f.ReplyText)
}

// scanFollowUps 对观察名单里的会话做一次核对：客户有新消息就截图，返回截图张数。
// dry=true 时只判断不落盘（干跑模式）。
func scanFollowUps(page *rod.Page, items []ConvItem, cfg *Config, fs *followStore, dry bool, lg *Logger, base string) int {
	if fs == nil || !cfg.FollowUpScreenshot || len(items) == 0 {
		return 0
	}
	shots := 0
	for _, c := range items {
		if c.Kind == "stranger_folder" || looksLikeGroup(c) {
			continue
		}
		f, ok := fs.get(c.Name)
		if !ok {
			continue
		}
		if !fs.watching(c.Name, cfg.FollowUpWatchHours) {
			continue
		}
		if !needFollowCheck(c, f) {
			continue
		}
		// 无红点的会话 90 秒内不重复点开（有红点=确实有新消息，照常核对）
		if !c.Unread && fs.recentlyChecked(c.Name) {
			continue
		}
		fs.markChecked(c.Name)

		opened, why := openConversation(page, c.Name, lg)
		if !opened {
			lg.Log("  [二次消息] 打不开会话 %q（%s），下轮再试", truncate(c.Name, 18), why)
			continue
		}

		last := evalLastMsg(page)
		if last == nil {
			continue
		}
		if last.Side != "left" {
			// 最后一条还是我们自己的（或判断不出左右）→ 客户还没回，什么都不做
			lg.Log("  [二次消息] %q 最后一条不是对方消息，继续观察", truncate(c.Name, 18))
			continue
		}

		key := strings.TrimSpace(last.Text)
		if key == "" {
			key = c.Preview // 非文字消息（图片/表情）拿不到文字，用预览兜底去重
		}
		if key != "" && key == f.LastText {
			continue // 同一条消息已截过
		}

		if dry {
			lg.Log("  [二次消息] %q 有客户新消息 %q（干跑，不截图）", truncate(c.Name, 18), truncate(last.Text, 24))
			continue
		}

		path := followShot(page, base, c.Name)
		if path == "" {
			lg.Log("  [二次消息] %q 截图失败，下轮重试", truncate(c.Name, 18))
			continue
		}
		nf := fs.recordShot(c.Name, key, path)
		shots++
		lg.Log("  [二次消息] %q：%q → 已截图 %s（已回复过，不再回复）",
			truncate(c.Name, 18), truncate(last.Text, 24), baseName(path))
		if lg.FollowHook != nil {
			lg.FollowHook(nf)
		}
	}
	return shots
}

// followShot 把当前聊天页截下来（先滚到最新一条），存到 shots/followup/。
// 返回文件路径；失败返回 ""。
func followShot(page *rod.Page, base, name string) string {
	_, _ = evalRaw(page, SCROLL_CHAT_BOTTOM_JS) // 尽量滚到底，保证最新消息在截图里
	wait(600)

	dir := filepath.Join(base, "shots", "followup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	safe := strings.Trim(shotNameRe.ReplaceAllString(name, "_"), "_")
	if r := []rune(safe); len(r) > 20 {
		safe = string(r[:20])
	}
	if safe == "" {
		safe = "conv"
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.png", safe, time.Now().Format("20060102_150405")))
	data, err := page.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
	if err != nil || len(data) == 0 {
		return ""
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ""
	}
	return path
}
