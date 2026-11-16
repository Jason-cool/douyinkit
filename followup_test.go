package douyinkit

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 观察名单的落盘/读取/匹配逻辑（不依赖浏览器）

func TestFollowStoreRoundTrip(t *testing.T) {
	base := t.TempDir()
	s, err := loadFollowStore(base)
	if err != nil {
		t.Fatalf("空目录不应报错：%v", err)
	}
	if s.watching("张三", 0) {
		t.Fatal("没回复过的会话不应该在观察名单里")
	}

	s.markReplied("张三", "晚点回复您")
	if !s.watching("张三", 0) {
		t.Fatal("回复后应进入观察名单")
	}

	// 重新加载：状态要能持久化
	s2, err := loadFollowStore(base)
	if err != nil {
		t.Fatalf("重新加载失败：%v", err)
	}
	f, ok := s2.get("张三")
	if !ok || f.ReplyText != "晚点回复您" {
		t.Fatalf("持久化内容不对：%+v ok=%v", f, ok)
	}

	nf := s2.recordShot("张三", "好的", "/tmp/x.png")
	if nf.Shots != 1 || nf.LastText != "好的" {
		t.Fatalf("截图记录不对：%+v", nf)
	}
	if list := s2.list(); len(list) != 1 || list[0].LastPath != "/tmp/x.png" {
		t.Fatalf("列表内容不对：%+v", list)
	}

	if !s2.remove("张三") || s2.watching("张三", 0) {
		t.Fatal("Unwatch 后应移出名单")
	}
}

func TestFollowStoreWatchHours(t *testing.T) {
	base := t.TempDir()
	s, _ := loadFollowStore(base)
	s.markReplied("李四", "晚点回复您")
	// 观察期 1 小时：刚回复时在观察期，把时间往前推 2 小时就不在了
	if !s.watching("李四", 1) {
		t.Fatal("刚回复应在观察期内")
	}
	s.mu.Lock()
	s.m["李四"].RepliedAt = time.Now().Add(-2 * time.Hour)
	s.mu.Unlock()
	if s.watching("李四", 1) {
		t.Fatal("超过观察期应恢复为普通会话")
	}
	if !s.watching("李四", 0) {
		t.Fatal("0 表示永久观察")
	}
}

func TestFollowStoreBadFile(t *testing.T) {
	base := t.TempDir()
	path := followStatePath(base)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// 时间没带时区 → time.Time 解析失败，旧代码会静默丢掉整份名单
	bad := `{"张三":{"name":"张三","replied_at":"2026-11-16T12:00:00"}}`
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := loadFollowStore(base)
	if err == nil {
		t.Fatal("坏文件应该返回错误，而不是静默变成空名单")
	}
	if s == nil || s.watching("张三", 0) {
		t.Fatal("坏文件时不应误判为在观察")
	}

	// 正常写入的文件（带时区）必须能读回
	good := `{"张三":{"name":"张三","reply_text":"晚点回复您","replied_at":"2026-11-16T12:00:00+08:00"}}`
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := loadFollowStore(base)
	if err != nil {
		t.Fatalf("正常文件读取失败：%v", err)
	}
	if !s2.watching("张三", 0) {
		t.Fatal("正常文件应读到观察记录")
	}
}

func TestSeedFromHistory(t *testing.T) {
	base := t.TempDir()
	s, _ := loadFollowStore(base)
	hist := map[string]string{
		"a": time.Now().Add(-1 * time.Hour).Format("2006-01-02T15:04:05"),
		"b": time.Now().Add(-30 * 24 * time.Hour).Format("2006-01-02T15:04:05"),
		"c": "不是时间",
	}
	if n := s.seedFromHistory(hist, "晚点回复您"); n != 1 {
		t.Fatalf("只应补入最近 7 天的 1 条，实际 %d", n)
	}
	if !s.watching("a", 0) || s.watching("b", 0) || s.watching("c", 0) {
		t.Fatal("补入范围不对")
	}
}

func TestNeedFollowCheck(t *testing.T) {
	f := FollowUp{ReplyText: "晚点回复您"}
	cases := []struct {
		name string
		c    ConvItem
		want bool
	}{
		{"预览是我们的回复 → 不看", ConvItem{Preview: "24分钟前 晚点回复您"}, false},
		{"预览变成对方的话 → 要看", ConvItem{Preview: "刚刚 好的，什么时候"}, true},
		{"有未读 → 要看", ConvItem{Preview: "晚点回复您", Unread: true}, true},
		{"预览是旧的 → 要看（可能是非文字消息）", ConvItem{Preview: "10/25 在吗"}, true},
	}
	for _, c := range cases {
		if got := needFollowCheck(c.c, f); got != c.want {
			t.Errorf("%s：want %v got %v", c.name, c.want, got)
		}
	}
}
