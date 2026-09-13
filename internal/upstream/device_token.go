// device_token.go X-Device-Token 的文件兜底读取（与桌面端共用状态文件）。
//
// 容器内无桌面端 Turing Shield SDK，无法像 Python fork（xiaofan6ya/converter.py）那样
// 现取 token。这里提供另一条路径：宿主把桌面端生成的 device token 落 /app/data/device_token
// （或任意挂载路径），网关定期读取注入。读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略
// （优雅降级不注入，不影响主流程）。
package upstream

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

var errDeviceTokenTooLarge = errors.New("device token file too large")

// deviceTokenFile 文件读取缓存 TTL（秒）。桌面端 SDK 自身也有缓存，这里再兜一层
// 避免每次出站请求都 stat+read 文件。
const deviceTokenFileTTL = 5 * time.Minute

// deviceTokenFileMaxLen token 文件最大字节数。token 通常几百字节；超过 1KB
// 视为异常（非 token 内容 / 文件被误用），忽略不注入。
const deviceTokenFileMaxLen = 1024

// deviceTokenFileCache 缓存 device token 文件读取结果（path → token+读取时刻）。
type deviceTokenFileCache struct {
	mu       sync.Mutex
	path     string
	token    string
	readAt   time.Time
	lastErr  error
}

var dtFileCache = &deviceTokenFileCache{}

// readDeviceTokenFile 读取并缓存 device token 文件；5 分钟内复用上次结果。
// 返回空串表示无可用 token（文件未配置 / 读失败 / 内容过长 / 空白）。
func readDeviceTokenFile(path string) string {
	if path == "" {
		return ""
	}
	// 快路径：缓存命中且未过期，直接返回（不持锁读返回值，仅判读时间）。
	dtFileCache.mu.Lock()
	defer dtFileCache.mu.Unlock()
	if path == dtFileCache.path && time.Since(dtFileCache.readAt) < deviceTokenFileTTL {
		return dtFileCache.token
	}
	// 缓存未命中或过期：重新读文件。
	dtFileCache.path = path
	tok, err := readTrimmedFile(path, deviceTokenFileMaxLen)
	if err != nil {
		// 读失败：清空缓存 token，避免注入过期/错误的值。
		dtFileCache.token = ""
		dtFileCache.lastErr = err
		dtFileCache.readAt = time.Now()
		return ""
	}
	dtFileCache.token = tok
	dtFileCache.lastErr = nil
	dtFileCache.readAt = time.Now()
	return tok
}

// readTrimmedFile 读文件并 trim 首尾空白，超过 maxLen 返回错误（拒绝过长内容）。
func readTrimmedFile(path string, maxLen int) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > int64(maxLen) {
		return "", errDeviceTokenTooLarge
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
