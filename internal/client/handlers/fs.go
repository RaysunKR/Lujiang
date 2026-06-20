package handlers

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// IsBinary 用 NUL-byte 嗅探前 1KiB 判断是否二进制。
func IsBinary(b []byte) bool {
	n := len(b)
	if n > 1024 {
		n = 1024
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

// HandleList 列目录。
func HandleList(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.FSListReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	abs, err := filepath.Abs(req.Path)
	if err != nil {
		return writeFSError(stream, err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return writeFSError(stream, err)
	}
	// 显式初始化 Entries 为空切片：空目录时返回 `[]` 而非 `null`。
	// 否则前端 `[...res.entries]` 会抛 "... is not iterable"。
	out := proto.FSListRes{
		Path:    filepath.ToSlash(abs),
		Entries: []proto.FSEntry{},
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		t := proto.FSEntryFile
		switch {
		case e.IsDir():
			t = proto.FSEntryDir
		case info.Mode()&os.ModeSymlink != 0:
			t = proto.FSEntrySymlink
		}
		out.Entries = append(out.Entries, proto.FSEntry{
			Name:    e.Name(),
			Type:    t,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixMilli(),
		})
	}
	return WriteJSONRes(stream, out)
}

// HandleRead 读文件。文本走 utf8，二进制走 base64。
//
// 大文件保护：先 stat 文件大小，base64 编码后不能超过 tunnelmux.MaxFramePayload
// （base64 膨胀系数 4/3，外加几 KiB JSON 开销），否则提前报错，避免 OOM。
func HandleRead(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.FSReadReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	info, err := os.Stat(req.Path)
	if err != nil {
		return writeFSError(stream, err)
	}
	// 4 MiB headroom 给 JSON wrapper + path 字段。
	maxSrc := int64(tunnelmux.MaxFramePayload-4<<20) * 3 / 4
	if info.Size() > maxSrc {
		return writeFSError(stream, fmt.Errorf(
			"file %s is %d bytes; max supported is %d (frame limit %d bytes after base64)",
			req.Path, info.Size(), maxSrc, tunnelmux.MaxFramePayload,
		))
	}
	b, err := os.ReadFile(req.Path)
	if err != nil {
		return writeFSError(stream, err)
	}
	res := proto.FSReadRes{Size: int64(len(b))}
	if IsBinary(b) {
		res.Encoding = "base64"
		res.Content = base64.StdEncoding.EncodeToString(b)
	} else {
		res.Encoding = "utf8"
		res.Content = string(b)
	}
	return WriteJSONRes(stream, res)
}

// HandleWrite 写文件。
func HandleWrite(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.FSWriteReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	var data []byte
	switch req.Encoding {
	case "utf8", "":
		data = []byte(req.Content)
	case "base64":
		b, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			return writeFSError(stream, err)
		}
		data = b
	default:
		return writeFSError(stream, fmt.Errorf("unsupported encoding: %s", req.Encoding))
	}
	// 父目录可能不存在（前端新建文件提示明确支持 "src/foo.txt" 这种相对路径），
	// os.WriteFile 不会自动建父目录，得先 MkdirAll。
	if parent := filepath.Dir(req.Path); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return writeFSError(stream, err)
		}
	}
	if err := os.WriteFile(req.Path, data, 0644); err != nil {
		return writeFSError(stream, err)
	}
	return WriteJSONRes(stream, proto.FSWriteRes{Size: int64(len(data))})
}

// HandleStat stat 单个路径。
func HandleStat(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.FSStatReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	info, err := os.Stat(req.Path)
	if err != nil {
		return writeFSError(stream, err)
	}
	t := proto.FSEntryFile
	switch {
	case info.IsDir():
		t = proto.FSEntryDir
	case info.Mode()&os.ModeSymlink != 0:
		t = proto.FSEntrySymlink
	}
	return WriteJSONRes(stream, proto.FSStatRes{
		Type:    t,
		Size:    info.Size(),
		ModTime: info.ModTime().UnixMilli(),
	})
}

// HandleMkdir 创建目录（含父目录）。
func HandleMkdir(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.FSMkdirReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	if err := os.MkdirAll(req.Path, 0755); err != nil {
		return writeFSError(stream, err)
	}
	return WriteJSONRes(stream, struct{}{})
}

// HandleRemove 删除文件或目录。
func HandleRemove(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.FSRemoveReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	var err error
	if req.Recursive {
		err = os.RemoveAll(req.Path)
	} else {
		err = os.Remove(req.Path)
	}
	if err != nil {
		return writeFSError(stream, err)
	}
	return WriteJSONRes(stream, struct{}{})
}

// HandleMove 移动 / 重命名。
func HandleMove(stream net.Conn, _ proto.StreamHeader) error {
	var req proto.FSMoveReq
	if err := ReadJSONReq(stream, &req); err != nil {
		return err
	}
	if err := os.Rename(req.From, req.To); err != nil {
		return writeFSError(stream, err)
	}
	return WriteJSONRes(stream, struct{}{})
}

// writeFSError 写入 FSError 响应；返回原 err 让 dispatcher 记录日志。
func writeFSError(stream net.Conn, err error) error {
	msg := err.Error()
	var pErr *fs.PathError
	if errors.As(err, &pErr) {
		msg = pErr.Op + " " + pErr.Path + ": " + pErr.Err.Error()
	}
	_ = WriteJSONRes(stream, proto.FSError{Error: msg})
	return err
}
