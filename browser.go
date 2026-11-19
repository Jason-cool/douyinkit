package douyinkit

// 浏览器启动与页面工具 —— Linux 服务器无头（headless）运行；
// Windows/macOS 本地调试时有显示器路径可选。逻辑复刻 Windows 版 browser.go。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// killStaleChrome 杀掉仍占着本 profile 的上轮残留浏览器进程（全平台）。
// Leakless(false) 下 Go 进程被强杀后浏览器会存活：旧进程占着单例套接字，
// 新实例会把会话“移交”给旧进程后静默退出，launcher 读不到 debug url（报错内容为空）。
// 删锁文件对活进程无效，必须杀进程；只匹配启动参数 --user-data-dir=本目录，不会误杀用户自己的浏览器。
func killStaleChrome(dir string) {
	if runtime.GOOS == "windows" {
		killStaleChromeWindows(dir)
		return
	}
	_ = exec.Command("pkill", "-9", "-f", "user-data-dir="+dir).Run()
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
	cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
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

// 浏览器各启动步骤的超时。之前没有超时：任一步卡住（DevTools 连不上 / 私信页
// load 事件不来）就会永远停在「启动浏览器 ...」一行，进程看着活着其实在空转。
const (
	stepLaunchTimeout   = 90 * time.Second // chromium 进程起来并给出调试端口
	stepConnectTimeout  = 60 * time.Second // CDP 连接
	stepNavigateTimeout = 90 * time.Second // 打开私信页（服务器上风控/网络慢是常态，给足余量）
)

// openPage 打开 douyin.com/chat。headless=true 用于 Linux 服务器（无窗口）；
// headless=false 用于本地调试（弹窗口）。会话保存在 BaseDir/session/profile。
// 每一步都带超时并打日志：卡住时能看到具体停在哪一步，而不是一行「启动浏览器」挂死。
func openPage(channel, base string, headless bool, logf func(format string, args ...any)) (*rod.Browser, *rod.Page, func(), error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	l, err := newLauncher(channel, base, headless)
	if err != nil {
		return nil, nil, nil, err
	}
	// 启动前清理上次残留的单例锁、杀掉占着 profile 的残留进程，避免“profile 正被占用”启动失败
	clearProfileLocks(profileDir(base))
	killStaleChrome(profileDir(base))

	// Launch 同样可能挂死（chromium 起了却一直不吐调试端口，典型原因是 profile 被活进程占用）。
	// 包一层超时：到点杀掉残留进程并报错，让上层走重连/退避，而不是一行日志挂到天荒地老。
	type launchRes struct {
		url string
		err error
	}
	launchCh := make(chan launchRes, 1)
	go func() {
		u, err := l.Launch()
		launchCh <- launchRes{u, err}
	}()
	var u string
	select {
	case r := <-launchCh:
		u, err = r.url, r.err
	case <-time.After(stepLaunchTimeout):
		killStaleChrome(profileDir(base))
		return nil, nil, nil, fmt.Errorf("启动浏览器进程超时（%v）：chromium 一直没给出调试端口（常见原因：profile 被另一个活着的浏览器实例占用），已清理残留进程", stepLaunchTimeout)
	}
	if err != nil {
		// 重试一次：再杀残留进程 + 清锁后重启（应对旧进程刚在释放套接字的情况）
		if l2, e2 := newLauncher(channel, base, headless); e2 == nil {
			killStaleChrome(profileDir(base))
			clearProfileLocks(profileDir(base))
			u, err = l2.Launch()
		}
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("启动浏览器进程失败：%w（如果另一个实例正在运行，请先关闭它）", err)
	}
	logf("浏览器进程已启动，连接 DevTools ...")
	b := rod.New().ControlURL(u)
	// Connect 没有自带超时：端口被防火墙吞/进程假死时会永远等。
	// 不能用 b.Timeout()（会给整个浏览器会话设死限），用并行等待的方式只限「连接」这一步。
	connDone := make(chan error, 1)
	go func() { connDone <- b.Connect() }()
	select {
	case err := <-connDone:
		if err != nil {
			_ = b.Close()
			return nil, nil, nil, fmt.Errorf("连接浏览器 DevTools 失败：%w", err)
		}
	case <-time.After(stepConnectTimeout):
		_ = b.Close()
		killStaleChrome(profileDir(base))
		return nil, nil, nil, fmt.Errorf("连接浏览器 DevTools 超时（%v）：chromium 可能没起来或端口不通，已清理残留进程，请重试", stepConnectTimeout)
	}
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		_ = b.Close()
		return nil, nil, nil, fmt.Errorf("新建页面失败：%w", err)
	}
	// Navigate 等的是 load 事件：网络/风控把请求挂住时它永远不来，不设超时
	// 整个进程就会停在「启动浏览器 ...」。超时不算致命——页面可能只是慢，
	// 后续登录/二维码流程自己会继续等渲染。
	navCtx, navCancel := context.WithTimeout(context.Background(), stepNavigateTimeout)
	defer navCancel()
	if err := page.Context(navCtx).Navigate(chatURL); err != nil {
		logf("! 私信页 %v 内没有加载完（网络慢或被风控拦截），先继续登录流程", stepNavigateTimeout)
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
