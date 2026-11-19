# douyinkit —— 抖音私信自动回复（Linux 服务器版 / 可引用 Go 库）

从 Windows 版 `抖音私信助手-Go` 移植而来，**回复逻辑零改动**（扫描未读、群聊拦截、陌生人二级入口、
左右气泡判归属、零宽空格防护、冷却/限流/免打扰全部保留），差别在于：

| | Windows 版（已备份） | 本版 |
|---|---|---|
| 形态 | 独立 exe + bat | **可 import 的 Go 库** + 演示程序 |
| 浏览器 | 有窗口 | **无头（headless）**，服务器无桌面也能跑 |
| 登录 | 本地扫码（看屏幕） | **未登录时抓二维码 → base64/链接返回，前端渲染扫码** |
| 平台 | Windows | Linux（同时兼容 Windows/macOS 便于调试） |

---

## 1. 作为库引用

```go
import douyinkit "github.com/Jason-cool/douyinkit"

// 方式一：发布到自己的仓库后
//   go get github.com/Jason-cool/douyinkit
// 方式二：不发布，本地用 replace 指到本目录
//   require github.com/Jason-cool/douyinkit v0.0.0
//   replace github.com/Jason-cool/douyinkit => /path/to/douyinkit

c, err := douyinkit.New(douyinkit.Options{
    BaseDir: "/var/lib/douyin",        // 登录态/日志/冷却记录都放这里
    Headless: nil,                      // nil = Linux 自动无头，本地调试传 false
    OnQR: func(qr *douyinkit.QRCode) {
        // 二维码（首次抓取 + 每次刷新）都会回调到这里
        // qr.DataURL 形如 data:image/png;base64,xxx ，直接给前端 <img src>
        // qr.Base64  是纯 base64（PNG 字节），走 JSON 接口时用它
        pushToFrontend(qr.DataURL)
    },
    OnReply: func(name, result string, sent bool) {
        log.Printf("会话=%s 结果=%s 已发送=%v", name, result, sent)
    },
})
defer c.Close()

// 登录：已登录直接返回；未登录则抓二维码回调 OnQR，并持续监控刷新，直到扫码成功
if err := c.EnsureLogin(ctx, nil); err != nil {
    return err
}

// 业务：dry=true 只判定不发送；Run 是持续轮询（页面断线会自动重连）
c.Run(ctx, false)
```

`go.mod` 引用本地目录：

```
require douyinkit v0.0.0
replace douyinkit => /path/to/抖音私信助手-Linux
```

### 核心 API

| API | 说明 |
|---|---|
| `New(Options) (*Client, error)` | 创建客户端（不启动浏览器） |
| `(*Client) Connect() error` | 启动/复用无头浏览器并打开私信页，幂等 |
| `(*Client) IsLoggedIn() bool` | 是否已登录（按需自动 Connect） |
| `(*Client) EnsureLogin(ctx, onQR) error` | **未登录时抓二维码 + 推给回调 + 等扫码**；已登录秒返回 |
| `(*Client) QRStatus() (*LoginStatus, error)` | **一次拿到「已登录 / 未登录+二维码」**，等价于 `/qr` 接口但不用起 HTTP（见下） |
| `(*Client) FetchQR() (*QRCode, error)` | 主动抓一张当前二维码（含过期自动刷新） |
| `(*Client) LatestQR() (*QRCode, bool)` | 取最近一次二维码（给 `/qr` 这类接口用） |
| `(*Client) RunOnce(dry bool) (int, error)` | 跑一轮，返回真实发送条数 |
| `(*Client) Run(ctx, dry bool) error` | 持续轮询，页面挂了自动重连（3+3 次重试） |
| `(*Client) FollowUps() []FollowUp` | 已自动回复、正在观察客户二次消息的会话（含最近截图路径） |
| `(*Client) Unwatch(name string) bool` | 把会话移出观察名单（人工接手后可恢复正常自动回复） |
| `(*Client) Close()` | 关闭浏览器 |

