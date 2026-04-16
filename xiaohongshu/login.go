package xiaohongshu

import (
	"context"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
)

type LoginAction struct {
	page *rod.Page
}

func NewLogin(page *rod.Page) *LoginAction {
	return &LoginAction{page: page}
}

// navigatePage 导航到指定 URL，Navigate 后 sleep 等待 SPA 初始渲染
func navigatePage(page *rod.Page, url string) error {
	if err := page.Navigate(url); err != nil {
		return errors.Wrap(err, "navigate failed")
	}
	time.Sleep(4 * time.Second)
	return nil
}

// isLoggedIn 检测登录状态：优先查 web_session cookie，兜底查 DOM 元素
func isLoggedIn(pp *rod.Page) bool {
	// 方法1：检查 web_session cookie（更可靠，不依赖 DOM 结构）
	cookies, err := pp.Cookies([]string{"https://www.xiaohongshu.com"})
	if err == nil {
		for _, c := range cookies {
			if c.Name == "web_session" && c.Value != "" {
				return true
			}
		}
	}
	// 方法2：检查已登录用户 DOM 元素（兜底）
	exists, _, _ := pp.Has(".main-container .user .link-wrapper .channel")
	return exists
}

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	pp := a.page.Context(ctx)

	if err := navigatePage(pp, "https://www.xiaohongshu.com/explore"); err != nil {
		return false, err
	}

	return isLoggedIn(pp), nil
}

func (a *LoginAction) Login(ctx context.Context) error {
	pp := a.page.Context(ctx)

	if err := navigatePage(pp, "https://www.xiaohongshu.com/explore"); err != nil {
		return err
	}

	if isLoggedIn(pp) {
		return nil
	}

	pp.MustElement(".main-container .user .link-wrapper .channel")
	return nil
}

func (a *LoginAction) FetchQrcodeImage(ctx context.Context) (string, bool, error) {
	pp := a.page.Context(ctx)

	if err := navigatePage(pp, "https://www.xiaohongshu.com/explore"); err != nil {
		return "", false, err
	}
	// 等待 JS 渲染
	time.Sleep(3 * time.Second)

	// 已登录则直接返回
	if isLoggedIn(pp) {
		return "", true, nil
	}

	// 点击"创作中心"按钮触发登录弹窗（5秒内找不到则跳过）
	if btn, err := pp.Timeout(5 * time.Second).Element(".reds-button-new.channel-btn"); err == nil {
		_ = btn.Click(proto.InputMouseButtonLeft, 1)
		time.Sleep(3 * time.Second)
	}

	// 依次尝试新旧二维码选择器
	qrcodeSelectors := []string{
		".qrcode-img-box .qrcode-img",
		".qrcode-img",
		".login-container .qrcode-img",
	}

	var src *string
	var lastErr error
	for _, sel := range qrcodeSelectors {
		el, err := pp.Timeout(15 * time.Second).Element(sel)
		if err != nil {
			lastErr = err
			continue
		}
		s, err := el.Attribute("src")
		if err != nil || s == nil || *s == "" {
			lastErr = errors.New("qrcode src is empty")
			continue
		}
		src = s
		lastErr = nil
		break
	}
	if lastErr != nil {
		return "", false, errors.Wrap(lastErr, "get qrcode failed")
	}

	return *src, false, nil
}

func (a *LoginAction) WaitForLogin(ctx context.Context) bool {
	// 每 3 秒重新导航检测登录态（扫码后当前页面不自动刷新）
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			pp := a.page.Context(ctx)
			if err := navigatePage(pp, "https://www.xiaohongshu.com/explore"); err != nil {
				continue
			}
			if isLoggedIn(pp) {
				return true
			}
		}
	}
}
