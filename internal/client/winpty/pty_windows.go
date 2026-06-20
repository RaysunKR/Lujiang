//go:build windows

package winpty

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 常量与过程地址，集中维护。仅 Windows 编译时存在。
const (
	procThreadAttributePseudoConsole uintptr = 0x00020016
	extendedStartupinfoPresent      uint32  = 0x00080000
	createUnicodeEnvironment        uint32  = 0x00000400
)

var (
	kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole    = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole    = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole     = kernel32.NewProc("ClosePseudoConsole")
	procInitializeProcAttrList = kernel32.NewProc("InitializeProcThreadAttributeList")
	procDeleteProcAttrList     = kernel32.NewProc("DeleteProcThreadAttributeList")
	procUpdateProcThreadAttrib = kernel32.NewProc("UpdateProcThreadAttribute")
	procCreateProcessW         = kernel32.NewProc("CreateProcessW")
)

// conPTY 是 Windows ConPTY 上的 PTY 实现。
type conPTY struct {
	hPC windows.Handle
	in  *os.File // 写入 → child stdin
	out *os.File // 读取 ← child stdout/stderr
	cmd *exec.Cmd

	attrBuf  []byte
	procInfo windows.ProcessInformation
	hProc    windows.Handle

	closed bool
}

func startWithSize(cmd *exec.Cmd, cols, rows int) (PTY, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// 1. 建两条管道：input（我们写 → child stdin），output（child → 我们读）。
	hInputRead, hInputWrite, err := makePipe()
	if err != nil {
		return nil, fmt.Errorf("create input pipe: %w", err)
	}
	hOutputRead, hOutputWrite, err := makePipe()
	if err != nil {
		windows.CloseHandle(hInputRead)
		windows.CloseHandle(hInputWrite)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}

	// 2. CreatePseudoConsole：接管 hInputRead 与 hOutputWrite。
	hPC, err := createPseudoConsole(cols, rows, hInputRead, hOutputWrite)
	if err != nil {
		windows.CloseHandle(hInputRead)
		windows.CloseHandle(hInputWrite)
		windows.CloseHandle(hOutputRead)
		windows.CloseHandle(hOutputWrite)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}

	// 3. ProcThreadAttributeList（PSEUDOCONSOLE = hPC）。
	attrBuf, err := buildAttrList(hPC)
	if err != nil {
		procClosePseudoConsole.Call(uintptr(hPC))
		windows.CloseHandle(hInputWrite)
		windows.CloseHandle(hOutputRead)
		return nil, fmt.Errorf("attribute list: %w", err)
	}

	// 4. CreateProcess（STARTUPINFOEX）。
	exe, args, err := resolveCmdLine(cmd)
	if err != nil {
		deleteAttrList(attrBuf)
		procClosePseudoConsole.Call(uintptr(hPC))
		windows.CloseHandle(hInputWrite)
		windows.CloseHandle(hOutputRead)
		return nil, err
	}
	envBlock, err := buildEnvBlock(cmd.Env)
	if err != nil {
		deleteAttrList(attrBuf)
		procClosePseudoConsole.Call(uintptr(hPC))
		windows.CloseHandle(hInputWrite)
		windows.CloseHandle(hOutputRead)
		return nil, err
	}
	pi, err := createProcess(exe, args, cmd.Dir, envBlock, attrBuf)
	if err != nil {
		deleteAttrList(attrBuf)
		procClosePseudoConsole.Call(uintptr(hPC))
		windows.CloseHandle(hInputWrite)
		windows.CloseHandle(hOutputRead)
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}

	c := &conPTY{
		hPC:      hPC,
		in:       os.NewFile(uintptr(hInputWrite), "|stdin"),
		out:      os.NewFile(uintptr(hOutputRead), "|stdout"),
		cmd:      cmd,
		attrBuf:  attrBuf,
		procInfo: pi,
		hProc:    pi.Process,
	}
	if c.cmd.Process == nil {
		// 用 FindProcess 让 os.Process 注册 finalizer / 正确的 handle 状态，
		// 避免 Go 1.25+ 在 synthetic Process 上 Kill() 时的 transient handle 崩溃。
		c.cmd.Process, _ = os.FindProcess(int(pi.ProcessId))
	}
	return c, nil
}

func (c *conPTY) Read(b []byte) (int, error)  { return c.out.Read(b) }
func (c *conPTY) Write(b []byte) (int, error) { return c.in.Write(b) }

func (c *conPTY) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true

	procClosePseudoConsole.Call(uintptr(c.hPC))
	_ = c.in.Close()
	_ = c.out.Close()
	_ = c.Wait()
	deleteAttrList(c.attrBuf)
	windows.CloseHandle(c.procInfo.Thread)
	return nil
}

func (c *conPTY) SetSize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid size %dx%d", cols, rows)
	}
	return resizePseudoConsole(c.hPC, cols, rows)
}

func (c *conPTY) Wait() error {
	if c.hProc == 0 {
		return nil
	}
	// 最多等 5 秒；超时则 kill。
	event, _ := windows.WaitForSingleObject(c.hProc, 5_000)
	if event == uint32(windows.WAIT_TIMEOUT) {
		_ = windows.TerminateProcess(c.hProc, 1)
		_, _ = windows.WaitForSingleObject(c.hProc, 1_000)
	}
	windows.CloseHandle(c.hProc)
	c.hProc = 0
	return nil
}

// ---- Win32 helpers ----

func makePipe() (read windows.Handle, write windows.Handle, err error) {
	sa := windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	var r, w windows.Handle
	if e := windows.CreatePipe(&r, &w, &sa, 0); e != nil {
		return 0, 0, e
	}
	return r, w, nil
}

