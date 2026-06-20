// Package shell 按平台枚举客户端可用的 shell，并选出默认 shell。
//
// 浏览器侧的"新建终端"流程会先 GET 客户端元数据（含 Shells 列表），让用户
// 选一个；客户端 PTY handler 启动时若 req.Shell 为空，则调 Default() 兜底。
package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Available 在 PATH 上依次检查候选 shell，返回确实存在的那些。
// 名字按"优先级"排序，第一个元素即推荐默认值。
func Available() []string {
	candidates := candidates()
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			// Windows 上 LookPath("powershell") 可能找到 .ps1 文件，
			// 这里额外兜底 .exe 扩展。
			out = append(out, baseName(path))
		}
	}
	if len(out) == 0 {
		// 最后兜底：返回平台默认 shell 的名字（即便 LookPath 失败）。
		switch runtime.GOOS {
		case "windows":
			return []string{"cmd"}
		default:
			return []string{"sh"}
		}
	}
	return out
}

// Default 返回 Available() 的第一个，没有就返回硬编码兜底。
func Default() string {
	if s := Available(); len(s) > 0 {
		return s[0]
	}
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

func baseName(p string) string {
	b := filepath.Base(p)
	// Windows：去掉 .exe 后缀以便 UI 展示。
	b = strings.TrimSuffix(b, ".exe")
	return b
}

func candidates() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"powershell", "pwsh", "cmd"}
	case "darwin":
		// macOS 默认 zsh；用户也可能装了 fish。
		return []string{"zsh", "bash", "fish", "sh"}
	default:
		// Linux/BSD：优先 $SHELL（如果有效），再补几个常见候选。
		out := []string{}
		if s := os.Getenv("SHELL"); s != "" {
			if _, err := exec.LookPath(s); err == nil {
				out = append(out, baseName(s))
			}
		}
		for _, c := range []string{"bash", "zsh", "fish", "sh"} {
			if !contains(out, c) {
				out = append(out, c)
			}
		}
		return out
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
