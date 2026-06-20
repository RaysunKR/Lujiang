package agent

import "strings"

// blockedArgMode 表示一个被屏蔽的 flag 是否带值。
type blockedArgMode int

const (
	blockedWithValue  blockedArgMode = iota // flag 后跟一个值
	blockedStandalone                       // flag 是 boolean，无值
)

// claudeBlockedArgs 是 Claude Code backend 自己 hardcode 的 flag 集合；
// 用户传入 custom_args 时会被 filter 掉这些，避免破坏 stream-json 协议。
var claudeBlockedArgs = map[string]blockedArgMode{
	"-p":                blockedStandalone,
	"--output-format":   blockedWithValue,
	"--input-format":    blockedWithValue,
	"--permission-mode": blockedWithValue,
}

// filterCustomArgs 移除 args 里命中 blocked 的 flag（含其后续值）。
// 借鉴 multica，简化：Lujiang 当前没有 user-configurable custom_args，
// 此处保留供未来扩展（如配置文件里允许用户叠加 flag）。
func filterCustomArgs(args []string, blocked map[string]blockedArgMode, log Logger) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	skip := false
	for _, raw := range args {
		if skip {
			skip = false
			continue
		}
		arg := unshellQuoteArg(raw)
		flag := arg
		hasInline := false
		if idx := strings.Index(arg, "="); idx > 0 {
			flag = arg[:idx]
			hasInline = true
		}
		mode, blocked_ := blocked[flag]
		if blocked_ {
			if log != nil {
				log.Warn("custom_args: blocked protocol-critical flag, skipping", "flag", flag)
			}
			if mode == blockedWithValue && !hasInline {
				skip = true
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

// unshellQuoteArg 剥一层 shell 引号。
func unshellQuoteArg(arg string) string {
	if strings.HasPrefix(arg, "-") {
		if idx := strings.Index(arg, "="); idx > 0 {
			val := arg[idx+1:]
			if u, ok := stripSurroundingQuotes(val); ok {
				return arg[:idx+1] + u
			}
			return arg
		}
	}
	if u, ok := stripSurroundingQuotes(arg); ok {
		return u
	}
	return arg
}

func stripSurroundingQuotes(s string) (string, bool) {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1], true
		}
	}
	return s, false
}