`QRCode` 结构：`Base64`（纯 base64）、`DataURL`（`data:image/png;base64,...`）、`CapturedAt`。

### QRStatus：不用起接口，一次拿到「已登录 / 二维码」

`QRStatus` 就是演示程序 `GET /qr` 的库内版本，导包后直接调用，返回结构与接口 JSON 一致：

```go
st, err := c.QRStatus()
switch {
case err != nil:
    log.Println("取二维码失败：", err)
case st.LoggedIn:
    fmt.Println(st.Text())      // "已登录"
default:
    fmt.Println(st.Text())      // "未登录，请用抖音 App 扫码"
    fmt.Println(st.DataURL)     // data:image/png;base64,... → 前端 <img src>
    // st.QRBase64 是纯 base64（PNG 字节），走 JSON 给前端时用它
}
```

```go
// LoginStatus（字段与 /qr 的 JSON 完全一致，可直接 json.Marshal 给前端）
type LoginStatus struct {
    LoggedIn   bool       `json:"logged_in"`
    QRBase64   string     `json:"qr_base64,omitempty"`
    DataURL    string     `json:"data_url,omitempty"`
    CapturedAt *time.Time `json:"captured_at,omitempty"`
}
```

行为约定：

- **已登录** → `LoggedIn=true`，不带二维码（不会去截屏）；
- **未登录** → 返回可直接渲染的二维码，必要时现场抓取（含骨架屏自愈）；90 秒内的二维码走缓存，不会每次轮询都截屏；
- **首次调用**会自动 `Connect`（多花十来秒启动浏览器）；
- **并发**：如果另一个 goroutine 正在抓码（例如 `EnsureLogin` 在跑），本次**立即**返回空状态 `{}`，不阻塞，3 秒后重试即可；
- 浏览器启动失败后 10 秒内不会重复尝试（避免前端每 3 秒轮询把 CPU 打满）。

前端典型用法：每 3 秒调一次，`logged_in=false` 且有 `data_url` 就渲染，`logged_in=true` 就跳转。

> 二维码来自登录页元素 `#douyin_login_comp_scan_code_qrcode` 的**元素截图**（canvas 渲染，没有图片地址），
> 实测 180×180、清晰可扫；页面提示「已失效/点击刷新」时会自动点刷新，找不到按钮就整页重载重抓。
> 抓取轮询周期默认 30 秒（`Options.QRRefresh` 可改），二维码换了就再次回调 `OnQR`。

**v3.2 起加了「二维码自愈」**（解决「页面停在私信页骨架屏，前端一直显示『等待二维码』」）：

- 抓码改成**轮询**（每 600ms 探一次元素，最长等 30 秒），登录页渲染慢也能等到，不再一次性超时；
- **抓到的图要过图像校验**：统计深色像素占比，只画了 logo、格子还没渲染的「半成品」会被丢掉继续等
  （否则前端拿到一张空二维码，扫码毫无反应）；
- 页面既没有二维码、也没有登录浮层时，判定为「残留登录态卡死」，**自动清 cookie + localStorage/sessionStorage
  后重载回游客态**（实测清完 3~4 秒内抖音自己弹出登录框并渲染二维码）；
- 有登录 cookie 时先软重载（不清 cookie，避免误删有效登录态），确实失效了才清；
- `EnsureLogin` 抓码失败会重试 3 次；demo 进程也不因一次失败退出，**HTTP 二维码服务常驻**，10 秒后自动重试。

**Windows 启动自检**：从 Git Bash / 精简 PATH 的环境启动时 `PATH` 里没有 `System32`，
`exec.Command("powershell")` 会直接 "executable file not found"，导致残留浏览器杀不掉、
每次启动都报 `Failed to get the debug url`。现在改用 `%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe`
绝对路径，启动前会先清掉占着同一 profile 的旧 chrome/msedge（不会碰你自己的浏览器）。

