package douyinkit

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

func TestQRValidationCompositesTransparentBackground(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 180, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 180; x++ {
			if (x/10+y/10)%2 == 0 {
				img.SetNRGBA(x, y, color.NRGBA{A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := validateQRImage(buf.Bytes()); err != nil {
		t.Fatalf("transparent code pixels rejected: %v", err)
	}
	buf.Reset()
	if err := png.Encode(&buf, image.NewNRGBA(img.Bounds())); err != nil {
		t.Fatal(err)
	}
	if looksLikeQR(buf.Bytes()) {
		t.Fatal("transparent blank accepted as a code")
	}
}

func TestCachedLoginStatusPreservesCaptureFailureUntilSuccess(t *testing.T) {
	c := &Client{}
	c.setLoginProgress("error", errors.New("截图超时"))
	c.setLoginProgress("fetching", nil)
	c.mu.Lock()
	st := c.CachedLoginStatus()
	c.mu.Unlock()
	if st.Error != "截图超时" || st.Stage != "fetching" || !st.Busy {
		t.Fatalf("failure disappeared during retry: %+v", st)
	}
	c.cacheQR(&QRCode{DataURL: "data:image/png;base64,fixture", CapturedAt: time.Now()})
	st = c.CachedLoginStatus()
	if st.Error != "" || st.Stage != "waiting_scan" || st.DataURL == "" || st.LoggedIn {
		t.Fatalf("unexpected capture snapshot: %+v", st)
	}
	c.loggedIn.Store(true)
	st = c.CachedLoginStatus()
	if !st.LoggedIn || st.DataURL != "" || st.Error != "" {
		t.Fatalf("unexpected login snapshot: %+v", st)
	}
}

// These are isolated local HTML fixtures, not a connection to the user's server.
func TestBrowserQRFixtures(t *testing.T) {
	if os.Getenv("DOUYINKIT_BROWSER_TEST") != "1" {
		t.Skip("set DOUYINKIT_BROWSER_TEST=1 for browser fixtures")
	}
	bin, err := pickChrome("auto")
	if err != nil || bin == "" {
		t.Fatal("locally installed browser required")
	}
	l := launcher.New().Bin(bin).Headless(true).Leakless(false).UserDataDir(t.TempDir()).Set("no-sandbox")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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
	if err := b.SetCookies([]*proto.NetworkCookieParam{{Name: "sessionid", Value: "stale-cookie-fixture", Domain: ".douyin.com", Path: "/"}}); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if err := os.MkdirAll(base+"/shots", 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Client{browser: b, page: page, lg: &Logger{}, base: base}
	load := func(body string) {
		t.Helper()
		html.Store("<!doctype html><meta charset=utf-8><style>body{background:white}</style>" + body)
		if err := page.Timeout(5 * time.Second).Navigate(server.URL + "/chat"); err != nil {
			t.Fatal(err)
		}
		if err := page.Timeout(5 * time.Second).WaitLoad(); err != nil {
			t.Fatal(err)
		}
	}
	pattern := `<script>const c=document.querySelector('canvas');const x=c.getContext('2d');x.fillStyle='white';x.fillRect(0,0,180,180);x.fillStyle='black';for(let y=0;y<18;y++)for(let i=0;i<18;i++)if((i+y)%2===0)x.fillRect(i*10,y*10,10,10);</script>`
	t.Run("large container is read from canvas", func(t *testing.T) {
		load(`<div id="douyin_login_comp_scan_code_qrcode" style="width:500px;height:500px"><canvas width="180" height="180"></canvas></div>` + pattern)
		state, err := c.observeLoginLocked(ctx)
		if err != nil || !state.Authenticated || !state.QRVisible || state.ready() {
			t.Fatalf("state=%+v error=%v", state, err)
		}
		el, err := page.Timeout(2 * time.Second).Element(qrSelPrimary)
		if err != nil {
			t.Fatal(err)
		}
		container, err := el.Timeout(5*time.Second).Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
		if err != nil {
			t.Fatal(err)
		}
		if looksLikeQR(container) {
			t.Fatal("fixture must reproduce container screenshot rejection")
		}
		qr, err := c.fetchQRContextLocked(ctx)
		if err != nil || qr == nil || qr.Base64 == "" {
			t.Fatalf("canvas not delivered with cookie present: qr=%v error=%v", qr != nil, err)
		}
		if got := c.CachedLoginStatus(); got.DataURL == "" || got.Stage != "waiting_scan" {
			t.Fatalf("capture not cached: %+v", got)
		}
	})
	t.Run("hidden duplicate does not hide visible code", func(t *testing.T) {
		c.setLastQR(nil)
		load(`<div id="douyin_login_comp_scan_code_qrcode" style="display:none"></div><div id="douyin_login_comp_scan_code_qrcode"><canvas width="180" height="180"></canvas></div>` + pattern)
		qr, err := c.captureQR(ctx, qrSelPrimary)
		if err != nil || qr == nil {
			t.Fatalf("visible duplicate not captured: %v", err)
		}
	})
	t.Run("extra verification is exposed instead of returning background QR", func(t *testing.T) {
		c.setLastQR(&QRCode{DataURL: "old-background-QR", CapturedAt: time.Now()})
		load(`<div id="douyin_login_comp_scan_code_qrcode" style="width:180px;height:180px"><canvas width="180" height="180"></canvas></div><section><h2>身份验证</h2><p>为保障账号安全，请先完成身份验证，以确保为本人操作</p><button>接收短信验证码</button><button>发送短信验证</button></section>` + pattern)
		qr, err := c.fetchQRContextLocked(ctx)
		if qr != nil || !errors.Is(err, errVerificationRequired) {
			t.Fatalf("verification not recognized: qr=%v error=%v", qr != nil, err)
		}
		st := c.CachedLoginStatus()
		if st.Stage != "verification_required" || st.Error == "" || st.DataURL != "" || st.LoggedIn {
			t.Fatalf("incorrect verification status: %+v", st)
		}
		if _, ok := c.LatestQR(); ok {
			t.Fatal("background QR returned over verification dialog")
		}
	})
	t.Run("QR disappearance preserves SSO cookies and local storage", func(t *testing.T) {
		if err := b.SetCookies(nil); err != nil {
			t.Fatal(err)
		}
		if err := b.SetCookies([]*proto.NetworkCookieParam{{Name: "sso_in_progress", Value: "pending-fixture", Domain: ".douyin.com", Path: "/"}}); err != nil {
			t.Fatal(err)
		}
		load(`<p>正在确认登录</p>`)
		if _, err := page.Eval(`() => localStorage.setItem('sso_pending','preserve-me')`); err != nil {
			t.Fatal(err)
		}
		c.setLastQR(nil)
		c.loginHadQR = true
		c.loginUncertainSince = time.Now().Add(-45 * time.Second)
		qr, err := c.fetchQRContextLocked(ctx)
		if qr != nil || !errors.Is(err, errAuthPending) {
			t.Fatalf("transition treated as a guest: qr=%v error=%v", qr != nil, err)
		}
		state, err := page.Eval(`() => ({host:location.host,stored:localStorage.getItem('sso_pending')})`)
		if err != nil || state.Value.Get("stored").Str() != "preserve-me" || !strings.Contains(server.URL, state.Value.Get("host").Str()) {
			t.Fatalf("transition was reset or navigated: %v %v", state, err)
		}
		cookies, err := b.GetCookies()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, cookie := range cookies {
			if cookie.Name == "sso_in_progress" {
				found = true
			}
		}
		if !found {
			t.Fatal("SSO cookies were erased during transition")
		}
		if st := c.CachedLoginStatus(); st.Stage != "logging_in" || st.DataURL != "" {
			t.Fatalf("incorrect transition state: %+v", st)
		}
		// Restore this fixture's auth Cookie for the next capture-failure test.
		if err := b.SetCookies([]*proto.NetworkCookieParam{{Name: "sessionid", Value: "stale-cookie-fixture", Domain: ".douyin.com", Path: "/"}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("blank visible code with cookie is an error", func(t *testing.T) {
		c.setLastQR(nil)
		load(`<div id="douyin_login_comp_scan_code_qrcode" style="width:180px;height:180px;background:white"><canvas width="180" height="180"></canvas></div>`)
		qr, err := c.fetchQRContextLocked(ctx)
		if qr != nil || err == nil || errors.Is(err, errAuthPending) {
			t.Fatalf("capture failure swallowed as pending login: qr=%v error=%v", qr != nil, err)
		}
		if !strings.Contains(err.Error(), "深色像素") {
			t.Fatalf("missing image diagnostics: %v", err)
		}
		st := c.CachedLoginStatus()
		if st.Error == "" || st.Stage != "error" || st.LoggedIn {
			t.Fatalf("missing failure snapshot: %+v", st)
		}
		state, probeErr := c.observeLoginLocked(ctx)
		if probeErr != nil || !state.Authenticated {
			t.Fatalf("capture failure erased cookie: %+v %v", state, probeErr)
		}
	})
}
