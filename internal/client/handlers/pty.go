package handlers

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lujiang/lujiang/internal/client/shell"
	"github.com/lujiang/lujiang/internal/client/winpty"
	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// PTYManager 维护客户端侧活跃 pty 的注册表。
//
// 每个 pty 用一个 string ID 标识，便于 resize / close RPC 找回。
// 数据帧直接在 pty.create 的 stream 上收发，不走 Manager。
type PTYManager struct {
	log  *slog.Logger
	mu   sync.Mutex
	next atomic.Int64
	pts  map[string]*ptyEntry
}

// NewPTYManager 构造空 Manager。
func NewPTYManager(log *slog.Logger) *PTYManager {
	return &PTYManager{log: log, pts: map[string]*ptyEntry{}}
}

type ptyEntry struct {
	id     string
	pty    winpty.PTY
	cmd    *exec.Cmd
	closed atomic.Bool
}

// HandleCreate 实现 pty.create：spawn pty、回写 PtyID、随后在 stream 上双向转发。
//
// 该 handler 在 stream 关闭或对端 EOF 之前阻塞，关闭时 kill 子进程并清理。
func (m *PTYManager) HandleCreate(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.PTYCreateReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	if req.Shell == "" {
		req.Shell = shell.Default()
	}
	if req.Cols <= 0 {
		req.Cols = 80
	}
	if req.Rows <= 0 {
		req.Rows = 24
	}

	cmd, err := buildCmd(req)
	if err != nil {
		return writePTYError(stream, err)
	}
	pt, err := winpty.StartWithSize(cmd, req.Cols, req.Rows)
	if err != nil {
		return writePTYError(stream, fmt.Errorf("pty start: %w", err))
	}

	id := m.alloc(cmd, pt)
	m.log.Info("pty created", "id", id, "shell", req.Shell, "cols", req.Cols, "rows", req.Rows, "pid", cmd.Process.Pid)

	// 回写响应：写入后 stream 进入"原始帧"阶段（不再是 JSON RPC）。
	if err := WriteJSONRes(stream, proto.PTYCreateRes{PtyID: id}); err != nil {
		m.release(id)
		return err
	}

	// stream → pty（stdin）：读到 EOF 时关闭 pty 的写端，触发 shell 退出。
	go func() {
		for {
			payload, err := tunnelmux.ReadFrame(stream)
			if err != nil {
				break
			}
			if _, err := pt.Write(payload); err != nil {
				break
			}
		}
		_ = pt.Close()
	}()

	// pty（stdout/stderr） → stream：读到 EOF 时关 stream。
	// 写循环退出后再 release，避免 race。
	_, _ = pumpPTYToStream(pt, stream)
	m.release(id)
	return nil
}

// HandleResize 实现 pty.resize 短 RPC。
func (m *PTYManager) HandleResize(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.PTYResizeReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	entry, ok := m.lookup(req.PtyID)
	if !ok {
		return writePTYError(stream, fmt.Errorf("pty not found: %s", req.PtyID))
	}
	if req.Cols <= 0 || req.Rows <= 0 {
		return writePTYError(stream, fmt.Errorf("invalid resize %dx%d", req.Cols, req.Rows))
	}
	if err := entry.pty.SetSize(req.Cols, req.Rows); err != nil {
		return writePTYError(stream, err)
	}
	return WriteJSONRes(stream, struct{}{})
}

// HandleClose 实现 pty.close 短 RPC。kill 子进程；stream 自身的清理在
// HandleCreate 的写循环退出时进行。
func (m *PTYManager) HandleClose(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.PTYCloseReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	entry, ok := m.lookup(req.PtyID)
	if !ok {
		// 已经关了，幂等返回 ok。
		return WriteJSONRes(stream, struct{}{})
	}
	m.kill(entry)
	return WriteJSONRes(stream, struct{}{})
}

func (m *PTYManager) alloc(cmd *exec.Cmd, pt winpty.PTY) string {
	id := strconv.FormatInt(m.next.Add(1), 10)
	entry := &ptyEntry{id: id, pty: pt, cmd: cmd}
	m.mu.Lock()
	m.pts[id] = entry
	m.mu.Unlock()
	return id
}

