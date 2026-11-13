package douyinkit

// 浏览器启动与页面工具 —— Linux 服务器无头（headless）运行；
// Windows/macOS 本地调试时有显示器路径可选。逻辑复刻 Windows 版 browser.go。

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

const chatURL = "https://www.douyin.com/chat"

// Python 版 AUTH_COOKIES：只认这三个 cookie（passport_csrf_token 匿名也会下发，不能算登录）
var authCookies = map[string]bool{"sessionid": true, "sessionid_ss": true, "sid_tt": true}

func profileDir(base string) string {
	return filepath.Join(base, "session", "profile")
}

func profileHasData(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) >= 3
}

// clearProfileLocks 删除 Chromium 单例锁（SingletonLock/Socket/Cookie）。
// 浏览器非正常退出（容器被强杀/崩溃）时这些锁会残留在持久化的 profile 目录里，
// 导致下次启动误报“profile appears to be in use by another Chromium process”而失败。
// SingletonLock 通常是符号链接，os.Remove 删的是链接本身，安全。
func clearProfileLocks(dir string) {
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func historyPath(base string) string { return filepath.Join(base, "state", "replied.json") }

func filepathJoin(parts ...string) string { return filepath.Join(parts...) }
func baseName(p string) string            { return filepath.Base(p) }

// pickChrome 跨平台探测本机 Chrome/Chromium/Edge；都找不到返回 ""（rod 会自动下载内核）
func pickChrome(channel string) (string, error) {
	if channel == "bundled" {
		return "", nil
	}
	var paths []string
	switch runtime.GOOS {
	case "windows":
		if channel == "auto" || channel == "chrome" {
			paths = append(paths,
				`C:\Program Files\Google\Chrome\Application\chrome.exe`,
				`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
				filepath.Join(os.Getenv("LOCALAPPDATA"), `Google\Chrome\Application\chrome.exe`),
			)
		}
		if channel == "auto" || channel == "msedge" {
			paths = append(paths,
				`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
				`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			)
		}
	case "linux":
		if channel == "auto" || channel == "chrome" || channel == "chromium" {
			paths = append(paths,
				"/usr/bin/google-chrome",
				"/usr/bin/google-chrome-stable",
				"/usr/bin/chromium",
				"/usr/bin/chromium-browser",
				"/snap/bin/chromium",
				"/usr/bin/chrome",
			)
		}
		if channel == "auto" || channel == "msedge" {
			paths = append(paths, "/usr/bin/microsoft-edge", "/usr/bin/microsoft-edge-stable")
		}
	case "darwin":
		if channel == "auto" || channel == "chrome" {
			paths = append(paths,
				"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
				"/Applications/Chromium.app/Contents/MacOS/Chromium",
			)
		}
	}
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if channel == "auto" || channel == "bundled" {
		return "", nil // 本机都没装 → rod 自管内核（首次自动下载）
	}
	return "", fmt.Errorf("没找到 %s 对应的浏览器，请安装 chromium 或改用 -channel auto/bundled", channel)
}

// openPage 打开 douyin.com/chat。headless=true 用于 Linux 服务器（无窗口）；
// headless=false 用于本地调试（弹窗口）。会话保存在 BaseDir/session/profile。
func openPage(channel, base string, headless bool) (*rod.Browser, *rod.Page, func(), error) {
	bin, err := pickChrome(channel)
	if err != nil {
		return nil, nil, nil, err
	}
	// Leakless(false)：Windows Defender 会把 leakless.exe 误报为病毒导致启动失败；
	// Linux 上无影响，统一关闭。代价：Go 进程被强杀时浏览器可能残留，正常退出不受影响。
	l := launcher.New().Leakless(false)
	if headless {
		// 用 new headless（Chromium 109+），比 old headless 更接近真实浏览器，风控误判更少
		l = l.Set("headless", "new").
			Set("window-size", "1440,900").
			Set("no-sandbox") // root / 容器环境必需
	} else {
		l = l.Headless(false).
			Set("window-size", "1440,900").
			Set("window-position", "60,20")
	}
	l = l.Set(flags.UserDataDir, profileDir(base)).
		Set("disable-blink-features", "AutomationControlled")
	if bin != "" {
		l = l.Bin(bin)
	}
	// 启动前清理上次残留的单例锁，避免“profile 正被占用”启动失败
	clearProfileLocks(profileDir(base))
	u, err := l.Launch()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("启动浏览器失败：%w（如果另一个实例正在运行，请先关闭它）", err)
	}
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		return nil, nil, nil, fmt.Errorf("连接浏览器失败：%w", err)
	}
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		_ = b.Close()
		return nil, nil, nil, fmt.Errorf("新建页面失败：%w", err)
	}
	if err := page.Navigate(chatURL); err != nil {
		_ = b.Close()
		return nil, nil, nil, fmt.Errorf("打开私信页失败：%w", err)
	}
	return b, page, func() { _ = b.Close() }, nil
}

// hasAuthCookie 复刻 Python has_auth_cookie
func hasAuthCookie(b *rod.Browser) bool {
	cookies, err := b.GetCookies()
	if err != nil {
		return false
	}
	for _, c := range cookies {
		if authCookies[c.Name] && len(c.Value) > 8 {
			return true
		}
	}
	return false
}

func loginPageShown(page *rod.Page) bool {
	v, ok := evalRaw(page, LOGIN_PAGE_JS)
	if !ok {
		return false
	}
	var b bool
	if v.Unmarshal(&b) != nil {
		return false
	}
	return b
}

func pageAlive(page *rod.Page) bool {
	_, err := page.Eval("() => 1")
	return err == nil
}

func wait(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func mouseClick(page *rod.Page, x, y float64) {
	page.Mouse.MustMoveTo(x, y).MustClick(proto.InputMouseButtonLeft)
}

func saveShot(page *rod.Page, path string) {
	data, err := page.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
