package douyinkit

// 浏览器启动与页面工具 —— Linux 服务器无头（headless）运行；
// Windows/macOS 本地调试时有显示器路径可选。逻辑复刻 Windows 版 browser.go。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
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

// killStaleChrome 杀掉仍占着本 profile 的上轮残留浏览器进程（全平台）。
// Leakless(false) 下 Go 进程被强杀后浏览器会存活：旧进程占着单例套接字，
// 新实例会把会话“移交”给旧进程后静默退出，launcher 读不到 debug url（报错内容为空）。
// 删锁文件对活进程无效，必须杀进程；只匹配启动参数 --user-data-dir=本目录，不会误杀用户自己的浏览器。
func killStaleChrome(dir string) {
	if runtime.GOOS == "windows" {
		killStaleChromeWindows(dir)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pattern := `(^|[[:space:]])--user-data-dir=` + regexp.QuoteMeta(dir) + `([[:space:]]|$)`
	_ = exec.CommandContext(ctx, "pkill", "-9", "-f", pattern).Run()
	time.Sleep(500 * time.Millisecond)
}

// killStaleChromeWindows Windows 版：命令行里包含本 profile 目录的 chrome/msedge 才杀，
// 不碰用户自己开着的浏览器。目录通过环境变量传入，避免中文路径在命令行里的转义问题。
// （v3.1 之前 Windows 直接跳过清理，导致本地残留浏览器后每次启动都报
// "Failed to get the debug url: exit status 1"，只能手动杀进程。）
func killStaleChromeWindows(dir string) {
	ps := windowsPowerShell()
	if ps == "" {
		return // 连 PowerShell 都找不到，只能靠后面的启动重试
	}
	script := "Get-CimInstance Win32_Process -Filter \"Name='chrome.exe' OR Name='msedge.exe'\" | " +
		"Where-Object { $_.CommandLine -and $_.CommandLine -like (\"*\" + $env:DY_PROFILE_DIR + \"*\") } | " +
		"ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "DY_PROFILE_DIR="+dir)
	_ = cmd.Run()
	time.Sleep(800 * time.Millisecond) // 等进程真正释放 profile 句柄
}

// windowsPowerShell 返回 PowerShell 可执行文件的绝对路径。
// 不能直接 exec.Command("powershell")：从 Git Bash、计划任务或精简 PATH 的环境启动时，
// PATH 里常常没有 System32，会报 "executable file not found"，自检静默失效 ——
// 表现就是残留浏览器一直杀不掉、每次启动都「Failed to get the debug url」。
func windowsPowerShell() string {
	if p, err := exec.LookPath("powershell"); err == nil {
		return p
	}
	for _, root := range []string{os.Getenv("SystemRoot"), os.Getenv("windir"), `C:\Windows`} {
		if root == "" {
			continue
		}
		p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
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

// newLauncher 组装 launcher 配置（openPage 首次启动与重试共用）
func newLauncher(channel, base string, headless bool) (*launcher.Launcher, error) {
	bin, err := pickChrome(channel)
	if err != nil {
		return nil, err
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
	return l, nil
}

// 启动各步骤分别设置超时；导航返回并不表示页面渲染或登录已完成。
const (
	stepLaunchTimeout   = 45 * time.Second // chromium 进程起来并给出调试端口
	stepConnectTimeout  = 15 * time.Second // CDP 连接
	stepNavigateTimeout = 30 * time.Second // 打开私信页（服务器上风控/网络慢是常态，给足余量）
)

// openPage 打开 douyin.com/chat。headless=true 用于 Linux 服务器（无窗口）；
// headless=false 用于本地调试（弹窗口）。会话保存在 BaseDir/session/profile。
// 每一步都带超时并打日志：卡住时能看到具体停在哪一步，而不是一行「启动浏览器」挂死。
func openPage(ctx context.Context, channel, base string, headless bool, logf func(format string, args ...any)) (*rod.Browser, *rod.Page, func(), error) {
	return openPageAtURL(ctx, channel, base, headless, chatURL, logf)
}

func openPageAtURL(ctx context.Context, channel, base string, headless bool, targetURL string, logf func(format string, args ...any)) (*rod.Browser, *rod.Page, func(), error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	started := time.Now()
	killStaleChrome(profileDir(base))
	clearProfileLocks(profileDir(base))
	browserLog, _ := os.OpenFile(filepath.Join(base, "logs", "chromium.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	var l *launcher.Launcher
	var u string
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		l, err = newLauncher(channel, base, headless)
		if err != nil {
			break
		}
		if browserLog != nil {
			l.Logger(browserLog)
		}
		launchCtx, launchCancel := context.WithTimeout(ctx, stepLaunchTimeout)
		phaseStart := time.Now()
		u, err = l.Context(launchCtx).Launch()
		launchCancel()
		logf("浏览器进程启动：第 %d 次，耗时 %v，成功=%v", attempt, time.Since(phaseStart).Round(time.Millisecond), err == nil)
		if err == nil {
			break
		}
		l.Kill()
		clearProfileLocks(profileDir(base))
	}
	if err != nil {
		if browserLog != nil {
			_ = browserLog.Close()
		}
		return nil, nil, nil, fmt.Errorf("启动浏览器失败：%w（详细输出见 logs/chromium.log）", err)
	}
	// The browser connection needs a lifetime context, not a startup deadline.
	// Cancel it on failed startup so in-flight Connect/Page calls actually stop.
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	b := rod.New().Context(sessionCtx).ControlURL(u)
	connectedOK := false
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			if connectedOK {
				closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = b.Context(closeCtx).Close()
				cancel()
			}
			sessionCancel()
			l.Kill()
			if browserLog != nil {
				_ = browserLog.Close()
			}
		})
	}
	failed := true
	defer func() {
		if failed {
			cleanup()
		}
	}()
	phaseStart := time.Now()
	logf("浏览器进程已启动，连接 DevTools ...")
	connectCtx, connectCancel := context.WithTimeout(ctx, stepConnectTimeout)
	defer connectCancel()
	connected := make(chan error, 1)
	go func() { connected <- b.Connect() }()
	select {
	case err = <-connected:
	case <-connectCtx.Done():
		err = connectCtx.Err()
		sessionCancel()
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("连接 DevTools 失败（耗时 %v）：%w", time.Since(phaseStart).Round(time.Millisecond), err)
	}
	connectedOK = true
	logf("DevTools 已连接（%v）", time.Since(phaseStart).Round(time.Millisecond))
	phaseStart = time.Now()
	pageCtx, pageCancel := context.WithTimeout(ctx, 15*time.Second)
	defer pageCancel()
	// Page owns Mouse/Keyboard/root objects. Creating it on a temporary
	// browser context leaves those objects canceled even if Page.Context is
	// later replaced. Build the entire page on the lifetime context instead.
	type pageResult struct {
		page *rod.Page
		err  error
	}
	pageDone := make(chan pageResult, 1)
	go func() { p, e := b.Page(proto.TargetCreateTarget{}); pageDone <- pageResult{p, e} }()
	var page *rod.Page
	select {
	case result := <-pageDone:
		page, err = result.page, result.err
	case <-pageCtx.Done():
		err = pageCtx.Err()
		sessionCancel()
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("新建页面失败：%w", err)
	}
	logf("标签页已创建（%v）", time.Since(phaseStart).Round(time.Millisecond))
	phaseStart = time.Now()
	navCtx, navCancel := context.WithTimeout(ctx, stepNavigateTimeout)
	err = page.Context(navCtx).Navigate(targetURL)
	navCancel()
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	if err != nil {
		logf("! 私信页导航暂未完成（%v）：%v，后续继续检测页面", time.Since(phaseStart).Round(time.Millisecond), err)
	} else {
		logf("私信页导航完成（%v）；页面渲染和登录另行确认", time.Since(phaseStart).Round(time.Millisecond))
	}
	logf("浏览器启动各步骤完成（总计 %v）", time.Since(started).Round(time.Millisecond))
	failed = false
	return b, page, cleanup, nil
}

// hasAuthCookie 复刻 Python has_auth_cookie
func hasAuthCookie(b *rod.Browser) bool {
	if b == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginProbeTimeout)
	defer cancel()
	cookies, err := b.Context(ctx).GetCookies()
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
	if page == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginProbeTimeout)
	defer cancel()
	_, err := page.Context(ctx).Eval("() => 1")
	return err == nil
}

func wait(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func mouseClick(page *rod.Page, x, y float64) {
	if err := page.Mouse.MoveTo(proto.Point{X: x, Y: y}); err != nil {
		return
	}
	_ = page.Mouse.Click(proto.InputMouseButtonLeft, 1)
}

func saveShot(page *rod.Page, path string) {
	data, err := page.Timeout(5*time.Second).Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}
