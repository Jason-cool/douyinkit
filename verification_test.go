package douyinkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestVerificationSnapshotIsCopied(t *testing.T) {
	c := &Client{}
	if err := c.updateVerificationLocked(&SMSVerification{Method: "receive_sms", Step: "code_sent", PhoneMasked: "181******60", CodeLength: 6, CanSubmit: true}); err != nil {
		t.Fatal(err)
	}
	c.setLoginProgress("verification_required", errVerificationRequired)
	st := c.CachedLoginStatus()
	if st.Verification == nil || len(st.Verification.ID) != 32 {
		t.Fatalf("missing verification id: %+v", st)
	}
	st.Verification.PhoneMasked = "mutated"
	if c.CachedLoginStatus().Verification.PhoneMasked != "181******60" {
		t.Fatal("caller mutated cached verification")
	}
}

func TestBrowserReceiveSMSFlow(t *testing.T) {
	if os.Getenv("DOUYINKIT_BROWSER_TEST") != "1" {
		t.Skip("set DOUYINKIT_BROWSER_TEST=1 for isolated browser fixtures")
	}
	bin, err := pickChrome("auto")
	if err != nil || bin == "" {
		t.Fatal("local browser required")
	}
	l := launcher.New().Bin(bin).Headless(true).Leakless(false).UserDataDir(t.TempDir()).Set("no-sandbox")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	u, err := l.Context(ctx).Launch()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Kill)
	b := rod.New().ControlURL(u)
	if err = b.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Timeout(2 * time.Second).Close() })
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatal(err)
	}
	var submissions atomic.Int32
	var verificationDelay atomic.Int64
	verificationDelay.Store(int64(300 * time.Millisecond))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/verify" {
			submissions.Add(1)
			body, _ := io.ReadAll(r.Body)
			time.Sleep(time.Duration(verificationDelay.Load()))
			w.Header().Set("Content-Type", "application/json")
			if string(body) == "654321" {
				_, _ = w.Write([]byte(`{"ok":true}`))
			} else if string(body) == "222222" {
				_, _ = w.Write([]byte(`{"error":"错误次数过多或验证码过期，请稍后重试"}`))
			} else {
				_, _ = w.Write([]byte(`{"error":"验证码错误"}`))
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(smsFixtureHTML))
	}))
	defer server.Close()
	if err = b.SetCookies([]*proto.NetworkCookieParam{{Name: "sessionid", Value: "fixture-cookie-value", Domain: ".douyin.com", Path: "/"}}); err != nil {
		t.Fatal(err)
	}
	if err = page.Navigate(server.URL + "/chat"); err != nil {
		t.Fatal(err)
	}
	if err = page.Timeout(4 * time.Second).WaitLoad(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c := &Client{browser: b, page: page, lg: &Logger{UserLog: func(format string, args ...any) { /* no OTP passed here */ }}, cfg: DefaultConfig(), opt: Options{AutoReceiveSMS: true}}
	c.lg.UserLog = func(format string, args ...any) { logs.WriteString(fmt.Sprintf(format, args...)) }
	loginDone := make(chan error, 1)
	go func() { loginDone <- c.EnsureLogin(ctx, nil) }()
	deadline := time.Now().Add(8 * time.Second)
	for {
		st := c.CachedLoginStatus()
		if st.Verification != nil && st.Verification.Step == "code_sent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic receive did not open SMS form: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	v := c.CachedLoginStatus().Verification
	if v.Method != "receive_sms" || v.PhoneMasked != "181******60" || v.CanResend || !v.CanSubmit || v.CodeLength != 6 {
		t.Fatalf("incorrect SMS state: %+v", v)
	}
	if _, err = c.SendSMSCode(ctx, v.ID); err == nil {
		t.Fatal("cooldown was bypassed")
	}
	if _, err = c.SubmitSMSCode(ctx, "wrong-session", "123456"); err == nil {
		t.Fatal("stale verification accepted")
	}
	if _, err = c.SubmitSMSCode(ctx, v.ID, "1234"); err == nil {
		t.Fatal("wrong length accepted")
	}
	if submissions.Load() != 0 {
		t.Fatal("invalid request reached verification handler")
	}
	// Let the fixture's cooldown expire without waiting a real minute. Polling
	// must not resend; only an explicit SendSMSCode call may do so.
	c.mu.Lock()
	c.smsResendAt = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, err = page.Eval(`() => window.expireSMS()`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	counts, err := page.Eval(`() => ({received:window.received,outgoing:window.outgoing})`)
	if err != nil || counts.Value.Get("received").Int() != 1 || counts.Value.Get("outgoing").Int() != 0 {
		t.Fatalf("polling duplicated SMS or used outgoing method: %v %v", counts, err)
	}
	if _, err = c.SendSMSCode(ctx, v.ID); err != nil {
		t.Fatalf("explicit resend failed: %v", err)
	}
	st, err := c.SubmitSMSCode(ctx, v.ID, "111111")
	if err == nil || st.Verification == nil || st.Verification.Error != "验证码错误" {
		t.Fatalf("wrong code error missing: %+v %v", st, err)
	}
	st, err = c.SubmitSMSCode(ctx, v.ID, "222222")
	if err == nil || st.Verification == nil || st.Verification.Error != "错误次数过多或验证码过期，请稍后重试" || st.Verification.Step == "verifying" {
		t.Fatalf("expired code error missing or still verifying: %+v %v", st, err)
	}
	verificationDelay.Store(int64(3 * time.Second))
	st, err = c.SubmitSMSCode(ctx, v.ID, "654321")
	if err != nil || (!st.LoggedIn && (st.Verification == nil || st.Verification.Step != "verifying")) {
		t.Fatalf("correct code not accepted: %+v %v", st, err)
	}
	select {
	case err := <-loginDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureLogin did not resume after verification")
	}
	st = c.CachedLoginStatus()
	if !st.LoggedIn || st.Verification != nil {
		t.Fatalf("login did not finish after asynchronous verification: %+v", st)
	}
	if submissions.Load() != 3 {
		t.Fatalf("submissions=%d", submissions.Load())
	}
	encoded, _ := json.Marshal(st)
	if strings.Contains(string(encoded), "654321") || strings.Contains(logs.String(), "654321") || strings.Contains(logs.String(), "111111") {
		t.Fatal("verification code leaked into response/logs")
	}
}

const smsFixtureHTML = `<!doctype html><meta charset="utf-8"><style>section{width:600px;min-height:250px}input,button{height:40px;min-width:80px}</style>
<section id="choice"><h2>身份验证</h2><p>为保障账号安全，请先完成身份验证，以确保为本人操作</p><button id="receive">接收短信验证码</button><button onclick="window.outgoing++">发送短信验证</button></section>
<section id="sms" style="display:none"><h2>接收短信验证码</h2><p>短信已发送至 181******60</p><input placeholder="请输入验证码" maxlength="6"><span id="retry">41s后重新发送</span><p id="error"></p><button>取消</button><button id="verify" disabled>验证</button></section>
<script>
window.received=0;window.outgoing=0;
const sms=document.querySelector('#sms'),retry=document.querySelector('#retry'),input=sms.querySelector('input'),verify=document.querySelector('#verify'),error=document.querySelector('#error');
function receive(){window.received++;document.querySelector('#choice').style.display='none';sms.style.display='block';retry.textContent='41s后重新发送';}
document.querySelector('#receive').onclick=receive;
window.expireSMS=()=>{retry.textContent='重新发送';retry.onclick=receive;};
input.addEventListener('input',()=>{error.textContent='';setTimeout(()=>verify.disabled=!/^\d{6}$/.test(input.value),250);});
verify.onclick=async()=>{verify.disabled=true;const result=await(await fetch('/verify',{method:'POST',body:input.value})).json();if(result.ok){document.body.innerHTML='<div class="conversationConversationItemwrapper" style="width:220px;height:60px">客户</div>';}else{error.textContent=result.error;verify.disabled=false;}};
</script>`