func (m *PTYManager) lookup(id string) (*ptyEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.pts[id]
	return e, ok
}

// release 关闭 pty、kill 子进程、移出注册表。幂等。
func (m *PTYManager) release(id string) {
	m.mu.Lock()
	entry, ok := m.pts[id]
	if ok {
		delete(m.pts, id)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	if entry.closed.CompareAndSwap(false, true) {
		m.kill(entry)
		_ = entry.pty.Close()
		if entry.cmd.Process != nil {
			_ = entry.pty.Wait()
		}
		m.log.Info("pty released", "id", id)
	}
}

func (m *PTYManager) kill(entry *ptyEntry) {
	// 直接调 cmd.Process.Kill 在 Windows 上对 ConPTY 起的进程会失败
	// （synthetic os.Process 没有真实 handle）。PTY.Close 已经会触发
	// ClosePseudoConsole → 子进程退出，这里只在 Unix 路径上兜底。
	if entry.cmd == nil || entry.cmd.Process == nil {
		return
	}
	_ = entry.cmd.Process.Kill()
}

// pumpPTYToStream 把 pty 的输出原样帧化转发到 stream。
func pumpPTYToStream(pt winpty.PTY, stream net.Conn) (int64, error) {
	var total int64
	buf := make([]byte, 8192)
	for {
		n, err := pt.Read(buf)
		if n > 0 {
			if werr := tunnelmux.WriteFrame(stream, buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

func buildCmd(req proto.PTYCreateReq) (*exec.Cmd, error) {
	args, envTweaks := shellStartup(req.Shell)
	cmd := exec.Command(req.Shell, args...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	// 父进程 env 是底，叠加请求里指定的 env，再叠加 shell startup 必需的
	// TERM/LS_COLORS 等。后写覆盖先写，所以请求 env 可以让用户强制覆盖。
	base := os.Environ()
	if len(req.Env) > 0 {
		base = append(base, envSlice(req.Env)...)
	}
	for k, v := range envTweaks {
		// 只在用户没显式指定时设 —— 让用户能覆盖默认。
		if !hasEnv(base, k) {
			base = append(base, k+"="+v)
		}
	}
	cmd.Env = base
	return cmd, nil
}

func hasEnv(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// shellStartup 按平台 + shell 选择启动参数和默认环境变量。
//
// 关键问题：直接 exec("bash") 在 Linux 上会跑成 interactive non-login shell，
// 只读 ~/.bashrc。但 Debian/Ubuntu 的老版 ~/.bashrc 头部有 `[ -z "$PS1" ] && return`
// 守卫（PS1 在 bashrc 被源的时候还没设），结果 LS_COLORS / 彩色 prompt / alias
// 全部不生效，终端里 ls / grep / 提示符全是单色。
//
// 修法：unix 系 shell 一律加 -l（login shell）—— login 路径会 source
// /etc/profile → /etc/profile.d/*.sh，Ubuntu/Debian 的 ls 颜色就是这么注册的。
// 同时兜底设 TERM=xterm-256color，防 client 进程没继承到 TERM 时颜色被关掉。
func shellStartup(shellName string) (args []string, env map[string]string) {
	env = map[string]string{
		"TERM": "xterm-256color",
	}
	if runtime.GOOS == "windows" {
		// Windows 上 powershell/cmd 自己会 emit VT 颜色，不需要 -l；
		// TERM 给 Windows 终端程序（vim/less 等）用，保留。
		return nil, env
	}
	switch shellName {
	case "bash", "sh", "dash", "zsh", "ksh", "ash":
		args = []string{"-l"}
	case "fish":
		// fish 没有 login 概念的兼容旗标；保持 interactive（默认）即可。
		args = nil
	}
	return args, env
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func writePTYError(stream net.Conn, err error) error {
	_ = WriteJSONRes(stream, proto.PTYError{Error: err.Error()})
	return err
}
