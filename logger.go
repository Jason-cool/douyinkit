package douyinkit

// 日志：写文件（BaseDir/logs/日期.log）+ 可选的用户钩子。
// 调用方可以通过 Config.Logf 完全接管日志输出。

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	mu         sync.Mutex
	f          *os.File
	UserLog    func(format string, args ...any)     // 调用方提供的日志函数（可为 nil，默认打 stdout）
	Hook       func(name, result string, sent bool) // 回复事件钩子（OnReply，可为 nil）
	FollowHook func(f FollowUp)                     // 二次消息截图钩子（OnFollowUp，可为 nil）
}

func newLogger(base string) *Logger {
	lg := &Logger{}
	dir := filepath.Join(base, "logs")
	_ = os.MkdirAll(dir, 0o755)
	if f, err := os.OpenFile(filepath.Join(dir, time.Now().Format("2006-01-02")+".log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		lg.f = f
	}
	return lg
}

func (lg *Logger) Log(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if lg.UserLog != nil {
		lg.UserLog("%s", line)
	} else {
		fmt.Println(line)
	}
	if lg.f != nil {
		_, _ = lg.f.WriteString(line + "\n")
		_ = lg.f.Sync()
	}
}

// Close 关闭日志文件句柄（进程退出前调用；Close 后再 Log 只会丢文件输出）
func (lg *Logger) Close() {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if lg.f != nil {
		_ = lg.f.Close()
		lg.f = nil
	}
}
