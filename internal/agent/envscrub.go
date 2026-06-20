package agent

import "strings"

// isFilteredChildEnvKey 判断一个从父进程继承的环境变量是否要在 spawn
// agent 子进程时剥掉。
//
// 这些是 Claude Code 自身的内部运行时标记，子进程看到会误以为自己跑在
// 嵌套会话里、或者继承了父进程的 exec path / transport。
//
// 不能一刀切剥离 CLAUDE_CODE_* 前缀 —— 用户的配置（CLAUDE_CODE_GIT_BASH_PATH
// 等）必须保留，否则 Windows 上 Claude Code 找不到 bash.exe 直接挂掉
// （multica 实际踩过）。
func isFilteredChildEnvKey(key string) bool {
	switch key {
	case "CLAUDECODE",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_EXECPATH",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_SSE_PORT":
		return true
	}
	// CLAUDECODE_*（CLAUDE 和 CODE 之间没有下划线）是内部命名空间，剥。
	// CLAUDE_CODE_* 是用户命名空间，保留。
	return strings.HasPrefix(key, "CLAUDECODE_")
}
