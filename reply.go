package douyinkit

// 回复主逻辑 —— 复刻 Python 版 dy_reply.py 的 scan / is_recent / looks_like_group /
// reply_one / open_stranger_folder / back_from_folder / process_list / reply_to_named / run_once

import (
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ysmood/gson"
)

// ---------------------------------------------------------------- JS 返回结构

type ConvItem struct {
	CX      int    `json:"cx"`
	CY      int    `json:"cy"`
	Y       int    `json:"y"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Preview string `json:"preview"`
	Raw     string `json:"raw"`
	Unread  bool   `json:"unread"`
	Badge   string `json:"badge"`
	Active  bool   `json:"active"`
}

type HeaderInfo struct {
	Text    string `json:"text"`
	IsGroup bool   `json:"isGroup"`
}

type LastMsgInfo struct {
	Text string `json:"text"`
	Side string `json:"side"`
}

type InputPos struct {
	CX int `json:"cx"`
	CY int `json:"cy"`
	W  int `json:"w"`
	H  int `json:"h"`
}

type InputFocus struct {
	Focused  bool `json:"focused"`
	Editable bool `json:"editable"`
	Disabled bool `json:"disabled"`
}

type BtnPos struct {
	CX   int    `json:"cx"`
	CY   int    `json:"cy"`
	Text string `json:"text"`
}

// ---------------------------------------------------------------- Eval 辅助

func evalRaw(page *rod.Page, js string) (*gson.JSON, bool) {
	res, err := page.Eval(js)
	if err != nil || res == nil {
		return nil, false
	}
	return &res.Value, true
}

// scan 对应 Python scan(page)
func evalConvList(page *rod.Page) []ConvItem {
	v, ok := evalRaw(page, LIST_CONV_JS)
	if !ok {
		return nil
	}
	var out []ConvItem
	if v.Unmarshal(&out) != nil {
		return nil
	}
	return out
}

func evalHeader(page *rod.Page) *HeaderInfo {
	v, ok := evalRaw(page, HEADER_JS)
	if !ok {
		return nil
	}
	var h HeaderInfo
	if v.Unmarshal(&h) != nil {
		return nil
	}
	return &h
}

func evalLastMsg(page *rod.Page) *LastMsgInfo {
	v, ok := evalRaw(page, LAST_MSG_JS)
	if !ok {
		return nil
	}
	var m LastMsgInfo
	if v.Unmarshal(&m) != nil {
		return nil
	}
	return &m
}

func evalInput(page *rod.Page) *InputPos {
	v, ok := evalRaw(page, INPUT_JS)
	if !ok {
		return nil
	}
	var p InputPos
	if v.Unmarshal(&p) != nil {
		return nil
	}
	return &p
}

// evalInputText 对应 Python 读 INPUT_TEXT_JS（返回 "" 表示拿不到）
func evalInputText(page *rod.Page) string {
	v, ok := evalRaw(page, INPUT_TEXT_JS)
	if !ok {
		return ""
	}
	var s string
	if v.Unmarshal(&s) != nil {
		return ""
	}
	return s
}

// evalFocusInput 主动聚焦输入框并回读其可编辑/禁用状态（诊断“写不进去”用）
func evalFocusInput(page *rod.Page) *InputFocus {
	v, ok := evalRaw(page, FOCUS_INPUT_JS)
	if !ok {
		return nil
	}
	var f InputFocus
	if v.Unmarshal(&f) != nil {
		return nil
	}
	return &f
}

// waitInputReady 轮询等待聊天面板加载完成、输入框真正可编辑。
// 截图证实：点击会话后右侧可能长时间处于 loading，此时输入框未渲染，
// 固定等待会扑空。最多等 maxWait，返回最后一次探测状态。
func waitInputReady(page *rod.Page, maxWait time.Duration) *InputFocus {
	deadline := time.Now().Add(maxWait)
	var last *InputFocus
	for {
		last = evalFocusInput(page)
		if last != nil && last.Editable && !last.Disabled {
			return last
		}
		if time.Now().After(deadline) {
			return last
		}
		wait(500)
	}
}

func evalBackBtn(page *rod.Page) *BtnPos {
	v, ok := evalRaw(page, BACK_JS)
	if !ok {
		return nil
	}
	var b BtnPos
	if v.Unmarshal(&b) != nil {
		return nil
	}
	return &b
}

func evalSendBtn(page *rod.Page) *BtnPos {
	v, ok := evalRaw(page, SEND_BTN_JS)
	if !ok {
		return nil
	}
	var b BtnPos
	if v.Unmarshal(&b) != nil {
		return nil
	}
	return &b
}

// ---------------------------------------------------------------- DOM 点击（不依赖坐标）
//
// 服务器 headless 环境里「JS 坐标」和「CDP 鼠标落点」可能对不上，导致点了没反应/点错会话。
// 这里统一改成：JS 打 data 标记 → 元素级 Click（rod 内部会 scrollIntoView 并用自己的坐标点击）。

func evalArgs(page *rod.Page, js string, args ...interface{}) (*gson.JSON, bool) {
	res, err := page.Eval(js, args...)
	if err != nil || res == nil {
		return nil, false
	}
	return &res.Value, true
}

// markConv 按名字给会话项打标记，返回 exact / contains / none
func markConv(page *rod.Page, name string) string {
	v, ok := evalArgs(page, MARK_CONV_JS, name)
	if !ok {
		return "none"
	}
	return v.Str()
}

// markInput 给可编辑输入框打标记
func markInput(page *rod.Page) bool {
	v, ok := evalRaw(page, MARK_INPUT_JS)
	return ok && v.Bool()
}

// clickMarked 点击已打标记的元素（真实鼠标事件）；失败返回 false
func clickMarked(page *rod.Page, sel string) bool {
	el, err := page.Timeout(3 * time.Second).Element(sel)
	if err != nil || el == nil {
		return false
	}
	_ = el.ScrollIntoView()
	return el.Click(proto.InputMouseButtonLeft, 1) == nil
}

// jsClickMarked 对已打标记元素派发原生 click()（应对只监听 click 的组件）
func jsClickMarked(page *rod.Page, sel string) bool {
	v, ok := evalArgs(page, JS_CLICK_JS, sel)
	return ok && v.Bool()
}

// headerHas 轮询检查右侧标题栏是否已经切到目标会话
func headerHas(page *rod.Page, name string, maxWait time.Duration) bool {
	deadline := time.Now().Add(maxWait)
	for {
		if hdr := evalHeader(page); hdr != nil && strings.Contains(hdr.Text, name) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		wait(400)
	}
}

func currentHeader(page *rod.Page) string {
	if hdr := evalHeader(page); hdr != nil {
		return hdr.Text
	}
	return ""
}

// logMetrics 打印视口信息，用于排查「点击落空」类问题
func logMetrics(page *rod.Page, lg *Logger) {
	if v, ok := evalRaw(page, METRICS_JS); ok && v != nil {
		lg.Log("    视口：%dx%d dpr=%.2f scrollY=%d 内容高=%d",
			v.Get("w").Int(), v.Get("h").Int(), v.Get("dpr").Num(),
			v.Get("sy").Int(), v.Get("sh").Int())
	}
}

// nativeInputDead 记录「本环境里 CDP 原生输入事件是否被吞掉」。
// 容器/无头环境实测：InsertText 能写入，但鼠标点击、回车都不生效（事件被吞）。
// 一旦发现这种情况，后续直接走 JS 派发，不再逐个白等超时。
var nativeInputDead bool

// ---------------------------------------------------------------- 打开会话（命中点感知）

type RowPoint struct {
	Ok         bool   `json:"ok"`
	Why        string `json:"why"`
	Rows       int    `json:"rows"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Hit        string `json:"hit"`
	Covered    string `json:"covered"`
	Rect       []int  `json:"rect"`
	RowTag     string `json:"rowTag"`
	RowClass   string `json:"rowClass"`
	RowPE      string `json:"rowPE"`
	RowDisplay string `json:"rowDisplay"`
	RowOpacity string `json:"rowOpacity"`
	Href       string `json:"href"`
	Child0     string `json:"child0"`
	Child1     string `json:"child1"`
}