> 触发场景很常见：容器/服务器上的 profile 里只剩 `ttwid`、`odin_tt` 这类匿名 cookie（没有 `sessionid`），
> 前端路由误以为已登录，于是停在骨架屏，既不给二维码也不给登录框 —— 旧版会直接报「没找到二维码元素」退出。


### 二次消息跟进：自动回复过一次之后，客户再来只截图不回复

规则：**每个会话只自动回复一次**。发出「晚点回复您」这类话术成功后，该会话进入观察名单；
之后客户再发消息，程序不再回复，而是把聊天页截图存下来（`BaseDir/shots/followup/`），
并通过 `Options.OnFollowUp` 回调通知你（可在回调里发企业微信/邮件/入库）。

```go
c, _ := douyinkit.New(douyinkit.Options{
    BaseDir: "/data/douyin",
    OnFollowUp: func(f douyinkit.FollowUp) {
        // f.Name 客户昵称、f.LastPath 截图路径、f.LastText 客户这条消息、f.Shots 第几次
        log.Printf("客户 %s 又来信了：%s（截图 %s）", f.Name, f.LastText, f.LastPath)
    },
})

// 人工接手后可解除观察，之后它再来消息会重新自动回复
c.Unwatch("某客户昵称")
for _, f := range c.FollowUps() { ... }   // 当前观察名单
```

行为细节：

- **只截一次**：同一条消息按文字去重，反复轮询不会刷出一堆相同截图；
- **截最新画面**：截图前先把消息区滚到底，保证客户刚发的那条在画面里，昵称也能看到（便于确认是谁）；
- **重启不丢**：观察名单持久化在 `state/followup.json`，进程重启后依旧只截图不回复；
- **干跑不落盘**：`RunOnce(true)` / `-send` 未开启时只在日志里提示，不写文件、不回复；
- **陌生人消息里也生效**：折叠在「陌生人消息」入口内的会话同样只截图不再回复；
- **名单会一直保留**（`followup_watch_hours: 0`）；想让它 N 小时后恢复自动回复，把该值改成小时数即可。

配置项（`BaseDir/config.json`，缺省即下列值）：

```json
{
  "followup_screenshot": true,   // 客户二次消息自动截图
  "followup_no_reply": true,     // 二次消息不再自动回复
  "followup_watch_hours": 0      // 观察期（小时），0 = 永久
}
```

`FollowUp` 结构：

```go
type FollowUp struct {
    Name       string     `json:"name"`         // 会话昵称
    ReplyText  string     `json:"reply_text"`   // 当时自动回复的内容
    RepliedAt  time.Time  `json:"replied_at"`   // 自动回复时间
    Shots      int        `json:"shots"`        // 已截图次数
    LastText   string     `json:"last_text"`    // 最近一次截图对应的客户消息
    LastPath   string     `json:"last_path"`    // 最近一次截图文件
    LastShotAt *time.Time `json:"last_shot_at"`
}
```

> 截图是 1440×900 的整页视口截图（含会话列表 + 聊天区 + 顶部昵称），不做裁剪，
> 方便人工判断上下文；如果只要聊天区，可在 `OnFollowUp` 里按 `LastPath` 再裁。

### 关于点击方式（Linux 服务器必读）

v1 用「JS 取坐标 → CDP 鼠标点击」，在部分 Linux/容器环境里会整体偏移：日志看着点了，实际什么都没点中，
表现为**会话打不开、发送按钮点不到、截图文件名和内容对不上**。

v2 起改成 **DOM 标记 + 元素点击**：先给目标元素打 `data-dy-target` 之类的标记，
再用 `Element.Click()`（浏览器自己 `scrollIntoView` 并用自己的坐标派发真实鼠标事件），Go 侧不再做任何坐标运算。
覆盖：会话行、输入框、发送按钮、陌生人消息入口、返回按钮。坐标点击只在 class 改名、标记失败时兜底。

