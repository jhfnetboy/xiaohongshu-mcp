package configs

var (
	useHeadless = true
	binPath     = ""
	connectURL  = "" // CDP WebSocket URL，非空时连接已有 Chrome
)

func InitHeadless(h bool) {
	useHeadless = h
}

func IsHeadless() bool {
	return useHeadless
}

func SetBinPath(b string) {
	binPath = b
}

func GetBinPath() string {
	return binPath
}

// SetConnectURL 设置 CDP 连接 URL（连接已有 Chrome）
func SetConnectURL(u string) {
	connectURL = u
}

// GetConnectURL 获取 CDP 连接 URL
func GetConnectURL() string {
	return connectURL
}