type ChatState struct {
	Header    string `json:"header"`
	Active    string `json:"active"`
	Rows      int    `json:"rows"`
	Bubbles   int    `json:"bubbles"`
	HasEditor bool   `json:"hasEditor"`
}

func evalRowPoint(page *rod.Page, name string) *RowPoint {
	v, ok := evalArgs(page, ROW_POINT_JS, name)
	if !ok {
		return nil
	}
	var p RowPoint
	if v.Unmarshal(&p) != nil {
		return nil
	}
	return &p
}

func evalChatState(page *rod.Page) *ChatState {
	v, ok := evalRaw(page, CHAT_STATE_JS)
	if !ok {
		return nil
	}
	var st ChatState
	if v.Unmarshal(&st) != nil {
		return nil
	}
	return &st
}

// openedNow 右侧是否已切到目标会话。
// 两个判据取并：标题栏包含会话名，或列表里「激活行」就是它——
// 后者不依赖标题栏选择器，选择器改名时也不会误判成「没打开」。
func openedNow(st *ChatState, name string) bool {
	if st == nil {
		return false
	}
	if st.Header != "" && strings.Contains(st.Header, name) {
		return true
	}
	if st.Active != "" && (st.Active == name || strings.Contains(st.Active, name) || strings.Contains(name, st.Active)) {
		return true
	}
	return false
}