v3 起再加一层：**命中点探测 + 多策略点击**。有些容器里浏览器对 CDP 派发的输入事件整条不响应
（现象很典型：`InsertText` 能把字写进输入框，但鼠标点击、回车都不生效），只靠一种点击方式必然失败。
现在的顺序是：

| 顺序 | 方式 | 是否依赖 CDP 输入事件 |
|---|---|---|
| ① | 先用 `elementFromPoint` 在会话行内多点探测，找到「确实能命中这一行」的坐标，再发真实鼠标事件 | 是 |
| ② | 元素点击 `Element.Click()`（自带滚动到可见） | 是 |
| ③ | 在命中点上派发完整指针序列 `pointerdown→mousedown→pointerup→mouseup→click` | 否（纯 JS） |
| ④ | 原生 `el.click()` | 否（纯 JS） |

每一步都验证「右侧是否真的切过去了」，判据取并：**标题栏出现会话名** 或 **列表激活行就是它**
（后者不依赖标题栏选择器，改版也不会误判）。一旦发现 ①② 都失败、③ 成功，会记住这个环境，
后续直接走 JS 派发并在日志提示「本环境不接收原生鼠标事件」，不再逐个白等超时。

发送同样多级兜底：CDP 回车 → JS 派发 Enter → 点发送按钮元素 → 原生 `click()` → 指针序列。

打开失败时日志会打印**结构自证**，直接指出是「点不中」还是「点了没反应」：

```
第 1 次打开失败（方式=exact，标题栏="" 激活行="" 气泡=0），重试
结构诊断：<DIV> rect=[0 68 258 64] 命中点=(116,100) 命中元素=DIV.xxx 挡住它的是=DIV.maskYyy
结构诊断：class=conversationConversationItemwrapper ...
结构诊断：pointer-events=auto display=flex opacity=1 子节点=DIV.xxx > DIV.yyy
```

- `命中元素` 为空、`挡住它的是=xxx` → 有浮层/遮罩盖在列表上，把那行的遮挡物名字发出来即可定位
- `命中元素` 正常但三种点击都无效 → 页面把事件吞了，看是否 `pointer-events` 异常或该行是链接（日志会打 `href`）

另：发送前会清空输入框残留（避免上次失败的文字累积成「晚点回复您晚点回复您」）。

---

## 2. 演示程序（带 HTTP 二维码服务）

```bash
# Linux 服务器（无头，默认）
./douyinkit-demo-linux-amd64 -base /var/lib/douyin -addr :6060

# 本地调试（弹浏览器窗口）
go run ./cmd/demo -no-headless -base ./testdata -addr :6060

# 持续自动回复（真实发送）
./douyinkit-demo-linux-amd64 -send -loop -base /var/lib/douyin

# 只测一个会话（上线前小范围验证发送链路，不碰其他会话）
./douyinkit-demo-linux-amd64 -base /var/lib/douyin -to "昵称"          # 干跑，不发送
./douyinkit-demo-linux-amd64 -base /var/lib/douyin -to "昵称" -send     # 真发一条
```

内置接口：

| 路由 | 返回 |
|---|---|
| `GET /qr` | `{"logged_in":false,"qr_base64":"iVBORw0...","data_url":"data:image/png;base64,...","captured_at":"..."}` |
| `GET /` | 演示页：自动每 3 秒刷新二维码，登录成功后显示「✅ 登录成功」（前端接入参考实现） |

前端最简接法：

```html
<img id="qr" />
<script>
setInterval(async () => {
  const j = await (await fetch('/qr')).json();
  if (j.data_url) qr.src = j.data_url;     // 直接渲染 base64
  if (j.logged_in) location.reload();
}, 3000);
</script>
```

### 2.1 独立小工具（每个方法一个 demo，可单独跑）

每个工具只干一件事，都支持统一基础参数 `-base / -account / -headless / -channel`：

