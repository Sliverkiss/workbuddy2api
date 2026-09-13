//go:build !unix

// persist_other.go 非 Unix 平台（Windows 等）回落：无 POSIX uid/gid 语义，
// 恒返回 0（仅影响首错日志的属主信息可读性）。
// syscall.Stat_t 在 Windows 不存在，本文件与 persist_unix.go 由构建标签互斥。
package pool

import (
	"os"
)

// statUID 非 Unix 平台恒 0。
func statUID(info os.FileInfo) int { return 0 }

// statGID 非 Unix 平台恒 0。
func statGID(info os.FileInfo) int { return 0 }
