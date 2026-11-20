package douyinkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

func TestLoginReadinessRequiresCookieAndRealPage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state loginObservation
		want  bool
	}{
		{"cookie only", loginObservation{Authenticated: true}, false},
		{"page only", loginObservation{pageLoginState: pageLoginState{ChatReady: true}}, false},
		{"modal over page", loginObservation{Authenticated: true, pageLoginState: pageLoginState{ChatReady: true, LoginVisible: true}}, false},
		{"identity verification blocks ready page", loginObservation{Authenticated: true, pageLoginState: pageLoginState{ChatReady: true, VerificationRequired: true}}, false},
		{"confirmed", loginObservation{Authenticated: true, pageLoginState: pageLoginState{ChatReady: true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.state.ready() != tc.want {
				t.Fatalf("state=%+v", tc.state)
			}
		})
	}
}

func TestSnapshotsDoNotWaitForBrowserAndAreCopies(t *testing.T) {
	c := &Client{}
	qr := &QRCode{Base64: "fresh", CapturedAt: time.Now()}
	c.setLastQR(qr)
	qr.Base64 = "mutated"
	c.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, ok := c.LatestQR()
		if !ok || got.Base64 != "fresh" {
			t.Errorf("snapshot=%+v ok=%v", got, ok)
			return
		}
		got.Base64 = "also mutated"
		c.IsLoggedInCached()
		st, err := c.QRStatus()
		if err != nil || st.QRBase64 != "fresh" {
			t.Errorf("busy QRStatus=%+v error=%v", st, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		c.mu.Unlock()
		t.Fatal("snapshot waited for browser lock")
	}
	c.mu.Unlock()
	c.setLastQR(&QRCode{CapturedAt: time.Now().Add(-qrCacheTTL)})
	if _, ok := c.LatestQR(); ok {
		t.Fatal("expired QR returned")
	}
	c.setLastQR(&QRCode{CapturedAt: time.Now()})
	c.loggedIn.Store(true)
	if _, ok := c.LatestQR(); ok {
		t.Fatal("QR returned after login")
	}
}

func TestEnsureLoginCanCancelWhileBrowserBusy(t *testing.T) {
	c := &Client{}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.EnsureLogin(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

func TestConcurrentSnapshots(t *testing.T) {
	c := &Client{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.setLastQR(&QRCode{CapturedAt: time.Now()})
				c.LatestQR()
				c.loggedIn.Store(j%2 == 0)
				c.IsLoggedInCached()
			}
		}()
	}
	wg.Wait()
}

// Opt-in: uses an isolated temporary browser profile and local HTML fixtures.
// No Douyin navigation, QR scan, or real message sending is performed.
func TestBrowserLoginFixtures(t *testing.T) {
	if os.Getenv("DOUYINKIT_BROWSER_TEST") != "1" {
		t.Skip("set DOUYINKIT_BROWSER_TEST=1 for local browser fixtures")
	}
	bin, err := pickChrome("auto")
	if err != nil || bin == "" {
		t.Fatal("a locally installed browser is required")
	}
	l := launcher.New().Bin(bin).Headless(true).Leakless(false).UserDataDir(t.TempDir()).Set("no-sandbox")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	u, err := l.Context(ctx).Launch()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Kill)
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Timeout(2 * time.Second).Close() })
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatal(err)
	}
	var html atomic.Value
	html.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html.Load().(string)))
	}))
	defer server.Close()
	if err := b.SetCookies([]*proto.NetworkCookieParam{{Name: "sessionid", Value: "fixture-session-value", Domain: ".douyin.com", Path: "/"}}); err != nil {
		t.Fatal(err)
	}
	c := &Client{browser: b, page: page, lg: &Logger{}, cfg: DefaultConfig()}
	style := `<style>div,main{width:220px;min-height:60px}</style>`
	for _, tc := range []struct {
		name, body string
		ready      bool
	}{
		{"chat mentions login", `<div class="conversationConversationItemwrapper">客户：需要扫码登录吗？</div>`, true},
		{"positional fallback is not a chat", `<div>扫码登录</div>`, false},
		{"visible login blocks chat", `<div class="conversationConversationItemwrapper">客户</div><div role="dialog">扫码登录</div>`, false},
		{"hidden login ignored", `<div class="conversationConversationItemwrapper">客户</div><div role="dialog" style="display:none">扫码登录</div>`, true},
		{"transparent parent ignored", `<div class="conversationConversationItemwrapper">客户</div><div style="opacity:0"><div role="dialog">扫码登录</div></div>`, true},
		{"real empty chat", `<main class="conversationList">暂无会话</main>`, true},
		{"skeleton is pending", `<main class="conversationList"></main>`, false},
		{"extra identity verification blocks chat", `<div class="conversationConversationItemwrapper">客户</div><section><h2>身份验证</h2><p>为保障账号安全，请先完成身份验证，以确保为本人操作</p><button>接收短信验证码</button><button>发送短信验证</button></section>`, false},
		{"message mentioning identity verification is not a dialog", `<div class="conversationConversationItemwrapper">客户：身份验证怎么弄？</div>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			html.Store("<!doctype html><meta charset=utf-8>" + style + tc.body)
			if err := page.Timeout(5 * time.Second).Navigate(server.URL + "/chat"); err != nil {
				t.Fatal(err)
			}
			if err := page.Timeout(5 * time.Second).WaitLoad(); err != nil {
				t.Fatal(err)
			}
			state, err := c.observeLoginLocked(context.Background())
			if err != nil || state.ready() != tc.ready {
				t.Fatalf("state=%+v error=%v wantReady=%v", state, err, tc.ready)
			}
			if c.IsLoggedInCached() != tc.ready {
				t.Fatal("snapshot disagrees with readiness")
			}
		})
	}
	// A ready chat consistently passes settleLogin and EnsureLogin without
	// triggering a reload or requiring an arbitrary sleep.
	html.Store("<!doctype html>" + style + `<div class="conversationConversationItemwrapper">客户</div>`)
	if err := page.Navigate(server.URL + "/chat"); err != nil {
		t.Fatal(err)
	}
	if err := page.Timeout(5 * time.Second).WaitLoad(); err != nil {
		t.Fatal(err)
	}
	if !c.settleLogin(context.Background(), time.Second) {
		t.Fatal("ready chat failed login settlement")
	}
	if err := c.EnsureLogin(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	c.cfg.AutoReply = false
	c.cfg.FollowUpScreenshot = false
	runCtx, runCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer runCancel()
	if err := c.Run(runCtx, true); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run rejected login accepted by EnsureLogin: %v", err)
	}
}

func TestBrowserStartupContextDoesNotExpireSession(t *testing.T) {
	if os.Getenv("DOUYINKIT_BROWSER_TEST") != "1" {
		t.Skip("set DOUYINKIT_BROWSER_TEST=1 for local browser fixtures")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><title>local fixture</title><button id="click" onclick="window.clicked=event.isTrusted">test</button>`))
	}))
	defer server.Close()
	base := t.TempDir()
	if err := os.MkdirAll(base+"/logs", 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, page, cleanup, err := openPageAtURL(ctx, "auto", base, true, server.URL+"/chat", t.Logf)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := page.Timeout(2 * time.Second).Eval(`() => document.title`); err != nil {
		t.Fatalf("session inherited expired startup context: %v", err)
	}
	el, err := page.Timeout(2 * time.Second).Element("#click")
	if err != nil {
		t.Fatal(err)
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		t.Fatalf("native input inherited expired startup context: %v", err)
	}
	if result, err := page.Eval(`() => window.clicked===true`); err != nil || !result.Value.Bool() {
		t.Fatal("trusted click was not delivered")
	}
	cleanup() // cleanup is idempotent
}