| 工具 | 作用 | 典型用法 |
|---|---|---|
| `cmd/login` | **只登录**：出二维码等扫码，成功即退出 | `go run ./cmd/login`；无头用 `go run ./cmd/login -headless yes` 然后打开 `qr.html` 扫 |
| `cmd/qr` | **只取二维码/状态**：一次输出 JSON 或起 HTTP 接口 | `go run ./cmd/qr`；常驻接口 `go run ./cmd/qr -serve :6060` |
| `cmd/reply` | **只自动回复**：一轮/持续/定向，不含二维码页面 | `go run ./cmd/reply -send -loop`；干跑去掉 `-send` |
| `cmd/autoreply` | **只设置自动回复开关**：改 config.json，不启动浏览器 | `go run ./cmd/autoreply -off`（关）/ `-on`（开）/ 不带参查看 |
| `cmd/followups` | **只看/管理二次消息观察名单**：不启动浏览器 | `go run ./cmd/followups`；解除观察 `go run ./cmd/followups -unwatch 昵称` |
| `cmd/gate` | **是否启动 Run 的开关演示**：闸门关不轮询，运行中可开/关/启/停 | `go run ./cmd/gate -no-reply`；接口 `/status` `/toggle` `/start` `/stop` `/qr` |
| `cmd/preheat` | **只预热（常驻模式）**：后台启动浏览器+预取二维码，进程保活 | `go run ./cmd/preheat`；带观察接口 `go run ./cmd/preheat -addr :6063` |

多账号统一加 `-account 账号名`（数据目录变为 `<base>/<账号名>`），例如：

```bash
go run ./cmd/login -account 店铺A            # 给店铺A扫码
go run ./cmd/autoreply -account 店铺B -off   # 关掉店铺B的自动回复
go run ./cmd/followups -account 店铺A        # 看店铺A的观察名单
```

对应库方法：login→`Connect/EnsureLogin`，qr→`QRStatus`，reply→`RunOnce/Run/SendTo`，
autoreply→`Options.AutoReply/Config.AutoReply`（运行中切换用 `SetAutoReply`），
followups→`FollowUps/Unwatch`，gate→`Run` 的启动时机 + `SetAutoReply` 运行中切换，
preheat→`Client.Preheat/Manager.Preheat/Manager.PreheatAll`。

### Preheat：常驻 + 预热（服务器扫码慢的方案 B）

原理：把「浏览器冷启动 + 登录页加载 + 风控挑战」提前到用户扫码**之前**，
用户要扫码时二维码已经抓好，秒出。已登录账号浏览器常驻内存，不再反复冷启动。

```go
// 单账号：创建后预热一行搞定（异步，失败不影响主流程）
c, _ := douyinkit.New(opt)
c.Preheat()

// 多账号：模板开 Preheat，新账号创建即自动后台预热
m, _ := douyinkit.NewManager("/data/douyin", douyinkit.Options{Preheat: true})
c1, _ := m.Account("shop1")   // 创建即预热
m.Preheat("shop1")            // 也可手动预热 / m.PreheatAll() 全量预热
```

命令行加 `-preheat` 即可（`cmd/demo` / `cmd/gate`），不加则行为与之前完全一致：

```bash
./douyinkit-demo-linux-amd64 -preheat -send -loop -base /var/lib/douyin
```

注意：每个常驻浏览器约占 300~500MB 内存；不活跃账号可 `Close()` 释放，
登录态在磁盘 profile 里，下次预热/使用免扫码。


---

## 3. Docker 部署（推荐，规避 glibc 过旧）

在 CentOS 7 等老系统（glibc 2.17）上直接跑，rod 自动下载的新版 Chromium 会报
`GLIBC_2.18/2.25 not found`。用 Docker 把运行环境（Alpine/musl + 预装 Chromium）
一起打包，容器自包含、不依赖宿主机 glibc，宿主机只要有 Docker 即可，**无需升级 glibc**。