func waitOpened(page *rod.Page, name string, maxWait time.Duration) (*ChatState, bool) {
	deadline := time.Now().Add(maxWait)
	for {
		st := evalChatState(page)
		if openedNow(st, name) {
			return st, true
		}
		if time.Now().After(deadline) {
			return st, false
		}
		wait(300)
	}
}

// waitChatPanel 会话已切过去，但右侧消息可能还在加载：等到出现气泡或输入框再返回
func waitChatPanel(page *rod.Page, st *ChatState, lg *Logger) *ChatState {
	if st != nil && (st.Bubbles > 0 || st.HasEditor) {
		return st
	}
	deadline := time.Now().Add(12 * time.Second)
	var last = st
	for {
		last = evalChatState(page)
		if last != nil && (last.Bubbles > 0 || last.HasEditor) {
			return last
		}
		if time.Now().After(deadline) {
			lg.Log("    右侧 12 秒内没加载出消息（气泡=0、无输入框），按实际情况继续")
			return last
		}
		wait(400)
	}
}

// jsPointerClickEl 对指定元素派发完整指针序列（先取元素中心再在命中点上点击）
func jsPointerClickEl(page *rod.Page, sel string) bool {
	v, ok := evalArgs(page, ELEM_CENTER_JS, sel)
	if !ok || v.Nil() {
		return false
	}
	return jsPointerClick(page, v.Get("x").Int(), v.Get("y").Int())
}

func jsEnter(page *rod.Page, sel string) bool {
	v, ok := evalArgs(page, JS_ENTER_JS, sel)
	return ok && v.Bool()
}

