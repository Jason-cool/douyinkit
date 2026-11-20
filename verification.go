package douyinkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// SMSVerification contains no entered code and no unmasked phone number.
type SMSVerification struct {
	ID                string `json:"verification_id"`
	Method            string `json:"method"`
	Step              string `json:"step"` // choose_method / code_sent / verifying
	PhoneMasked       string `json:"phone_masked,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
	CanResend         bool   `json:"can_resend"`
	CanSubmit         bool   `json:"can_submit"`
	CodeLength        int    `json:"code_length"`
	Error             string `json:"error,omitempty"`
}

func cloneVerification(v *SMSVerification) *SMSVerification {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}

func (c *Client) updateVerificationLocked(v *SMSVerification) error {
	if v == nil {
		return nil
	}
	if c.smsID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return errors.New("无法创建短信验证会话，请稍后重试")
		}
		c.smsID = hex.EncodeToString(id[:])
	}
	copy := *v
	copy.ID = c.smsID
	remaining := int(time.Until(c.smsResendAt).Seconds() + 0.999)
	if remaining > copy.RetryAfterSeconds {
		copy.RetryAfterSeconds = remaining
	}
	if copy.RetryAfterSeconds > 0 {
		copy.CanResend = false
	}
	if time.Now().Before(c.smsSubmittingUntil) && copy.Error == "" {
		copy.Step, copy.CanSubmit = "verifying", false
		copy.CanResend = false
	} else if copy.Error != "" {
		c.smsSubmittingUntil = time.Time{}
	}
	c.snapshotMu.Lock()
	c.verification = &copy
	c.snapshotMu.Unlock()
	return nil
}

func (c *Client) clearVerificationLocked() {
	c.smsID = ""
	c.smsResendAt, c.smsSubmittingUntil = time.Time{}, time.Time{}
	c.snapshotMu.Lock()
	c.verification = nil
	c.snapshotMu.Unlock()
}

func (c *Client) markSMSActionLocked(ctx context.Context, action string) (string, error) {
	result, err := c.page.Context(ctx).Eval(markSMSActionJS, action)
	if err != nil || !result.Value.Bool() {
		return "", errors.New("短信验证页面已变化，请重新获取状态后重试")
	}
	return `[data-dy-sms-action="` + action + `"]`, nil
}

func (c *Client) clickSMSActionLocked(ctx context.Context, action string) error {
	selector, err := c.markSMSActionLocked(ctx, action)
	if err != nil {
		return err
	}
	// Dispatch exactly once. An ambiguous timeout must not cause duplicate SMS.
	el, err := c.page.Context(ctx).Element(selector)
	if err != nil {
		return errors.New("短信验证控件当前不可操作，请重新查询状态")
	}
	usable, err := el.Eval(`() => !this.disabled && !this.closest('[disabled],[aria-disabled="true"]') && getComputedStyle(this).pointerEvents!=='none'`)
	if err != nil || !usable.Value.Bool() {
		return errors.New("短信验证控件当前不可操作，请重新查询状态")
	}
	// This container accepts DOM clicks with a user gesture while CDP mouse
	// events can be silently ignored. Dispatch once; never replay an ambiguous
	// SMS request through a second strategy.
	result, err := c.page.Context(ctx).Evaluate(rod.Eval(`sel => {const e=document.querySelector(sel);if(!e||e.disabled||e.closest('[disabled],[aria-disabled="true"]')||getComputedStyle(e).pointerEvents==='none')return false;e.click();return true}`, selector).ByUser())
	if err != nil {
		return err
	}
	if !result.Value.Bool() {
		return errors.New("短信验证控件当前不可操作，请重新查询状态")
	}
	return nil
}

// prepareSMSLocked only selects receive-SMS automatically. Resending always
// requires SendSMSCode, and shares the same cooldown with concurrent callers.
func (c *Client) prepareSMSLocked(ctx context.Context, resend bool) error {
	state, err := c.observeLoginLocked(ctx)
	if err != nil {
		return err
	}
	if state.ready() {
		return nil
	}
	if !state.VerificationRequired || state.Verification == nil {
		return errors.New("当前没有待处理的短信身份验证")
	}
	v := c.CachedLoginStatus().Verification
	if v == nil {
		return errors.New("短信验证状态尚未准备好")
	}
	if v.Step != "choose_method" && !resend {
		return nil
	}
	if time.Now().Before(c.smsResendAt) || v.RetryAfterSeconds > 0 {
		return fmt.Errorf("请等待 %d 秒后再接收验证码", v.RetryAfterSeconds)
	}
	action := "receive"
	if v.Step != "choose_method" {
		if !v.CanResend {
			return errors.New("当前暂不可重新发送验证码，请稍后重试")
		}
		action = "resend"
	}
	c.smsResendAt = time.Now().Add(time.Minute)
	if err := c.updateVerificationLocked(v); err != nil {
		return err
	}
	if err := c.clickSMSActionLocked(ctx, action); err != nil {
		return errors.New("接收验证码操作未确认，请稍后查看状态，勿重复请求")
	}
	c.lg.Log("已请求接收短信验证码（不使用发送短信验证）")
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err = c.observeLoginLocked(ctx)
		if err != nil {
			return err
		}
		if state.ready() {
			return nil
		}
		if v = c.CachedLoginStatus().Verification; v != nil && v.Step != "choose_method" {
			if v.Error != "" {
				return errors.New(v.Error)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("短信接收页面尚未确认，请稍后查询状态")
		}
		if err := waitContext(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// SendSMSCode selects receive-SMS, or resends once the cooldown has expired.
// The optional id protects an existing verification from stale UI requests.
func (c *Client) SendSMSCode(ctx context.Context, id string) (*LoginStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := c.lockContext(ctx); err != nil {
		return c.CachedLoginStatus(), err
	}
	defer c.mu.Unlock()
	if c.page == nil {
		return c.CachedLoginStatus(), errors.New("浏览器会话不存在，请先扫码登录")
	}
	if state, err := c.observeLoginLocked(ctx); err != nil {
		return c.CachedLoginStatus(), err
	} else if state.ready() {
		return c.CachedLoginStatus(), nil
	}
	if id != "" && id != c.smsID {
		return c.CachedLoginStatus(), errors.New("身份验证会话已更新，请刷新状态后重试")
	}
	err := c.prepareSMSLocked(ctx, true)
	return c.CachedLoginStatus(), err
}

var smsCodePattern = regexp.MustCompile(`^[0-9]{4,8}$`)

// SubmitSMSCode submits to this client's existing page. The code is never
// stored in Client, returned in status, or written to the application log.
func (c *Client) SubmitSMSCode(ctx context.Context, id, code string) (*LoginStatus, error) {
	if !smsCodePattern.MatchString(code) {
		return c.CachedLoginStatus(), errors.New("验证码必须为 4 至 8 位数字")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := c.lockContext(ctx); err != nil {
		return c.CachedLoginStatus(), err
	}
	defer c.mu.Unlock()
	state, err := c.observeLoginLocked(ctx)
	if err != nil {
		return c.CachedLoginStatus(), err
	}
	if state.ready() {
		return c.CachedLoginStatus(), nil
	}
	v := c.CachedLoginStatus().Verification
	if id == "" || id != c.smsID {
		return c.CachedLoginStatus(), errors.New("身份验证会话已更新，请刷新状态后重试")
	}
	if v == nil || v.Step != "code_sent" || !v.CanSubmit {
		return c.CachedLoginStatus(), errors.New("当前不能提交验证码，请先接收验证码或等待验证完成")
	}
	if len(code) != v.CodeLength {
		return c.CachedLoginStatus(), fmt.Errorf("请输入 %d 位短信验证码", v.CodeLength)
	}
	selector, err := c.markSMSActionLocked(ctx, "input")
	if err != nil {
		return c.CachedLoginStatus(), err
	}
	el, err := c.page.Context(ctx).Element(selector)
	if err != nil {
		return c.CachedLoginStatus(), errors.New("没有找到短信验证码输入框")
	}
	if err = el.Focus(); err != nil {
		return c.CachedLoginStatus(), errors.New("无法聚焦短信验证码输入框")
	}
	if err = el.SelectAllText(); err != nil {
		return c.CachedLoginStatus(), errors.New("无法选择短信验证码输入框")
	}
	// Use a trusted input event, without Rod's text trace or storing the code.
	err = (proto.InputInsertText{Text: code}).Call(c.page.Context(ctx))
	if err != nil {
		return c.CachedLoginStatus(), errors.New("填写短信验证码失败")
	}
	// Check actual button enablement after React processes the input.
	selector, err = c.markSMSActionLocked(ctx, "submit")
	if err != nil {
		return c.CachedLoginStatus(), err
	}
	button, err := c.page.Context(ctx).Element(selector)
	if err != nil {
		return c.CachedLoginStatus(), errors.New("没有找到短信验证按钮")
	}
	enableDeadline := time.Now().Add(3 * time.Second)
	for {
		result, e := button.Eval(`() => !this.disabled && !this.closest('[disabled],[aria-disabled="true"]') && getComputedStyle(this).pointerEvents!=='none'`)
		if e == nil && result.Value.Bool() {
			break
		}
		if time.Now().After(enableDeadline) {
			return c.CachedLoginStatus(), errors.New("短信验证按钮尚不可用，请检查验证码后重试")
		}
		if e := waitContext(ctx, 100*time.Millisecond); e != nil {
			return c.CachedLoginStatus(), e
		}
	}
	c.smsSubmittingUntil = time.Now().Add(30 * time.Second)
	pending := *v
	pending.Error = "" // the prior attempt's error must not cancel this submission
	if err := c.updateVerificationLocked(&pending); err != nil {
		return c.CachedLoginStatus(), err
	}
	if err = c.clickSMSActionLocked(ctx, "submit"); err != nil {
		return c.CachedLoginStatus(), errors.New("验证码提交结果暂未确认，请查询登录状态")
	}
	c.lg.Log("短信验证码已提交，等待身份验证及登录确认")
	// Return an accepted/verifying result promptly; qrCode polling continues
	// observing the result. Production HTTP write_timeout is only 10 seconds.
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err = c.observeLoginLocked(ctx)
		if err != nil {
			return c.CachedLoginStatus(), nil // already dispatched; do not invite duplicate submission
		}
		if state.ready() {
			return c.CachedLoginStatus(), nil
		}
		v = c.CachedLoginStatus().Verification
		if v != nil && v.Error != "" {
			return c.CachedLoginStatus(), errors.New(v.Error)
		}
		if !state.VerificationRequired {
			c.setLoginProgress("logging_in", nil)
			return c.CachedLoginStatus(), nil
		}
		if time.Now().After(deadline) {
			return c.CachedLoginStatus(), nil
		}
		if err := waitContext(ctx, 200*time.Millisecond); err != nil {
			return c.CachedLoginStatus(), nil
		}
	}
}