> 本镜像基于内网 harbor（`harbor.chaoniu.top:4433/sushou/alpine-chromium`），拉取快。
> 首次构建前确保构建机能访问并登录该 harbor：`docker login harbor.chaoniu.top:4433`。

```bash
# 构建 + 启动（首次会拉基础镜像、编译，需几分钟）
docker compose up -d --build

# 看日志（二维码抓取、扫码、回复都在这里）
docker compose logs -f

# 扫码登录：浏览器打开 http://服务器IP:6060/ ，用抖音 APP 扫一扫
# 登录态持久化在宿主机 ./douyinkit-data/session/profile，重建容器不丢

# 停止 / 重启
docker compose down
docker compose restart
```

不用 compose 的原生命令：

```bash
docker build -t douyinkit:latest .
docker run -d --name douyin-assistant \
  --shm-size=2g \
  -e TZ=Asia/Shanghai \
  -p 6060:6060 \
  -v $(pwd)/douyinkit-data:/app/douyinkit-data \
  --restart unless-stopped \
  douyinkit:latest
```

要点：
- **`--shm-size=2g` 必加**：Chromium 默认 `/dev/shm` 只有 64MB，页面渲染会崩溃。
- **时区**：镜像内已设 `Asia/Shanghai`，免打扰时段/消息时间判断才准确。
- **中文字体**：镜像已装 `font-noto-cjk`（Alpine 包名），否则中文昵称/消息显示成方块，影响识别。
- **先干跑**：想只观察不真实发送，编辑 `docker-compose.yml` 把 `command` 里的
  `-send`、`-loop` 去掉，再 `docker compose up -d`。
- 程序用 `-channel chromium` 走容器内 `/usr/bin/chromium`，不再下载 rod 内核。

---

## 4. Linux 裸机部署

> ⚠️ 仅适用于 glibc ≥ 2.28 的系统（Ubuntu 20.04+/Debian 11+/CentOS Stream 9+）。
> CentOS 7 等老系统请用上面的 Docker 方案。

```bash
# 1) 装 Chromium（Debian/Ubuntu）
apt-get update && apt-get install -y chromium fonts-noto-cjk
# CentOS/RHEL: yum install -y chromium google-noto-sans-cjk-fonts
# fonts-noto-cjk 必装：否则中文昵称/消息会显示成方块，影响识别

# 2) 拷贝二进制
scp dist/douyinkit-demo-linux-amd64 root@server:/opt/douyin/douyinkit-demo
ssh root@server 'chmod +x /opt/douyin/douyinkit-demo'

# 3) 首次运行 → 控制台/接口会给出二维码，扫码后登录态落在 /var/lib/douyin/session/profile
/opt/douyin/douyinkit-demo -base /var/lib/douyin -addr :6060
```

`/etc/systemd/system/douyin-bot.service`：

```ini
[Unit]
Description=Douyin private message bot
After=network-online.target

[Service]
Type=simple
ExecStart=/opt/douyin/douyinkit-demo -base /var/lib/douyin -addr :6060 -send -loop
Restart=always
RestartSec=10
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now douyin-bot
journalctl -u douyin-bot -f          # 看日志（同时也会写 /var/lib/douyin/logs/日期.log）
```

> **安全**：`/qr` 接口不带鉴权，等于把登录入口暴露出去。建议只监听内网
> （`-addr 127.0.0.1:6060` + Nginx 反代加鉴权），或用防火墙限制来源 IP。
> 登录态 `session/profile` 等同账号凭证，别外传、别提交到仓库。

---

## 5. 交叉编译（在 Windows/macOS 上出 Linux 包）

```bash
GOOS=linux GOARCH=amd64 go build -o dist/douyinkit-demo-linux-amd64 ./cmd/demo
# ARM 服务器（如 Oracle/阿里云 ARM 实例）
GOOS=linux GOARCH=arm64 go build -o dist/douyinkit-demo-linux-arm64 ./cmd/demo
```