func jsPointerClick(page *rod.Page, x, y int) bool {
	v, ok := evalArgs(page, JS_POINTER_CLICK_JS, x, y)
	if !ok {
		return false
	}
	return v.Get("ok").Bool()
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// logRowDiag 打开失败时打印结构自证：到底是「点不中（被谁挡住）」还是「点了没反应」
func logRowDiag(page *rod.Page, name string, lg *Logger) {
	p := evalRowPoint(page, name)
	if p == nil {
		lg.Log("    结构诊断：拿不到目标行信息")
		return
	}
	lg.Log("    结构诊断：<%s> rect=%v 命中点=(%d,%d) 命中元素=%s 挡住它的是=%s",
		p.RowTag, p.Rect, p.X, p.Y, orNone(p.Hit), orNone(p.Covered))
	lg.Log("    结构诊断：class=%s", truncate(p.RowClass, 100))
	lg.Log("    结构诊断：pointer-events=%s display=%s opacity=%s 子节点=%s > %s",
		p.RowPE, p.RowDisplay, p.RowOpacity, orNone(p.Child0), orNone(p.Child1))
	if p.Href != "" {
		lg.Log("    结构诊断：该行是链接 href=%s", truncate(p.Href, 90))
	}
}

// openConversation 打开指定会话。返回 (是否打开, 失败原因)。
//
// 服务器（headless/容器）上曾出现「怎么点都不切换会话」：JS 算出的坐标与 CDP 鼠标事件
// 对不上，所有点击都落空。因此这里不再信任任何单一方式，按可靠性依次尝试，每步都用
// 「右侧是否真的切过去了」验证：
//  1. 命中点点击：先用 elementFromPoint 找到「确实能点中这一行」的坐标，再发真实鼠标事件；
//  2. 元素点击：rod 自带 scrollIntoView + 元素中心点击；
//  3. 指针序列：在命中点上派发 pointerdown/mousedown/.../click（应对只监听按下事件的组件）；
//  4. 原生 click()：直接 el.click()。
func openConversation(page *rod.Page, name string, lg *Logger) (bool, string) {
	inList := false
	for _, c := range evalConvList(page) {
		if c.Kind != "stranger_folder" && c.Name == name {
			inList = true
			break
		}
	}
	if !inList {
		return false, "会话不在列表可视区"
	}

	for attempt := 1; attempt <= 3; attempt++ {
		mode := markConv(page, name)
		pt := evalRowPoint(page, name)
		if (pt == nil || !pt.Ok) && mode == "none" {
			return false, "会话不在列表可视区"
		}

		nativeTried := false
		if !nativeInputDead {
			// ① 命中点 + 真实鼠标事件
			if pt != nil && pt.Ok {
				nativeTried = true
				mouseClick(page, float64(pt.X), float64(pt.Y))
				if st, ok := waitOpened(page, name, 8*time.Second); ok {
					waitChatPanel(page, st, lg)
					return true, ""
				}
			}
			// ② 元素点击
			if mode != "none" && clickMarked(page, `[data-dy-target="1"]`) {
				nativeTried = true
				if st, ok := waitOpened(page, name, 5*time.Second); ok {
					waitChatPanel(page, st, lg)
					return true, ""
				}
			}
		}
		// ③ 命中点上的完整指针序列（纯 JS，不依赖 CDP 输入事件）
		if pt != nil {
			if jsPointerClick(page, pt.X, pt.Y) {
				if st, ok := waitOpened(page, name, 6*time.Second); ok {
					if nativeTried {
						nativeInputDead = true
						lg.Log("    提示：本环境不接收原生鼠标事件，后续改用 JS 派发（点击依旧生效）")
					}
					waitChatPanel(page, st, lg)
					return true, ""
				}
			}
		}
		// ④ 原生 click()
		if mode != "none" && jsClickMarked(page, `[data-dy-target="1"]`) {
			if st, ok := waitOpened(page, name, 4*time.Second); ok {
				waitChatPanel(page, st, lg)
				return true, ""
			}
		}

		if attempt < 3 {
			st := evalChatState(page)
			lg.Log("  第 %d 次打开失败（方式=%s，标题栏=%q 激活行=%q 气泡=%d），重试",
				attempt, mode, truncate(stateHeader(st), 18), truncate(stateActive(st), 18), stateBubbles(st))
			logRowDiag(page, name, lg)
			logMetrics(page, lg)
			wait(800)
		}
	}
	return false, "点击未选中目标会话"
}

func stateHeader(st *ChatState) string {
	if st == nil {
		return ""
	}
	return st.Header
}

func stateActive(st *ChatState) string {
	if st == nil {
		return ""
	}
	return st.Active
}

func stateBubbles(st *ChatState) int {
	if st == nil {
		return 0
	}
	return st.Bubbles
}

// ---------------------------------------------------------------- 判定逻辑

var groupHints = []string{"加入了群聊", "新成员可查看历史消息", "群公告", "群主", "管理员"}

// Python: re.compile(r"(刚刚|\b([1-9]|[1-5]\d)\s*分钟前)") —— 只有很近的会话才主动核对
var recentRe = regexp.MustCompile(`刚刚|([1-9]|[1-5]\d)\s*分钟前`)

func isRecent(c ConvItem) bool {
	blob := c.Preview + " " + c.Raw
	return recentRe.MatchString(blob)
}

// looksLikeGroup：在列表层面就判断是不是群聊，避免点开（点开会清掉未读、也会打扰群里）
func looksLikeGroup(c ConvItem) bool {
	if groupTailRe.MatchString(c.Name) {
		return true
	}
	blob := c.Name + " " + c.Preview
	for _, k := range groupHints {
		if strings.Contains(blob, k) {
			return true
		}
	}
	return false
}

// freshConvPos 按名字重新查一次列表，拿会话的当前坐标。
// 扫描时的坐标可能因列表重排/刷新而失效（点击落空、会话没选中），点击前必须取新坐标。
func freshConvPos(page *rod.Page, name string) *ConvItem {
	for _, c := range evalConvList(page) {
		if c.Kind != "stranger_folder" && c.Name == name {
			cc := c
			return &cc
		}
	}
	return nil
}

var groupTailRe = regexp.MustCompile(`\(\d+\)\s*$`)

// ---------------------------------------------------------------- 工具

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func randFloat(a, b float64) float64 { return a + rand.Float64()*(b-a) }
func randInt(a, b int) int           { return a + rand.Intn(b-a+1) }

func typingDelayMs(cfg *Config) int {
	a, b := 1.5, 3.5
	if len(cfg.TypingDelaySeconds) == 2 {
		a, b = cfg.TypingDelaySeconds[0], cfg.TypingDelaySeconds[1]
	}
	return int(randFloat(a, b) * 1000)
}

var shotNameRe = regexp.MustCompile(`[^\w\x{4e00}-\x{9fa5}-]`)

func dryShotPath(base, name string) string {
	safe := shotNameRe.ReplaceAllString(name, "_")
	if s := []rune(safe); len(s) > 16 {
		safe = string(s[:16])
	}
	if safe == "" {
		safe = "conv"
	}
	return filepathJoin(base, "shots", fmt.Sprintf("dry_%s_%s.png", safe, time.Now().Format("20060102_150405")))
}

// failShot 只要没成功发出消息（失败或跳过）就截图留证，返回截图文件名供日志打印
func failShot(page *rod.Page, base, name, tag string) string {
	shot := dryShotPath(base, name+"_"+tag)
	saveShot(page, shot)
	return baseName(shot)
}

// ---------------------------------------------------------------- 核心流程

// replyOne 复刻 Python reply_one(page, conv, cfg, dry, hist) -> str
func replyOne(page *rod.Page, conv ConvItem, cfg *Config, dry bool, hist map[string]string, lg *Logger, base string, fs *followStore) string {
	name := conv.Name
	// 打开会话：DOM 标记 + 元素点击（浏览器自己算坐标并滚动到可见），
	// 不再依赖 JS 计算出的坐标——服务器 headless 环境下那套坐标会整体偏移，点了没反应/点错会话
	opened, why := openConversation(page, name, lg)
	if !opened {
		if why == "会话不在列表可视区" {
			return fmt.Sprintf("会话不在列表可视区，跳过（未点击，截图 %s）", failShot(page, base, name, "notinline"))
		}
		return fmt.Sprintf("未打开会话（%s，右侧实际是「%s」，截图 %s），跳过",
			why, truncate(currentHeader(page), 18), failShot(page, base, name, "openfail"))
	}
	// 会话已打开，等输入框渲染就绪（企业号等特殊会话可能没有标准输入框，后面 evalInput 会报「没找到输入框」）
	waitInputReady(page, 6*time.Second)

	// 安全闸：群聊一律不自动回复（会把消息发到所有成员面前）
	if hdr := evalHeader(page); hdr != nil && hdr.IsGroup && !cfg.ReplyGroups {
		return fmt.Sprintf("跳过群聊（%s，截图 %s）", truncate(hdr.Text, 22), failShot(page, base, name, "group"))
	}

	// 判据：最后一条是不是对方发的。是 → 回；否 → 已在等对方，跳过
	if last := evalLastMsg(page); last != nil && last.Side == "right" && cfg.SkipIfLastMsgIsMine {
		return fmt.Sprintf("最后一条是我方发言，跳过（%q，截图 %s）", truncate(last.Text, 16), failShot(page, base, name, "lastmine"))
	}

	pos := evalInput(page)
	if pos == nil {
		return fmt.Sprintf("没找到输入框，跳过（截图 %s）", failShot(page, base, name, "noinput"))
	}

	text := strings.TrimSpace(cfg.ReplyText)
	if text == "" {
		return "reply_text 为空，跳过"
	}

	if dry {
		// 干跑时留一张聊天区截图，方便核对结构（输入框/发送按钮位置）
		shot := dryShotPath(base, name)
		saveShot(page, shot)
		lg.Log("    截图：%s", baseName(shot))
		return fmt.Sprintf("[干跑] 会回复「%s」", text)
	}

	// 打字前随机停顿，模拟真人
	wait(typingDelayMs(cfg))

	// 聚焦输入框：DOM 标记 + Focus + 元素点击（服务器上坐标点击点不中，必须走元素）
	if !markInput(page) {
		return fmt.Sprintf("没找到可编辑输入框，跳过（截图 %s）", failShot(page, base, name, "noinput"))
	}
	inEl, inErr := page.Timeout(3 * time.Second).Element(`[data-dy-input="1"]`)
	if inErr != nil || inEl == nil {
		return fmt.Sprintf("输入框定位失败，跳过（截图 %s）", failShot(page, base, name, "noinput"))
	}
	_ = inEl.ScrollIntoView()
	_ = inEl.Focus()
	_ = inEl.Click(proto.InputMouseButtonLeft, 1)
	wait(400)
	focus := evalFocusInput(page)

	// 清空残留：上一次发送失败会留下旧文案，不清空会越积越多
	// （服务器上曾出现输入框里「晚点回复您」重复三遍）
	if evalInputText(page) != "" {
		_ = inEl.SelectAllText()
		_ = page.Keyboard.Type(input.Backspace)
		wait(300)
		lg.Log("    已清空输入框残留内容")
	}

	// rod 对中文的输入要用 InsertText（等价于逐字上屏）；逐字发送 + 随机间隔，复刻 Python 版节奏
	for _, r := range text {
		if err := page.InsertText(string(r)); err != nil {
			return fmt.Sprintf("输入失败：%v（截图 %s）", err, failShot(page, base, name, "typefail"))
		}
		wait(randInt(60, 130))
	}
	wait(randInt(300, 900))

	if evalInputText(page) == "" {
		// 写不进去时留证：截图 + 输入框状态，便于区分「没聚焦」还是「被禁用/抖音限制只能发一条」
		shot := dryShotPath(base, name+"_fail")
		saveShot(page, shot)
		if focus != nil {
			lg.Log("    输入框状态：可编辑=%v 禁用=%v 已聚焦=%v（截图 %s）",
				focus.Editable, focus.Disabled, focus.Focused, baseName(shot))
			if focus.Disabled || !focus.Editable {
				return "输入框不可编辑/被禁用（可能被抖音限制：需对方先回复或仅能发一条），未发送"
			}
		} else {
			lg.Log("    未读取到输入框状态（截图 %s）", baseName(shot))
		}
		return "输入没写进去（输入框仍为空），未发送"
	}

	// 发送。容器/无头环境里 CDP 的键鼠事件可能整条被吞（实测：文字能写进输入框，
	// 但回车不发送、点发送按钮也没反应）。所以按「越接近原生越先试，纯 JS 兜底」的顺序逐个尝试：
	//   ① 回车（CDP）  ② JS 派发 Enter  ③ 元素点击发送按钮  ④ 原生 click()  ⑤ 指针序列点击
	usedJSEnter := false
	if !nativeInputDead {
		_ = page.Keyboard.Type(input.Enter)
		wait(1200)
	} else {
		lg.Log("    本环境原生键盘事件无效，直接用 JS 派发 Enter")
	}

	if evalInputText(page) != "" {
		if !nativeInputDead {
			lg.Log("    回车没发出去，改用 JS 派发 Enter")
		}
		usedJSEnter = true
		jsEnter(page, `[data-dy-input="1"]`)
		wait(1500)
	}

	// 发送成功的判据：输入框被清空
	if evalInputText(page) != "" {
		btnFound := false
		if v, ok := evalRaw(page, MARK_SEND_JS); ok && v.Bool() {
			btnFound = true
			lg.Log("    JS Enter 也没发出去，改用发送按钮")
			clickMarked(page, `[data-dy-send="1"]`)
			wait(1500)
			// 仍未清空 → 再派发一次原生 click（有些组件只监听 click）
			if evalInputText(page) != "" {
				jsClickMarked(page, `[data-dy-send="1"]`)
				wait(1200)
			}
			// 仍未清空 → 整条指针序列（应对只监听 pointerdown/mousedown 的组件）
			if evalInputText(page) != "" {
				jsPointerClickEl(page, `[data-dy-send="1"]`)
				wait(1200)
			}
		}
		if evalInputText(page) != "" {
			// 留证：截图 + 是否找到发送按钮 + 视口信息，便于核对是选择器失效还是坐标/窗口问题
			shot := dryShotPath(base, name+"_sendfail")
			saveShot(page, shot)
			lg.Log("    发送按钮：找到=%v（截图 %s）", btnFound, baseName(shot))
			logMetrics(page, lg)
			return "发送后输入框仍在，可能未发出（请查看截图确认）"
		}
	}

	if usedJSEnter && !nativeInputDead {
		// 原生回车没发出去、靠 JS 派发才成功 → 记下来，后续直接走 JS，省掉每次的等待
		nativeInputDead = true
		lg.Log("    提示：本环境不接收原生键盘事件，后续改用 JS 派发（发送依旧生效）")
	}

	hist[name] = time.Now().Format("2006-01-02T15:04:05")
	saveHistory(historyPath(base), hist)
	// 记进「二次消息观察名单」：之后客户再发消息只截图、不再自动回复
	fs.markReplied(name, text)
	return fmt.Sprintf("已回复「%s」", text)
}

// openStrangerFolder 复刻 Python open_stranger_folder
func openStrangerFolder(page *rod.Page, folder ConvItem, lg *Logger) bool {
	state := "无红点"
	if folder.Unread {
		state = "有未读"
	}
	lg.Log("  打开「陌生人消息」（%s，预览 %q）", state, truncate(folder.Preview, 24))
	// DOM 标记 + 元素点击；标记不到（class 改名）才退回坐标
	clicked := false
	if v, ok := evalRaw(page, MARK_STRANGER_JS); ok && v.Bool() {
		clicked = clickMarked(page, `[data-dy-stranger="1"]`)
		if !clicked {
			clicked = jsClickMarked(page, `[data-dy-stranger="1"]`)
		}
	}
	if !clicked {
		mouseClick(page, float64(folder.CX), float64(folder.CY))
	}
	wait(2600)
	return pageAlive(page)
}

// backFromFolder 复刻 Python back_from_folder
func backFromFolder(page *rod.Page, lg *Logger) {
	if pos := evalBackBtn(page); pos != nil {
		// DOM 标记 + 元素点击优先，失败再退回坐标
		if v, ok := evalRaw(page, MARK_BACK_JS); ok && v.Bool() {
			if clickMarked(page, `[data-dy-back="1"]`) || jsClickMarked(page, `[data-dy-back="1"]`) {
				wait(1800)
				lg.Log("  已返回主列表（点了「%s」）", pos.Text)
				return
			}
		}
		mouseClick(page, float64(pos.CX), float64(pos.CY))
		wait(1800)
		lg.Log("  已返回主列表（坐标点击「%s」）", pos.Text)
		return
	}
	lg.Log("  没找到返回按钮，刷新页面回到主列表")
	_ = page.Navigate(chatURL)
	wait(6000)
}

// processList 复刻 Python process_list：按顺序回复一组会话，返回真正发出的条数
func processList(page *rod.Page, items []ConvItem, cfg *Config, dry bool, hist map[string]string, lg *Logger, base string, fs *followStore) int {
	sent := 0
	for _, c := range items {
		if inQuietHours(cfg.QuietHours) {
			lg.Log("当前在免打扰时段，本轮不发送")
			break
		}
		if hourCount(hist) >= cfg.MaxRepliesPerHour {
			lg.Log("已达每小时发送上限，本轮停止")
			break
		}
		key := c.Name
		if !cooldownOK(hist, key, cfg.SameUserCooldownHours) {
			lg.Log("  冷却中，跳过：%s", truncate(key, 20))
			continue
		}
		// 已经自动回复过一次：客户再来消息不再回复（截图已在扫描阶段留好）
		if cfg.FollowUpNoReply && fs.watching(key, cfg.FollowUpWatchHours) {
			lg.Log("  已自动回复过，二次消息不再回复：%s（截图见 shots/followup）", truncate(key, 20))
			continue
		}
		res := replyOne(page, c, cfg, dry, hist, lg, base, fs)
		lg.Log("  %q → %s", truncate(key, 20), res)
		if lg.Hook != nil {
			lg.Hook(key, res, strings.HasPrefix(res, "已回复"))
		}
		if !dry && strings.HasPrefix(res, "已回复") {
			sent++
			wait(randInt(2000, 5000)) // 会话之间也停一下
		}
	}
	return sent
}

// replyToNamed 复刻 Python reply_to_named：定向回复指定会话（忽略未读标记）
func replyToNamed(page *rod.Page, name string, cfg *Config, dry bool, hist map[string]string, lg *Logger, base string, fs *followStore) int {
	convs := evalConvList(page)
	var hit *ConvItem
	for i := range convs {
		if convs[i].Kind != "stranger_folder" && strings.Contains(convs[i].Name, name) {
			hit = &convs[i]
			break
		}
	}

	inFolder := false
	if hit == nil {
		for _, f := range convs {
			if f.Kind == "stranger_folder" && openStrangerFolder(page, f, lg) {
				inFolder = true
				for _, c := range evalConvList(page) {
					if c.Kind != "stranger_folder" && strings.Contains(c.Name, name) {
						cc := c
						hit = &cc
					}
				}
				break
			}
		}
	}

	if hit == nil {
		lg.Log("没找到会话「%s」", name)
		if inFolder {
			backFromFolder(page, lg)
		}
		return 0
	}

	if looksLikeGroup(*hit) {
		lg.Log("「%s」被判定为群聊，拒绝发送", name)
		if inFolder {
			backFromFolder(page, lg)
		}
		return 0
	}

	res := replyOne(page, *hit, cfg, dry, hist, lg, base, fs)
	lg.Log("定向回复 %q → %s", name, res)
	if lg.Hook != nil {
		lg.Hook(name, res, strings.HasPrefix(res, "已回复"))
	}
	if inFolder {
		backFromFolder(page, lg)
	}
	if strings.HasPrefix(res, "已回复") {
		return 1
	}
	return 0
}

// runOnce 复刻 Python run_once：一轮完整扫描
func runOnce(page *rod.Page, cfg *Config, dry bool, hist map[string]string, lg *Logger, base string,
	fs *followStore, listOnly bool, dump bool) int {
	convs := evalConvList(page)
	if len(convs) == 0 {
		lg.Log("没扫到会话（可能列表还没加载完，或页面结构变了）")
		return 0
	}

	var folders, normals []ConvItem
	for _, c := range convs {
		if c.Kind == "stranger_folder" {
			folders = append(folders, c)
		} else {
			normals = append(normals, c)
		}
	}

	lg.Log("扫到 %d 项：普通会话 %d 个，陌生人消息入口 %d 个", len(convs), len(normals), len(folders))
	for _, c := range convs {
		tag := "单聊"
		if c.Kind == "stranger_folder" {
			tag = "陌生人消息"
		} else if looksLikeGroup(c) {
			tag = "群聊"
		}
		flag := ""
		if c.Unread {
			flag = " 【未读】"
		}
		lg.Log("  [%s]%s %q  预览：%q", tag, flag, truncate(c.Name, 26), truncate(c.Preview, 22))
	}

	if listOnly {
		lg.Log("只读模式：仅列出，未点击任何会话")
		return 0
	}

	sent := 0

	// ---- 0) 二次消息核对：已经自动回复过的会话，客户再发消息 → 截图留证，不回复 ----
	if cfg.FollowUpScreenshot {
		if n := scanFollowUps(page, normals, cfg, fs, dry, lg, base); n > 0 {
			lg.Log("二次消息截图：本轮 %d 张（%s）", n, filepathJoin(base, "shots", "followup"))
		}
	}

	// ---- 1) 普通会话（互关/已建立会话的私信） ----
	var targets []ConvItem
	for _, c := range normals {
		if c.Unread && !looksLikeGroup(c) {
			targets = append(targets, c)
		}
	}

	// 红点不一定出现（实测新消息到达时主列表可能没有红点），
	// 所以对「刚刚 / N分钟前」的会话再核对一次最后一条是谁说的。
	if cfg.CheckRecentConvs {
		for _, c := range normals {
			inTargets := false
			for _, t := range targets {
				if t.Name == c.Name && t.Y == c.Y {
					inTargets = true
					break
				}
			}
			if inTargets || looksLikeGroup(c) {
				continue
			}
			if isRecent(c) {
				targets = append(targets, c)
				lg.Log("  最近活跃，纳入核对：%q", truncate(c.Name, 18))
			}
		}
	}

	for _, c := range normals {
		if c.Unread && looksLikeGroup(c) {
			lg.Log("  跳过群聊，不点开：%s", truncate(c.Name, 20))
		}
	}
	if len(targets) > 0 {
		sent += processList(page, targets, cfg, dry, hist, lg, base, fs)
	}

	// ---- 2) 陌生人消息（非互关用户的私信会折叠在这里） ----
	for _, f := range folders {
		if !f.Unread && !dump {
			lg.Log("  陌生人消息入口无红点，跳过")
			continue
		}
		if !openStrangerFolder(page, f, lg) {
			continue
		}

		var innerAll, innerUnread []ConvItem
		for _, c := range evalConvList(page) {
			if c.Kind == "stranger_folder" {
				continue
			}
			innerAll = append(innerAll, c)
			if c.Unread {
				innerUnread = append(innerUnread, c)
			}
		}
		lg.Log("  入口内共 %d 个会话，标记未读 %d 个", len(innerAll), len(innerUnread))
		for _, c := range innerAll {
			flag := ""
			if c.Unread {
				flag = " 【未读】"
			}
			lg.Log("    ·%s %q  预览：%q", flag, truncate(c.Name, 24), truncate(c.Preview, 26))
		}

		// 陌生人消息里也可能有「已自动回复过、客户又来的」会话 —— 同样只截图不回复
		if cfg.FollowUpScreenshot {
			scanFollowUps(page, innerAll, cfg, fs, dry, lg, base)
		}

		var picked []ConvItem
		if len(innerUnread) > 0 {
			picked = innerUnread
		} else if len(innerAll) > 0 {
			// 子项没有红点时，取最近的一个（列表按时间倒序）
			picked = innerAll[:1]
			lg.Log("  子项无红点，按最近一条处理")
		}

		var pg []ConvItem
		for _, c := range picked {
			if !looksLikeGroup(c) {
				pg = append(pg, c)
			}
		}
		if len(pg) > 0 {
			sent += processList(page, pg, cfg, dry, hist, lg, base, fs)
		}

		backFromFolder(page, lg)
	}

	return sent
}
