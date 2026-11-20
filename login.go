package douyinkit

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

const loginProbeTimeout = 4 * time.Second

// pageLoginState only uses actual chat/login elements, never the positional
// fallback used when scanning conversations.
type pageLoginState struct {
	ChatReady            bool             `json:"chat_ready"`
	LoginVisible         bool             `json:"login_visible"`
	QRVisible            bool             `json:"qr_visible"`
	VerificationRequired bool             `json:"verification_required"`
	PageURL              string           `json:"page_url"`
	DocumentReady        string           `json:"document_ready"`
	Verification         *SMSVerification `json:"verification"`
}

type loginObservation struct {
	pageLoginState
	Authenticated bool
}

func (s loginObservation) ready() bool {
	return s.Authenticated && s.ChatReady && !s.LoginVisible && !s.VerificationRequired
}

func (c *Client) observeLoginLocked(ctx context.Context) (loginObservation, error) {
	var state loginObservation
	if c.browser == nil || c.page == nil {
		c.loggedIn.Store(false)
		return state, fmt.Errorf("浏览器未连接")
	}
	ctx, cancel := context.WithTimeout(ctx, loginProbeTimeout)
	defer cancel()
	cookies, err := c.browser.Context(ctx).GetCookies()
	if err != nil {
		c.loggedIn.Store(false)
		return state, err
	}
	for _, cookie := range cookies {
		if authCookies[cookie.Name] && len(cookie.Value) > 8 &&
			(cookie.Domain == "douyin.com" || cookie.Domain == ".douyin.com" ||
				cookie.Domain == "www.douyin.com") {
			state.Authenticated = true
			break
		}
	}
	result, err := c.page.Context(ctx).Eval(LOGIN_STATE_JS)
	if err != nil {
		c.loggedIn.Store(false)
		return state, err
	}
	if err := result.Value.Unmarshal(&state.pageLoginState); err != nil {
		c.loggedIn.Store(false)
		return state, err
	}
	c.loggedIn.Store(state.ready())
	if state.ready() {
		c.clearVerificationLocked()
		c.loginHadQR = false
		c.loginUncertainSince = time.Time{}
		c.setLastQR(nil)
		c.setLoginProgress("", nil)
	} else if state.VerificationRequired {
		if err := c.updateVerificationLocked(state.Verification); err != nil {
			return state, err
		}
		c.setLastQR(nil)
		c.setLoginProgress("verification_required", errVerificationRequired)
	} else if state.QRVisible {
		c.clearVerificationLocked()
		c.setLoginProgress("fetching", nil)
		c.loginHadQR = true
		c.loginUncertainSince = time.Time{}
	} else if c.loginUncertainSince.IsZero() {
		c.loginUncertainSince = time.Now()
	}
	return state, nil
}

// An indeterminate page after scanning is an SSO transition, not permission
// to clear Cookies/localStorage. Only a non-destructive navigation is used if
// it stays stuck after a grace period. Extra identity verification is left intact.
func (c *Client) handleLoginPendingLocked(ctx context.Context, state loginObservation) error {
	if state.VerificationRequired {
		c.setLastQR(nil)
		c.setLoginProgress("verification_required", errVerificationRequired)
		if c.opt.AutoReceiveSMS && state.Verification != nil && state.Verification.Step == "choose_method" && !time.Now().Before(c.smsResendAt) {
			if err := c.prepareSMSLocked(ctx, false); err != nil {
				c.lg.Log("! 请求接收短信验证码未完成：%v", err)
			}
		}
		return errVerificationRequired
	}
	c.setLoginProgress("logging_in", nil)
	if c.loginUncertainSince.IsZero() {
		c.loginUncertainSince = time.Now()
	}
	grace := 15 * time.Second
	if c.loginHadQR || state.Authenticated {
		grace = 90 * time.Second
	}
	if time.Since(c.loginUncertainSince) < grace || time.Since(c.lastLoginRecovery) < 30*time.Second {
		return errAuthPending
	}
	c.lastLoginRecovery = time.Now()
	c.loginUncertainSince = time.Now()
	path := filepath.Join(c.base, "shots", "login_pending.png")
	saveShot(c.page.Context(ctx), path)
	c.lg.Log("登录页面持续未就绪（页面=%s，保留 Cookie 和站点存储），仅重新打开私信页；截图 %s", state.PageURL, path)
	if err := c.navigateLocked(ctx); err != nil {
		c.setLoginProgress("error", err)
		return err
	}
	return errAuthPending
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) lockContext(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.mu.TryLock() {
			return nil
		}
		if err := waitContext(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
}

func (c *Client) navigateLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, stepNavigateTimeout)
	defer cancel()
	if c.page == nil {
		return fmt.Errorf("浏览器未连接")
	}
	c.loggedIn.Store(false)
	return c.page.Context(ctx).Navigate(chatURL)
}