// createPseudoConsole：CreatePseudoConsole 返回 HRESULT，0 = S_OK。
func createPseudoConsole(cols, rows int, hInputRead, hOutputWrite windows.Handle) (windows.Handle, error) {
	size := uint32(rows&0xFFFF)<<16 | uint32(cols&0xFFFF)
	var hPC windows.Handle
	r1, _, _ := procCreatePseudoConsole.Call(
		uintptr(size),
		uintptr(hInputRead),
		uintptr(hOutputWrite),
		0,
		uintptr(unsafe.Pointer(&hPC)),
	)
	if r1 != 0 {
		return 0, fmt.Errorf("hresult=0x%x", uint32(r1))
	}
	return hPC, nil
}

func resizePseudoConsole(hPC windows.Handle, cols, rows int) error {
	size := uint32(rows&0xFFFF)<<16 | uint32(cols&0xFFFF)
	r1, _, _ := procResizePseudoConsole.Call(uintptr(hPC), uintptr(size))
	if r1 != 0 {
		return fmt.Errorf("hresult=0x%x", uint32(r1))
	}
	return nil
}

func buildAttrList(hPC windows.Handle) ([]byte, error) {
	// 1. 取 size（首调必失败，GetLastError=ERROR_INSUFFICIENT_BUFFER）。
	var size uintptr
	procInitializeProcAttrList.Call(0, 1, 0, uintptr(unsafe.Pointer(&size)))

	buf := make([]byte, size)
	r1, _, e := procInitializeProcAttrList.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		1, 0,
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		if e == windows.Errno(0) {
			return nil, errors.New("InitializeProcThreadAttributeList failed")
		}
		return nil, e
	}

	// 2. UpdateProcThreadAttribute(PSEUDOCONSOLE = hPC)。
	r1, _, e = procUpdateProcThreadAttrib.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		0,
		procThreadAttributePseudoConsole,
		uintptr(hPC),
		unsafe.Sizeof(hPC),
		0, 0,
	)
	if r1 == 0 {
		deleteAttrList(buf)
		if e == windows.Errno(0) {
			return nil, errors.New("UpdateProcThreadAttribute failed")
		}
		return nil, e
	}
	return buf, nil
}

func deleteAttrList(buf []byte) {
	if len(buf) == 0 {
		return
	}
	procDeleteProcAttrList.Call(uintptr(unsafe.Pointer(&buf[0])))
}

// resolveCmdLine 从 exec.Cmd 推断 exe 路径 + Windows 命令行。
func resolveCmdLine(cmd *exec.Cmd) (exe string, args string, err error) {
	if cmd.Path == "" {
		return "", "", errors.New("cmd.Path is empty")
	}
	argv := cmd.Args
	if len(argv) == 0 {
		argv = []string{cmd.Path}
	}
	var line string
	for i, a := range argv {
		if i > 0 {
			line += " "
		}
		line += syscall.EscapeArg(a)
	}
	return cmd.Path, line, nil
}

// buildEnvBlock 把 cmd.Env 转成 UTF-16 "K=V\0K=V\0\0" 块；nil → NULL（继承父进程）。
//
// 不能用 windows.UTF16PtrFromString：它在字符串里见到 \0 就报 EINVAL，
// 而本函数本来就是要拼出多个 \0 终止符的。直接 utf16.Encode 后手工拼。
func buildEnvBlock(env []string) (*uint16, error) {
	if len(env) == 0 {
		return nil, nil
	}
	var block []uint16
	for _, kv := range env {
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	block = append(block, 0)
	return &block[0], nil
}

type startupInfoEx struct {
	windows.StartupInfo
	lpAttributeList uintptr
}

func createProcess(exe, args, dir string, envBlock *uint16, attrBuf []byte) (windows.ProcessInformation, error) {
	var pi windows.ProcessInformation

	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return pi, err
	}
	argPtr, err := windows.UTF16PtrFromString(args)
	if err != nil {
		return pi, err
	}
	var dirPtr *uint16
	if dir != "" {
		dirPtr, err = windows.UTF16PtrFromString(dir)
		if err != nil {
			return pi, err
		}
	}

	siex := startupInfoEx{}
	siex.StartupInfo.Cb = uint32(unsafe.Sizeof(siex))
	siex.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	siex.lpAttributeList = uintptr(unsafe.Pointer(&attrBuf[0]))
	// UserExistsError/conpty 实践：即便 std handles 留空，也要打 STARTF_USESTDHANDLES。
	// 不打的话 ConPTY 在某些父进程环境下（如父进程自己有 console）会被忽略，
	// 子进程落到继承父 console，输出绕过 ConPTY 管道。

	// 不要叠加 CREATE_NO_WINDOW：该 flag 会让 Windows 不为子进程分配 console，
	// 反而把 ConPTY 的输出路径切断（ConPTY 是子进程的 console）。
	flags := uint32(createUnicodeEnvironment | extendedStartupinfoPresent)

	r1, _, e := procCreateProcessW.Call(
		uintptr(unsafe.Pointer(exePtr)),
		uintptr(unsafe.Pointer(argPtr)),
		0, 0,
		0, // bInheritHandles 必须 FALSE
		uintptr(flags),
		uintptr(unsafe.Pointer(envBlock)),
		uintptr(unsafe.Pointer(dirPtr)),
		uintptr(unsafe.Pointer(&siex.StartupInfo)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r1 == 0 {
		if e == windows.Errno(0) {
			return pi, errors.New("CreateProcessW failed")
		}
		return pi, e
	}
	return pi, nil
}
