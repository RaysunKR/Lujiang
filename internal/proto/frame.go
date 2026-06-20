// Package proto 定义服务端与客户端共享的隧道协议类型。
//
// 隧道建立后，双方在 yamux session 上打开若干 stream。每条 stream 的第一行
// 是一个 JSON-encoded StreamHeader，声明该 stream 的 op 与可选 seq。后续帧
// 按 op 的语义解析（参见 internal/tunnelmux/framing.go 的长度前缀帧）。
package proto

import (
	"encoding/json"
	"fmt"
)

// StreamHeader 是每条 yamux stream 的首行 JSON。
type StreamHeader struct {
	Op  string `json:"op"`
	Seq int64  `json:"seq,omitempty"`
}

// MarshalLine 序列化为单行 JSON 加 '\n'。
func (h StreamHeader) MarshalLine() ([]byte, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("proto: marshal header: %w", err)
	}
	return append(b, '\n'), nil
}

// UnmarshalHeaderLine 从一行（不含尾 '\n'）解析 StreamHeader。
func UnmarshalHeaderLine(b []byte) (StreamHeader, error) {
	var h StreamHeader
	if err := json.Unmarshal(b, &h); err != nil {
		return StreamHeader{}, fmt.Errorf("proto: unmarshal header: %w", err)
	}
	return h, nil
}

// 隧道层 op。
const (
	OpPing       = "ping"       // 心跳探测，对方应立即回 Pong
	OpPong       = "pong"
	OpRegister   = "register"   // 客户端 → 服务端，附带 ClientMeta
	OpRegistered = "registered" // 服务端 → 客户端，注册成功
)

// ReadHeaderLine 从 r 读取一行（到 '\n'），解析为 StreamHeader。
// 行最大 4KiB，防止滥用。
func ReadHeaderLine(r interface{ Read(p []byte) (int, error) }) (StreamHeader, []byte, error) {
	const maxLine = 4 << 10
	var buf [1]byte
	var line []byte
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			line = append(line, buf[0])
			if len(line) > maxLine {
				return StreamHeader{}, nil, fmt.Errorf("proto: header line exceeds %d bytes", maxLine)
			}
		}
		if err != nil {
			return StreamHeader{}, nil, fmt.Errorf("proto: read header line: %w", err)
		}
	}
	h, err := UnmarshalHeaderLine(line)
	if err != nil {
		return StreamHeader{}, nil, err
	}
	return h, line, nil
}
