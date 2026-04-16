package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// PublishImageContent 发布图文内容
type PublishImageContent struct {
	Title        string
	Content      string
	Tags         []string
	ImagePaths   []string
	ScheduleTime *time.Time // 定时发布时间，nil 表示立即发布
	IsOriginal   bool       // 是否声明原创
	Visibility   string     // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products     []string   // 商品关键词列表，用于绑定带货商品
}

type PublishAction struct {
	page *rod.Page
}

const (
	urlOfPublic = `https://creator.xiaohongshu.com/publish/publish?source=official`
)

func NewPublishImageAction(page *rod.Page) (*PublishAction, error) {
	// 不用 WaitLoad：小红书创作者平台是 SPA，load 事件可能永远不触发，
	// 会耗尽整个 timeout 预算导致后续所有操作 context 已过期
	pp := page.Timeout(600 * time.Second)

	// 确保 tab 处于前台（viewport 才有效）
	_, _ = pp.Activate()

	// 自动 dismiss Chrome 弹窗（如"确认离开页面"等），避免 CDP 命令被阻塞
	go pp.EachEvent(func(e *proto.PageJavascriptDialogOpening) {
		logrus.Warnf("[CDP] 自动 dismiss 弹窗: %s", e.Message)
		_ = proto.PageHandleJavaScriptDialog{Accept: false}.Call(pp)
	})()

	// getInfo 用 channel 包装 Info()，防止 CDP 阻塞挂死
	getInfo := func() *proto.TargetTargetInfo {
		ch := make(chan *proto.TargetTargetInfo, 1)
		go func() {
			info, _ := pp.Info()
			ch <- info
		}()
		select {
		case info := <-ch:
			return info
		case <-time.After(5 * time.Second):
			return nil
		}
	}

	// 已在 creator 发布页时跳过 Navigate，避免不必要的页面刷新
	if info := getInfo(); info == nil || !strings.Contains(info.URL, "creator.xiaohongshu.com/publish") {
		if err := pp.Navigate(urlOfPublic); err != nil {
			return nil, errors.Wrap(err, "导航到发布页面失败")
		}
		// 等待 SPA 渲染
		time.Sleep(5 * time.Second)
	} else {
		if info := getInfo(); info != nil {
			logrus.Infof("[CDP] 已在发布页，跳过 Navigate: %s", info.URL)
		}
	}

	// 快速检测：如果跳转到了登录页，说明 session 失效，立即返回错误
	loginInfo := getInfo()
	if loginInfo != nil && strings.Contains(loginInfo.URL, "/login") {
		return nil, errors.New("creator session 失效，请重新扫码登录（DELETE /api/v1/login/cookies 后重新获取二维码）")
	}

	// 从 CHROME_CONNECT_URL (如 http://localhost:9222) 解析 DevTools 地址
	devtoolsAddr := "localhost:9222"
	if cu := os.Getenv("CHROME_CONNECT_URL"); cu != "" {
		cu = strings.TrimPrefix(cu, "http://")
		cu = strings.TrimPrefix(cu, "https://")
		devtoolsAddr = cu
	}

	if err := mustClickPublishTab(pp, "上传图文", devtoolsAddr); err != nil {
		logrus.Errorf("点击上传图文 TAB 失败: %v", err)
		return nil, err
	}

	// 等待图文上传区出现（div.upload-content 在点击"上传图文"后才渲染）
	if _, err := pp.Timeout(20 * time.Second).Element(`div.upload-content`); err != nil {
		logrus.Warnf("等待图文上传区超时，继续尝试: %v", err)
	}
	time.Sleep(1 * time.Second)

	// debug: 截图记录切换后的页面状态
	debugAfterPath := "/tmp/debug_after_tab.png"
	if _, err := os.Stat("/app/data"); err == nil {
		debugAfterPath = "/app/data/debug_after_tab.png"
	}
	if img, err := pp.Screenshot(false, nil); err == nil {
		_ = os.WriteFile(debugAfterPath, img, 0644)
		logrus.Infof("[debug] 切换 TAB 后截图: %s", debugAfterPath)
	}

	return &PublishAction{
		page: pp,
	}, nil
}

func (p *PublishAction) Publish(ctx context.Context, content PublishImageContent) error {
	if len(content.ImagePaths) == 0 {
		return errors.New("图片不能为空")
	}

	page := p.page.Context(ctx)

	if err := uploadImages(page, content.ImagePaths); err != nil {
		return errors.Wrap(err, "小红书上传图片失败")
	}

	tags := content.Tags
	if len(tags) >= 10 {
		logrus.Warnf("标签数量超过10，截取前10个标签")
		tags = tags[:10]
	}

	logrus.Infof("发布内容: title=%s, images=%v, tags=%v, schedule=%v, original=%v, visibility=%s, products=%v", content.Title, len(content.ImagePaths), tags, content.ScheduleTime, content.IsOriginal, content.Visibility, content.Products)

	if err := submitPublish(page, content.Title, content.Content, tags, content.ScheduleTime, content.IsOriginal, content.Visibility, content.Products); err != nil {
		return errors.Wrap(err, "小红书发布失败")
	}

	return nil
}

