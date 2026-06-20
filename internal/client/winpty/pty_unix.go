//go:build !windows

package winpty

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type unixPTY struct {
	tty *os.File // creack/pty.StartWithSize 返回值
	cmd *exec.Cmd
}

func startWithSize(cmd *exec.Cmd, cols, rows int) (PTY, error) {
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixPTY{tty: tty, cmd: cmd}, nil
}

func (u *unixPTY) Read(b []byte) (int, error)  { return u.tty.Read(b) }
func (u *unixPTY) Write(b []byte) (int, error) { return u.tty.Write(b) }

func (u *unixPTY) Close() error {
	// 关闭 tty 会让 EOF 传递到子进程，shell 通常随之退出。
	return u.tty.Close()
}

func (u *unixPTY) SetSize(cols, rows int) error {
	return pty.Setsize(u.tty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (u *unixPTY) Wait() error {
	if u.cmd.Process == nil {
		return nil
	}
	return u.cmd.Wait()
}
