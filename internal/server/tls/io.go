package tls

import (
	"os"
)

// readFile / osHostname 是为方便在测试中替换的小包装。
func readFile(p string) ([]byte, error)             { return os.ReadFile(p) }
func osHostname() (string, error)                   { return os.Hostname() }