// clickTabViaDevTools 通过 Chrome DevTools HTTP API 获取页面 WebSocket URL，
// 然后直接发送 Input.dispatchMouseEvent，绕过 go-rod browser-level session 的 Input domain 超时问题。
func clickTabViaDevTools(devtoolsAddr string, targetURL string, x, y float64) error {
	// 获取 tab 列表
	resp, err := http.Get(fmt.Sprintf("http://%s/json", devtoolsAddr))
	if err != nil {
		return errors.Wrap(err, "获取 Chrome tab 列表失败")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var tabs []struct {
		URL                  string `json:"url"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		Type                 string `json:"type"`
	}
	if err := json.Unmarshal(body, &tabs); err != nil {
		return errors.Wrap(err, "解析 tab 列表失败")
	}

	var wsURL string
	for _, t := range tabs {
		if t.Type == "page" && strings.Contains(t.URL, targetURL) {
			wsURL = t.WebSocketDebuggerURL
			break
		}
	}
	if wsURL == "" {
		return errors.Errorf("未找到包含 %s 的 tab", targetURL)
	}

	// 用 go 的标准库 WebSocket (golang.org/x/net/websocket 不可用，用 gorilla 也可能没装)
	// 用简单的 HTTP upgrade 自实现
	conn, err := dialWebSocket(wsURL)
	if err != nil {
		return errors.Wrap(err, "连接 tab WebSocket 失败")
	}
	defer conn.Close()

	send := func(id int, method string, params map[string]interface{}) error {
		msg := map[string]interface{}{"id": id, "method": method, "params": params}
		data, _ := json.Marshal(msg)
		return wsSend(conn, data)
	}
	recv := func() (map[string]interface{}, error) {
		data, err := wsRecv(conn)
		if err != nil {
			return nil, err
		}
		var m map[string]interface{}
		_ = json.Unmarshal(data, &m)
		return m, nil
	}

	mouseParams := func(typ string) map[string]interface{} {
		return map[string]interface{}{
			"type": typ, "x": x, "y": y,
			"button": "left", "clickCount": 1, "modifiers": 0,
		}
	}

	if err := send(1, "Input.dispatchMouseEvent", mouseParams("mousePressed")); err != nil {
		return err
	}
	if _, err := recv(); err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	if err := send(2, "Input.dispatchMouseEvent", mouseParams("mouseReleased")); err != nil {
		return err
	}
	if _, err := recv(); err != nil {
		return err
	}
	return nil
}

// dialWebSocket 简单实现 WebSocket 握手（不依赖第三方库）
func dialWebSocket(wsURL string) (net.Conn, error) {
	// ws://localhost:9222/devtools/page/XXX → localhost:9222 + /devtools/page/XXX
	u := strings.TrimPrefix(wsURL, "ws://")
	slash := strings.Index(u, "/")
	host := u[:slash]
	path := u[slash:]

	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}

	key := "dGhlIHNhbXBsZSBub25jZQ=="
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(string(buf[:n]), "101") {
		conn.Close()
		return nil, errors.New("WebSocket 握手失败")
	}
	return conn, nil
}

// wsSend 发送 WebSocket 文本帧（带 mask）
func wsSend(conn net.Conn, data []byte) error {
	frame := []byte{0x81}
	l := len(data)
	if l < 126 {
		frame = append(frame, byte(0x80|l))
	} else if l < 65536 {
		frame = append(frame, 0xFE, byte(l>>8), byte(l))
	}
	frame = append(frame, 0, 0, 0, 0) // mask key = 0
	frame = append(frame, data...)
	_, err := conn.Write(frame)
	return err
}

// wsRecv 接收 WebSocket 文本帧
func wsRecv(conn net.Conn) ([]byte, error) {
	conn.SetDeadline(time.Now().Add(8 * time.Second)) //nolint
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	length := int(header[1] & 0x7f)
	if length == 126 {
		ext := make([]byte, 2)
		if _, err := io.ReadFull(conn, ext); err != nil {
			return nil, err
		}
		length = int(ext[0])<<8 | int(ext[1])
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, err
	}
	return data, nil
}

func removePopCover(page *rod.Page) {

	// 先移除弹窗封面
	has, elem, err := page.Has("div.d-popover")
	if err != nil {
		return
	}
	if has {
		elem.MustRemove()
	}

	// 兜底：点击一下空位置吧
	clickEmptyPosition(page)
}

func clickEmptyPosition(page *rod.Page) {
	x := 380 + rand.Intn(100)
	y := 20 + rand.Intn(60)
	page.Mouse.MustMoveTo(float64(x), float64(y)).MustClick(proto.InputMouseButtonLeft)
}

// mustClickPublishTab 切换到目标发布 TAB。
// devtoolsAddr 用于直接发 Input domain 事件（go-rod browser session 的 Input domain 在 CDP 复用模式下可能超时）。
func mustClickPublishTab(page *rod.Page, tabname string, devtoolsAddr string) error {
	// debug: 异步截图，不阻塞主流程
	go func() {
		debugPngPath := "/tmp/debug_publish.png"
		if _, err := os.Stat("/app/data"); err == nil {
			debugPngPath = "/app/data/debug_publish.png"
		}
		if img, err := page.Screenshot(false, nil); err == nil {
			_ = os.WriteFile(debugPngPath, img, 0644)
			logrus.Infof("[debug] 截图已保存: %s", debugPngPath)
		}
	}()

	// waitElem 用 goroutine+channel 实现真正可超时的 Element 等待
	// go-rod 的 page.Timeout() 无法中断底层 WebSocket 读取，需要此包装
	waitElem := func(sel string, timeout time.Duration) (*rod.Element, error) {
		type result struct {
			elem *rod.Element
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			e, err := page.Element(sel)
			ch <- result{e, err}
		}()
		select {
		case r := <-ch:
			return r.elem, r.err
		case <-time.After(timeout):
			return nil, errors.Errorf("等待 %s 超时 (%v)", sel, timeout)
		}
	}

	// 等待 TAB 栏出现（最多 20 秒），确认 SPA 已渲染
	if _, err := waitElem(`div.creator-tab`, 20*time.Second); err != nil {
		return errors.Wrap(err, "页面未找到 TAB 栏（div.creator-tab），SPA 可能未渲染完成")
	}

	// 查找目标 TAB 并点击（最多重试 15 秒）
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		type elemsResult struct {
			elems rod.Elements
			err   error
		}
		ech := make(chan elemsResult, 1)
		go func() {
			e, err := page.Elements("div.creator-tab")
			ech <- elemsResult{e, err}
		}()

		var elems rod.Elements
		select {
		case r := <-ech:
			if r.err != nil {
				time.Sleep(300 * time.Millisecond)
				continue
			}
			elems = r.elems
		case <-time.After(5 * time.Second):
			logrus.Warn("获取 TAB 元素超时，重试")
			time.Sleep(300 * time.Millisecond)
			continue
		}

		var target *rod.Element
		for _, e := range elems {
			tch := make(chan string, 1)
			go func(el *rod.Element) {
				t, _ := el.Text()
				tch <- t
			}(e)
			var text string
			select {
			case text = <-tch:
			case <-time.After(3 * time.Second):
				continue
			}
			if strings.TrimSpace(text) == tabname {
				target = e
				break
			}
		}
		if target == nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}

		// 获取 target 元素的中心坐标，通过 DevTools WebSocket 发送 trusted mouse event
		type rectResult struct {
			x, y float64
			err  error
		}
		rch := make(chan rectResult, 1)
		go func() {
			box, err := target.Shape()
			if err != nil || len(box.Quads) == 0 {
				rch <- rectResult{err: errors.Wrap(err, "获取元素坐标失败")}
				return
			}
			center := box.Quads[0].Center()
			rch <- rectResult{x: center.X, y: center.Y}
		}()

		var tx, ty float64
		select {
		case r := <-rch:
			if r.err != nil {
				logrus.Warnf("获取 TAB 坐标失败: %v，重试", r.err)
				time.Sleep(300 * time.Millisecond)
				continue
			}
			tx, ty = r.x, r.y
		case <-time.After(5 * time.Second):
			logrus.Warn("获取 TAB 坐标超时，重试")
			time.Sleep(300 * time.Millisecond)
			continue
		}

		// 直接通过 page-level WebSocket 发 trusted Input event
		if err := clickTabViaDevTools(devtoolsAddr, "creator.xiaohongshu.com", tx, ty); err != nil {
			logrus.Warnf("DevTools 点击 TAB 失败: %v，重试", err)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		logrus.Infof("已点击发布 TAB: %s (%.0f,%.0f)", tabname, tx, ty)
		return nil
	}

	return errors.Errorf("没有找到发布 TAB - %s", tabname)
}

func getTabElement(page *rod.Page, tabname string) (*rod.Element, bool, error) {
	elems, err := page.Elements("div.creator-tab")
	if err != nil {
		return nil, false, err
	}

	for _, elem := range elems {
		if !isElementVisible(elem) {
			continue
		}

		text, err := elem.Text()
		if err != nil {
			logrus.Debugf("获取发布 TAB 文本失败: %v", err)
			continue
		}

		if strings.TrimSpace(text) != tabname {
			continue
		}

		blocked, err := isElementBlocked(elem)
		if err != nil {
			return nil, false, err
		}

		return elem, blocked, nil
	}

	return nil, false, nil
}

func isElementBlocked(elem *rod.Element) (bool, error) {
	result, err := elem.Eval(`() => {
		const rect = this.getBoundingClientRect();
		if (rect.width === 0 || rect.height === 0) {
			return true;
		}
		const x = rect.left + rect.width / 2;
		const y = rect.top + rect.height / 2;
		const target = document.elementFromPoint(x, y);
		return !(target === this || this.contains(target));
	}`)
	if err != nil {
		return false, err
	}

	return result.Value.Bool(), nil
}

func uploadImages(page *rod.Page, imagesPaths []string) error {
	// 验证文件路径有效性
	validPaths := make([]string, 0, len(imagesPaths))
	for _, path := range imagesPaths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			logrus.Warnf("图片文件不存在: %s", path)
			continue
		}
		validPaths = append(validPaths, path)
		logrus.Infof("获取有效图片：%s", path)
	}

	// 逐张上传：每张上传后等待预览出现，再上传下一张
	for i, path := range validPaths {
		selector := `input[type="file"]`
		if i == 0 {
			selector = ".upload-input"
		}

		uploadInput, err := page.Element(selector)
		if err != nil {
			return errors.Wrapf(err, "查找上传输入框失败(第%d张)", i+1)
		}
		if err := uploadInput.SetFiles([]string{path}); err != nil {
			return errors.Wrapf(err, "上传第%d张图片失败", i+1)
		}

		slog.Info("图片已提交上传", "index", i+1, "path", path)

		// 等待当前图片上传完成（预览元素数量达到 i+1），最多等 60 秒
		if err := waitForUploadComplete(page, i+1); err != nil {
			return errors.Wrapf(err, "第%d张图片上传超时", i+1)
		}
		time.Sleep(1 * time.Second)
	}

	return nil
}

// waitForUploadComplete 等待第 expectedCount 张图片上传完成，最多等 60 秒
func waitForUploadComplete(page *rod.Page, expectedCount int) error {
	maxWaitTime := 60 * time.Second
	checkInterval := 500 * time.Millisecond
	start := time.Now()
	lastLogCount := expectedCount - 1

	for time.Since(start) < maxWaitTime {
		uploadedImages, err := page.Elements(".img-preview-area .pr")
		if err != nil {
			time.Sleep(checkInterval)
			continue
		}

		currentCount := len(uploadedImages)
		// 数量变化时才打印，避免刷屏
		if currentCount != lastLogCount {
			slog.Info("等待图片上传", "current", currentCount, "expected", expectedCount)
			lastLogCount = currentCount
		}
		if currentCount >= expectedCount {
			slog.Info("图片上传完成", "count", currentCount)
			return nil
		}

		time.Sleep(checkInterval)
	}

	// debug: 记录页面 DOM，用于排查预览选择器是否失效
	if html, e := page.Eval(`() => {
		const area = document.querySelector('.img-preview-area');
		if (area) return '找到 .img-preview-area: ' + area.innerHTML.substring(0, 500);
		const divs = document.querySelectorAll('div[class*="preview"]');
		return '未找到 .img-preview-area，preview divs: ' + Array.from(divs).slice(0,5).map(d => d.className).join(' | ');
	}`); e == nil {
		logrus.Warnf("[debug] 上传超时 DOM 快照:\n%s", html.Value.String())
	}
	return errors.Errorf("第%d张图片上传超时(60s)，请检查网络连接和图片大小", expectedCount)
}

func submitPublish(page *rod.Page, title, content string, tags []string, scheduleTime *time.Time, isOriginal bool, visibility string, products []string) error {
	titleElem, err := page.Element("div.d-input input")
	if err != nil {
		return errors.Wrap(err, "查找标题输入框失败")
	}
	if err := titleElem.Input(title); err != nil {
		return errors.Wrap(err, "输入标题失败")
	}

	// 检查标题长度
	time.Sleep(500 * time.Millisecond)
	if err := checkTitleMaxLength(page); err != nil {
		return err
	}
	slog.Info("检查标题长度：通过")

	time.Sleep(1 * time.Second)

	contentElem, ok := getContentElement(page)
	if !ok {
		return errors.New("没有找到内容输入框")
	}
	if err := contentElem.Input(content); err != nil {
		return errors.Wrap(err, "输入正文失败")
	}
	if err := waitAndClickTitleInput(titleElem); err != nil {
		return err
	}
	if err := inputTags(contentElem, tags); err != nil {
		return err
	}

	time.Sleep(1 * time.Second)

	// 检查正文长度
	if err := checkContentMaxLength(page); err != nil {
		return err
	}
	slog.Info("检查正文长度：通过")

	// 处理定时发布
	if scheduleTime != nil {
		if err := setSchedulePublish(page, *scheduleTime); err != nil {
			return errors.Wrap(err, "设置定时发布失败")
		}
		slog.Info("定时发布设置完成", "schedule_time", scheduleTime.Format("2006-01-02 15:04"))
	}

	// 设置可见范围
	if err := setVisibility(page, visibility); err != nil {
		return errors.Wrap(err, "设置可见范围失败")
	}

	// 处理原创声明
	if isOriginal {
		if err := setOriginal(page); err != nil {
			slog.Warn("设置原创声明失败，继续发布", "error", err)
		} else {
			slog.Info("已声明原创")
		}
	}

	// 绑定商品
	if err := bindProducts(page, products); err != nil {
		return errors.Wrap(err, "绑定商品失败")
	}

	submitButton, err := page.Element(".publish-page-publish-btn button.bg-red")
	if err != nil {
		return errors.Wrap(err, "查找发布按钮失败")
	}
	if err := submitButton.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击发布按钮失败")
	}

	time.Sleep(3 * time.Second)
	return nil
}

// waitAndClickTitleInput 在填写正文后等待 1 秒并回点标题输入框，增强后续交互稳定性
func waitAndClickTitleInput(titleElem *rod.Element) error {
	slog.Info("正文填写完成，准备等待后回点标题输入框")
	time.Sleep(1 * time.Second)
	if err := titleElem.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "回点标题输入框失败")
	}
	slog.Info("已回点标题输入框，继续后续发布流程")
	return nil
}

// 检查标题是否超过最大长度
func checkTitleMaxLength(page *rod.Page) error {
	has, elem, err := page.Has(`div.title-container div.max_suffix`)
	if err != nil {
		return errors.Wrap(err, "检查标题长度元素失败")
	}

	// 元素不存在，说明标题没超长
	if !has {
		return nil
	}

	// 元素存在，说明标题超长
	titleLength, err := elem.Text()
	if err != nil {
		return errors.Wrap(err, "获取标题长度文本失败")
	}

	return makeMaxLengthError(titleLength)
}

func checkContentMaxLength(page *rod.Page) error {
	has, elem, err := page.Has(`div.edit-container div.length-error`)
	if err != nil {
		return errors.Wrap(err, "检查正文长度元素失败")
	}

	// 元素不存在，说明正文没超长
	if !has {
		return nil
	}

	// 元素存在，说明正文超长
	contentLength, err := elem.Text()
	if err != nil {
		return errors.Wrap(err, "获取正文长度文本失败")
	}

	return makeMaxLengthError(contentLength)
}

func makeMaxLengthError(elemText string) error {
	parts := strings.Split(elemText, "/")
	if len(parts) != 2 {
		return errors.Errorf("长度超过限制: %s", elemText)
	}

	currLen, maxLen := parts[0], parts[1]

	return errors.Errorf("当前输入长度为%s，最大长度为%s", currLen, maxLen)
}

// 查找内容输入框 - 使用Race方法处理两种样式
func getContentElement(page *rod.Page) (*rod.Element, bool) {
	var foundElement *rod.Element
	var found bool

	page.Race().
		Element("div.ql-editor").MustHandle(func(e *rod.Element) {
		foundElement = e
		found = true
	}).
		ElementFunc(func(page *rod.Page) (*rod.Element, error) {
			return findTextboxByPlaceholder(page)
		}).MustHandle(func(e *rod.Element) {
		foundElement = e
		found = true
	}).
		MustDo()

	if found {
		return foundElement, true
	}

	slog.Warn("no content element found by any method")
	return nil, false
}

func inputTags(contentElem *rod.Element, tags []string) error {
	if len(tags) == 0 {
		return nil
	}

	time.Sleep(1 * time.Second)

	for i := 0; i < 20; i++ {
		ka, err := contentElem.KeyActions()
		if err != nil {
			return errors.Wrap(err, "创建键盘操作失败")
		}
		if err := ka.Type(input.ArrowDown).Do(); err != nil {
			return errors.Wrap(err, "按下方向键失败")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ka, err := contentElem.KeyActions()
	if err != nil {
		return errors.Wrap(err, "创建键盘操作失败")
	}
	if err := ka.Press(input.Enter).Press(input.Enter).Do(); err != nil {
		return errors.Wrap(err, "按下回车键失败")
	}

	time.Sleep(1 * time.Second)

	for _, tag := range tags {
		tag = strings.TrimLeft(tag, "#")
		if err := inputTag(contentElem, tag); err != nil {
			return errors.Wrapf(err, "输入标签[%s]失败", tag)
		}
	}
	return nil
}

func inputTag(contentElem *rod.Element, tag string) error {
	if err := contentElem.Input("#"); err != nil {
		return errors.Wrap(err, "输入#失败")
	}
	time.Sleep(200 * time.Millisecond)

	for _, char := range tag {
		if err := contentElem.Input(string(char)); err != nil {
			return errors.Wrapf(err, "输入字符[%c]失败", char)
		}
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(1 * time.Second)

	page := contentElem.Page()
	topicContainer, err := page.Element("#creator-editor-topic-container")
	if err != nil || topicContainer == nil {
		slog.Warn("未找到标签联想下拉框，直接输入空格", "tag", tag)
		return contentElem.Input(" ")
	}

	firstItem, err := topicContainer.Element(".item")
	if err != nil || firstItem == nil {
		slog.Warn("未找到标签联想选项，直接输入空格", "tag", tag)
		return contentElem.Input(" ")
	}

	if err := firstItem.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击标签联想选项失败")
	}
	slog.Info("成功点击标签联想选项", "tag", tag)
	time.Sleep(200 * time.Millisecond)

	time.Sleep(500 * time.Millisecond) // 等待标签处理完成
	return nil
}

func findTextboxByPlaceholder(page *rod.Page) (*rod.Element, error) {
	elements := page.MustElements("p")
	if elements == nil {
		return nil, errors.New("no p elements found")
	}

	// 查找包含指定placeholder的元素
	placeholderElem := findPlaceholderElement(elements, "输入正文描述")
	if placeholderElem == nil {
		return nil, errors.New("no placeholder element found")
	}

	// 向上查找textbox父元素
	textboxElem := findTextboxParent(placeholderElem)
	if textboxElem == nil {
		return nil, errors.New("no textbox parent found")
	}

	return textboxElem, nil
}

func findPlaceholderElement(elements []*rod.Element, searchText string) *rod.Element {
	for _, elem := range elements {
		placeholder, err := elem.Attribute("data-placeholder")
		if err != nil || placeholder == nil {
			continue
		}

		if strings.Contains(*placeholder, searchText) {
			return elem
		}
	}
	return nil
}

func findTextboxParent(elem *rod.Element) *rod.Element {
	currentElem := elem
	for i := 0; i < 5; i++ {
		parent, err := currentElem.Parent()
		if err != nil {
			break
		}

		role, err := parent.Attribute("role")
		if err != nil || role == nil {
			currentElem = parent
			continue
		}

		if *role == "textbox" {
			return parent
		}

		currentElem = parent
	}
	return nil
}

// isElementVisible 检查元素是否可见
func isElementVisible(elem *rod.Element) bool {

	// 检查是否有隐藏样式
	style, err := elem.Attribute("style")
	if err == nil && style != nil {
		styleStr := *style

		if strings.Contains(styleStr, "left: -9999px") ||
			strings.Contains(styleStr, "top: -9999px") ||
			strings.Contains(styleStr, "position: absolute; left: -9999px") ||
			strings.Contains(styleStr, "display: none") ||
			strings.Contains(styleStr, "visibility: hidden") {
			return false
		}
	}

	visible, err := elem.Visible()
	if err != nil {
		slog.Warn("无法获取元素可见性", "error", err)
		return true
	}

	return visible
}

// setVisibility 设置可见范围
// 支持: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
func setVisibility(page *rod.Page, visibility string) error {
	if visibility == "" || visibility == "公开可见" {
		slog.Info("可见范围使用默认：公开可见")
		return nil
	}

	// 支持的选项校验
	supported := map[string]bool{"仅自己可见": true, "仅互关好友可见": true}
	if !supported[visibility] {
		return errors.Errorf("不支持的可见范围: %s，支持: 公开可见、仅自己可见、仅互关好友可见", visibility)
	}

	// 点击可见范围下拉框
	dropdown, err := page.Element("div.permission-card-wrapper div.d-select-content")
	if err != nil {
		return errors.Wrap(err, "查找可见范围下拉框失败")
	}
	if err := dropdown.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击可见范围下拉框失败")
	}
	time.Sleep(500 * time.Millisecond)

	// 在弹窗中查找并点击目标选项
	opts, err := page.Elements("div.d-options-wrapper div.d-grid-item div.custom-option")
	if err != nil {
		return errors.Wrap(err, "查找可见范围选项失败")
	}
	for _, opt := range opts {
		text, err := opt.Text()
		if err != nil {
			continue
		}
		if strings.Contains(text, visibility) {
			if err := opt.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return errors.Wrap(err, "选择可见范围失败")
			}
			slog.Info("已设置可见范围", "visibility", visibility)
			time.Sleep(200 * time.Millisecond)
			return nil
		}
	}
	return errors.Errorf("未找到可见范围选项: %s", visibility)
}

// setSchedulePublish 设置定时发布时间
func setSchedulePublish(page *rod.Page, t time.Time) error {
	// 1. 点击定时发布开关
	if err := clickScheduleSwitch(page); err != nil {
		return err
	}
	time.Sleep(800 * time.Millisecond)

	// 2. 设置日期时间
	if err := setDateTime(page, t); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)

	return nil
}

// clickScheduleSwitch 点击定时发布开关
func clickScheduleSwitch(page *rod.Page) error {
	switchElem, err := page.Element(".post-time-wrapper .d-switch")
	if err != nil {
		return errors.Wrap(err, "查找定时发布开关失败")
	}

	if err := switchElem.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击定时发布开关失败")
	}
	slog.Info("已点击定时发布开关")
	return nil
}

// setDateTime 设置日期时间
func setDateTime(page *rod.Page, t time.Time) error {
	dateTimeStr := t.Format("2006-01-02 15:04")

	input, err := page.Element(".date-picker-container input")
	if err != nil {
		return errors.Wrap(err, "查找日期时间输入框失败")
	}

	if err := input.SelectAllText(); err != nil {
		return errors.Wrap(err, "选择日期时间文本失败")
	}
	if err := input.Input(dateTimeStr); err != nil {
		return errors.Wrap(err, "输入日期时间失败")
	}
	slog.Info("已设置日期时间", "datetime", dateTimeStr)

	return nil
}

// setOriginal 设置原创声明
func setOriginal(page *rod.Page) error {
	// 根据小红书创作者页面的实际结构：
	// div.custom-switch-card 包含 span.has-tips 文本为"原创声明"
	// 开关是 div.d-switch 组件

	// 查找包含"原创声明"文本的 custom-switch-card
	switchCards, err := page.Elements("div.custom-switch-card")
	if err != nil {
		return errors.Wrap(err, "查找原创声明卡片失败")
	}

	for _, card := range switchCards {
		text, err := card.Text()
		if err != nil {
			continue
		}

		// 检查是否是原创声明卡片
		if !strings.Contains(text, "原创声明") {
			continue
		}

		// 找到原创声明卡片，查找其中的 d-switch
		switchElem, err := card.Element("div.d-switch")
		if err != nil {
			continue
		}

		// 检查开关是否已打开
		checked, err := switchElem.Eval(`() => {
			const input = this.querySelector('input[type="checkbox"]');
			return input ? input.checked : false;
		}`)
		if err != nil {
			continue
		}

		if checked.Value.Bool() {
			slog.Info("原创声明已开启")
			return nil
		}

		// 点击开关
		if err := switchElem.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return errors.Wrap(err, "点击原创声明开关失败")
		}

		time.Sleep(500 * time.Millisecond)

		// 处理原创声明确认弹窗
		if err := confirmOriginalDeclaration(page); err != nil {
			return errors.Wrap(err, "确认原创声明失败")
		}

		slog.Info("已开启原创声明")
		return nil
	}

	return errors.New("未找到原创声明选项")
}

// confirmOriginalDeclaration 处理原创声明确认弹窗
func confirmOriginalDeclaration(page *rod.Page) error {
	// 等待确认弹窗出现
	time.Sleep(800 * time.Millisecond)

	// 使用 JavaScript 直接处理弹窗，更可靠
	result, err := page.Eval(`
		() => {
			// 查找包含"原创声明须知"的 footer 区域
			const footers = document.querySelectorAll('div.footer');
			for (const footer of footers) {
				// 检查是否包含原创声明相关内容
				if (!footer.textContent.includes('原创声明须知')) {
					continue;
				}

				// 找到 checkbox 并勾选
				const checkbox = footer.querySelector('div.d-checkbox input[type="checkbox"]');
				if (checkbox && !checkbox.checked) {
					checkbox.click();
					console.log('已勾选原创声明须知 checkbox');
				}

				// 等待一下让按钮变为可用
				return 'found_footer';
			}
			return 'footer_not_found';
		}
	`)
	if err != nil {
		slog.Warn("执行查找弹窗脚本失败", "error", err)
	} else if result.Value.String() == "footer_not_found" {
		slog.Warn("未找到原创声明确认弹窗的 footer")
	}

	time.Sleep(500 * time.Millisecond)

	// 再次使用 JavaScript 点击声明原创按钮
	result2, err := page.Eval(`
		() => {
			const footers = document.querySelectorAll('div.footer');
			for (const footer of footers) {
				if (!footer.textContent.includes('声明原创')) {
					continue;
				}

				// 找到声明原创按钮
				const btn = footer.querySelector('button.custom-button');
				if (btn) {
					// 检查是否禁用
					if (btn.classList.contains('disabled') || btn.disabled) {
						// 尝试再次勾选 checkbox
						const checkbox = footer.querySelector('div.d-checkbox input[type="checkbox"]');
						if (checkbox && !checkbox.checked) {
							checkbox.click();
						}
						return 'button_disabled';
					}
					btn.click();
					return 'clicked';
				}
			}
			return 'button_not_found';
		}
	`)
	if err != nil {
		return errors.Wrap(err, "执行点击按钮脚本失败")
	}

	status := result2.Value.String()
	slog.Info("原创声明确认结果", "status", status)

	if status == "button_not_found" {
		return errors.New("未找到声明原创按钮")
	}
	if status == "button_disabled" {
		return errors.New("声明原创按钮仍处于禁用状态")
	}

	slog.Info("已成功点击声明原创按钮")
	time.Sleep(300 * time.Millisecond)

	return nil
}

// bindProducts 绑定商品到发布内容
func bindProducts(page *rod.Page, products []string) error {
	if len(products) == 0 {
		return nil
	}

	slog.Info("开始绑定商品", "products", products)

	// 点击"添加商品"按钮
	if err := clickAddProductButton(page); err != nil {
		return errors.Wrap(err, "点击添加商品按钮失败")
	}
	time.Sleep(1 * time.Second)

	// 等待商品选择弹窗出现
	modal, err := waitForProductModal(page)
	if err != nil {
		return errors.Wrap(err, "等待商品弹窗失败")
	}
	slog.Info("商品选择弹窗已打开")

	// 遍历搜索并选择商品
	var failedProducts []string
	for _, keyword := range products {
		if err := searchAndSelectProduct(page, modal, keyword); err != nil {
			slog.Warn("搜索选择商品失败", "keyword", keyword, "error", err)
			failedProducts = append(failedProducts, keyword)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 点击保存按钮
	slog.Info("准备点击保存按钮")
	if err := clickModalSaveButton(page, modal); err != nil {
		return errors.Wrap(err, "点击保存按钮失败")
	}
	slog.Info("保存按钮点击完成，开始等待弹窗关闭")

	// 等待弹窗关闭
	if err := waitForModalClose(page); err != nil {
		slog.Warn("等待弹窗关闭超时", "error", err)
	} else {
		slog.Info("弹窗已关闭")
	}

	if len(failedProducts) > 0 {
		return errors.Errorf("部分商品未找到: %v", failedProducts)
	}

	slog.Info("商品绑定完成", "total", len(products))
	time.Sleep(1000 * time.Millisecond)
	return nil
}

// clickAddProductButton 点击"添加商品"按钮
func clickAddProductButton(page *rod.Page) error {
	slog.Info("开始查找添加商品按钮")

	// 查找包含"添加商品"文本的元素
	spans, err := page.Elements("span.d-text")
	if err != nil {
		return errors.Wrap(err, "查找商品按钮文本失败")
	}

	for _, span := range spans {
		text, err := span.Text()
		if err != nil {
			continue
		}
		if strings.TrimSpace(text) == "添加商品" {
			slog.Info("找到添加商品文本，向上查找可点击父元素")
			// 向上查找可点击的父元素
			parent := span
			for i := 0; i < 5; i++ {
				p, err := parent.Parent()
				if err != nil {
					break
				}
				parent = p

				tagName, err := parent.Eval(`() => this.tagName.toLowerCase()`)
				if err != nil {
					continue
				}
				tag := tagName.Value.Str()

				// 检查是否为 button 或含 d-button class
				if tag == "button" {
					if err := parent.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return errors.Wrap(err, "点击添加商品按钮失败")
					}
					slog.Info("已点击添加商品按钮")
					time.Sleep(300 * time.Millisecond) // 确保弹窗动画开始
					return nil
				}

				cls, _ := parent.Attribute("class")
				if cls != nil && strings.Contains(*cls, "d-button") {
					if err := parent.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return errors.Wrap(err, "点击添加商品按钮失败")
					}
					slog.Info("已点击添加商品按钮")
					time.Sleep(300 * time.Millisecond) // 确保弹窗动画开始
					return nil
				}
			}
		}
	}

	return errors.New("未找到添加商品按钮，账号可能未开通商品功能")
}

// waitForProductModal 等待商品选择弹窗出现
func waitForProductModal(page *rod.Page) (*rod.Element, error) {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		modal, err := page.Element(".multi-goods-selector-modal")
		if err == nil && modal != nil {
			visible, _ := modal.Visible()
			if visible {
				slog.Info("商品选择弹窗已出现")
				return modal, nil
			}
		}
		time.Sleep(100 * time.Millisecond) // 缩短轮询间隔，更快响应
	}

	return nil, errors.New("等待商品选择弹窗超时")
}

// searchAndSelectProduct 搜索并选择商品
func searchAndSelectProduct(page *rod.Page, modal *rod.Element, keyword string) error {
	slog.Info("搜索商品", "keyword", keyword)

	// 1. 获取搜索框
	searchInput, err := modal.Element(`input[placeholder="搜索商品ID 或 商品名称"]`)
	if err != nil {
		return errors.Wrap(err, "未找到商品搜索框")
	}

	// 2. 清空并输入关键词（使用原生 JS setter + 完整事件）
	if err := searchInput.SelectAllText(); err != nil {
		slog.Warn("选择搜索框文本失败", "error", err)
	}
	time.Sleep(100 * time.Millisecond)

	// 使用 rod Input 输入关键词
	if err := searchInput.Input(keyword); err != nil {
		return errors.Wrap(err, "输入搜索关键词失败")
	}
	time.Sleep(300 * time.Millisecond)

	// 3. 触发搜索（模拟键盘 Enter）
	if err := page.Keyboard.Press(input.Enter); err != nil {
		return errors.Wrap(err, "触发搜索失败")
	}

	// 4. 等待搜索结果加载
	time.Sleep(1 * time.Second)

	// 等待 loading 消失（使用与工作代码相同的选择器）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		loading, err := modal.Element(".goods-list-loading")
		if err != nil || loading == nil {
			break
		}
		visible, _ := loading.Visible()
		if !visible {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 等待商品列表渲染完成（使用与工作代码相同的选择器）
	for time.Now().Before(deadline) {
		productList, err := modal.Element(".goods-list-normal .good-card-container")
		if err == nil && productList != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // 额外等待确保渲染完成

	// 5. 点击第一个商品的 checkbox（使用与工作代码相同的选择器）
	checkbox, err := modal.Element(".goods-list-normal .good-card-container .d-checkbox")
	if err != nil {
		return errors.Wrap(err, "未找到商品选择框")
	}

	// 检查是否已经选中
	isChecked, err := checkbox.Eval(`(el) => {
		return el.querySelector('.d-checkbox-simulator.checked') !== null ||
			   el.querySelector('input[type="checkbox"]:checked') !== null;
	}`)
	if err == nil && isChecked.Value.Bool() {
		slog.Info("商品已选中，跳过", "keyword", keyword)
		return nil
	}

	if err := checkbox.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击商品选择框失败")
	}

	// 6. 随机延迟模拟人为操作（800-1500ms）
	randomDelay := 800 + rand.Intn(700)
	time.Sleep(time.Duration(randomDelay) * time.Millisecond)

	slog.Info("已选择商品", "keyword", keyword)
	return nil
}

// clickModalSaveButton 点击保存按钮
func clickModalSaveButton(page *rod.Page, modal *rod.Element) error {
	// 查找保存按钮（参考工作代码：直接查找并点击，不强制要求找到）
	btn, err := modal.Element(".goods-selected-footer button")
	if err == nil && btn != nil {
		if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
			slog.Warn("点击保存按钮失败", "error", err)
		} else {
			slog.Info("已点击保存按钮")
			return nil
		}
	}

	// 尝试点击主按钮
	primaryBtn, err := modal.Element(".goods-selected-footer .d-button--primary")
	if err == nil && primaryBtn != nil {
		if err := primaryBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
			slog.Warn("点击主按钮失败", "error", err)
		} else {
			slog.Info("已点击主按钮")
			return nil
		}
	}

	slog.Warn("未找到保存按钮，继续执行")
	return nil
}

// waitForModalClose 等待弹窗关闭
func waitForModalClose(page *rod.Page) error {
	deadline := time.Now().Add(5 * time.Second)
	slog.Info("开始等待弹窗关闭")

	for time.Now().Before(deadline) {
		// 使用 Has 代替 Element，避免等待元素出现的阻塞
		has, _, err := page.Has(".multi-goods-selector-modal")
		if err != nil || !has {
			slog.Info("弹窗已关闭")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return errors.New("等待弹窗关闭超时")
}