依赖（go-rod）已在本机模块缓存；换新机器首次构建需要：
`set GOPROXY=https://goproxy.cn,direct`（Windows）或 `export GOPROXY=...`（Linux）。

---

## 6. 数据目录

```
<BaseDir>/
  config.json            配置文件（不存在则用默认值，可随时手改，重启生效）
  session/profile/       浏览器登录态（= 账号凭证，别外传）
  state/replied.json     冷却记录（昵称 → 上次回复时间），与 Windows/Python 版格式互通
  state/followup.json    二次消息观察名单（已回复过的会话、截图次数、最近截图）
  logs/2026-11-11.log    运行日志（每个跳过原因都在这里）
  shots/                 干跑/异常截图
  shots/followup/        客户二次消息截图（每个客户按时间命名，人工跟进用）
```

`config.json` 字段与 Windows 版一致：`reply_text`、`same_user_cooldown_hours`、
`max_replies_per_hour`、`poll_interval_seconds`、`quiet_hours`、`skip_if_last_msg_is_mine`、
`typing_delay_seconds`、`reply_groups`、`check_recent_convs`；
新增 `followup_screenshot`、`followup_no_reply`、`followup_watch_hours`（见上一节）。

---

## 7. 常见问题

**Q：启动浏览器报 `GLIBC_2.18 / GLIBC_2.25 not found`？**
A：宿主机 glibc 太旧（CentOS 7 = glibc 2.17），跑不动 rod 自动下载的新版 Chromium。
首选第 3 节的 **Docker 方案**；或把系统升级到 glibc ≥ 2.28（Ubuntu 20.04+/Debian 11+）。

**Q：无头模式下抓不到二维码 / 页面是空白？**
A：确认能访问 `douyin.com`（服务器出海网络受限时需配 proxy）；先看 `BaseDir/logs` 里的日志：
- 出现「页面不是登录页（疑似残留登录态/骨架屏），清理 cookie 后重载」→ 正常自愈流程，几秒后会出码；
- 一直是「没找到二维码元素」→ 看 `BaseDir/shots/qr_fail.png` 截图，通常是网络没通或被风控拦到验证页；
- 想对照页面结构变化，可本地 `-no-headless` 跑一次看。

**Q：前端页面一直显示「等待二维码」/ 图片加载失败？**
A：三种情况：
1. **进程已退出** —— 页面上的 fetch 全部失败（打开 F12 看 `/qr` 请求报错）。本版 demo 已改成登录失败不退出，会自动重试；
2. `/qr` 返回 `{"logged_in":false}` 且没有 `data_url` —— 正在抓码/自愈中，logs 里能看到进度，通常 3~10 秒出图；
3. 服务不在 6060 —— 确认 `-addr`，容器里要 `-addr :6060` 并映射端口。

自查命令：

```bash
curl -s http://127.0.0.1:6060/qr | head -c 200     # 有 data_url 说明正常
```


**Q：抖音风控识别出自动化？**
A：本版用 `--headless=new`（更接近真实 Chrome）+ 关闭自动化特征。
仍被拦时改用虚拟显示：

```bash
apt-get install -y xvfb
xvfb-run -a --server-args="-screen 0 1440x900x24" ./douyinkit-demo -base /var/lib/douyin
```

**Q：扫码后一直转圈？**
A：`EnsureLogin` 5 秒轮询一次登录 cookie；若长时间不成功，看日志是否停在登录框。
二维码有效期约 2 分钟，超时会自动换新码（`OnQR` 会再次回调），前端重新渲染即可。

**Q：想定向回复某个会话？**
A：库里用 `RunOnce` 走全量扫描；定向回复请用 Windows 版的 `-to`，或基于本库自行扩展
（`replyToNamed` 已保留在包内，未导出）。
