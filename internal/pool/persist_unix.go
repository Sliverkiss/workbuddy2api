// persist_unix.go 平台相关：从 os.FileInfo 取属主 uid/gid（Unix）。
// 非 Unix 平台或 Sys() 返回 nil（测试 fake）时回落 0，仅影响日志可读性。
package pool

import (
	"os"
	"syscall"
)

// statUID 返回文件属主 uid；取不到（非 Unix / 测试 fake）返回 0。
func statUID(info os.FileInfo) int {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid)
	}
	return 0
}

// statGID 返回文件属主 gid；取不到返回 0。
func statGID(info os.FileInfo) int {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(s.Gid)
	}
	return 0
}
