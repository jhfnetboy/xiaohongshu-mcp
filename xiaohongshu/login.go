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
// 不使用 WaitNavigation，避免 DOMContentLoaded 事件已触发导致永久阻塞
func navigatePage(page *rod.Page, url string) error {
	if err := page.Navigate(url); err != nil {
		return errors.Wrap(err, "navigate failed")
	}
	time.Sleep(4 * time.Second)
	return nil
}

func (a *LoginAction) CheckLoginStatus(ctx context.Context) (bool, error) {
	pp := a.page.Context(ctx)

	if err := navigatePage(pp, "https://www.xiaohongshu.com/explore"); err != nil {
		return false, err
	}
	time.Sleep(1 * time.Second)

	exists, _, err := pp.Has(`.main-container .user .link-wrapper .channel`)
	if err != nil {
		return false, errors.Wrap(err, "check login status failed")
	}
	if !exists {
		return false, nil
	}
	return true, nil
}

func (a *LoginAction) Login(ctx context.Context) error {
	pp := a.page.Context(ctx)

	if err := navigatePage(pp, "https://www.xiaohongshu.com/explore"); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)

	if exists, _, _ := pp.Has(".main-container .user .link-wrapper .channel"); exists {
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
	if exists, _, _ := pp.Has(".main-container .user .link-wrapper .channel"); exists {
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
	// 每 3 秒重新导航一次检测登录态，比等待当前页面更可靠
	// （扫码登录后当前页面不会自动刷新成已登录状态）
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
			time.Sleep(1 * time.Second)
			exists, _, _ := pp.Has(".main-container .user .link-wrapper .channel")
			if exists {
				return true
			}
		}
	}
}
