// Package winpty 在 Windows 上用 ConPTY、其它平台转给 creack/pty，
// 给 handlers 提供统一的 PTY 接口。
package winpty

import (
	"io"
	"os/exec"
)

// PTY 是 PTY 抽象：在 io.ReadWriteCloser 之上加 SetSize 与 Wait。
//
//   - Read：从 PTY 输出端读（child stdout/stderr 合流）。
//   - Write：写到 PTY 输入端（child stdin）。
//   - SetSize：调整 PTY 列行数。
//   - Wait：阻塞到子进程退出；多次调用安全。
type PTY interface {
	io.ReadWriteCloser
	SetSize(cols, rows int) error
	Wait() error
}

// StartWithSize 启动 cmd，挂到新建的 PTY 上，返回 PTY 句柄。
func StartWithSize(cmd *exec.Cmd, cols, rows int) (PTY, error) {
	return startWithSize(cmd, cols, rows)
}
