// Package tunnelmux 把 yamux 多路复用封装成可复用的辅助函数，并提供
// 长度前缀 JSON 帧编解码（用于流式 op 的双向数据帧）。
package tunnelmux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFramePayload 单帧最大字节数。 Originally 1MiB as OOM guard; raised to
// 256MiB so fs.read/write can move large binaries (e.g. images, executables,
// model files) without chunking. Beyond this we still refuse — both to bound
// memory pressure and because base64-encoded payloads inflate by 4/3.
const MaxFramePayload = 256 << 20 // 256 MiB

// ErrFrameTooLarge 在帧长度超过 MaxFramePayload 时返回。
var ErrFrameTooLarge = errors.New("tunnelmux: frame payload exceeds 256MiB")

// WriteFrame 写入 4-byte BE 长度前缀 + payload。
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return ErrFrameTooLarge
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("tunnelmux: write frame header: %w", err)
	}
	if len(payload) == 0 {
		return nil
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("tunnelmux: write frame payload: %w", err)
	}
	return nil
}

// ReadFrame 读取 4-byte BE 长度前缀 + payload。
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("tunnelmux: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFramePayload {
		return nil, ErrFrameTooLarge
	}
	if n == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("tunnelmux: read frame payload: %w", err)
	}
	return buf, nil
}
