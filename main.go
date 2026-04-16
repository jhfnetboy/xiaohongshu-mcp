package main

import (
	"flag"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
)

func main() {
	var (
		headless   bool
		binPath    string // 浏览器二进制文件路径
		port       string
		connectURL string // 连接已有 Chrome 的 CDP URL（如 ws://127.0.0.1:9222）
	)
	flag.BoolVar(&headless, "headless", true, "是否无头模式")
	flag.StringVar(&binPath, "bin", "", "浏览器二进制文件路径")
	flag.StringVar(&port, "port", ":18060", "端口")
	flag.StringVar(&connectURL, "connect", "", "连接已有 Chrome 的 CDP WebSocket URL")
	flag.Parse()

	if len(binPath) == 0 {
		binPath = os.Getenv("ROD_BROWSER_BIN")
	}
	if len(connectURL) == 0 {
		connectURL = os.Getenv("CHROME_CONNECT_URL")
	}

	configs.InitHeadless(headless)
	configs.SetBinPath(binPath)
	configs.SetConnectURL(connectURL)

	// 初始化服务
	xiaohongshuService := NewXiaohongshuService()

	// CDP 模式：连接已有 Chrome（如果配置了 -connect 或 CHROME_CONNECT_URL）
	xiaohongshuService.InitCDPSession()

	// 创建并启动应用服务器
	appServer := NewAppServer(xiaohongshuService)
	if err := appServer.Start(port); err != nil {
		logrus.Fatalf("failed to run server: %v", err)
	}
}
