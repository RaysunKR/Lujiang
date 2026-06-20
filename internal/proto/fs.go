package proto

// FSEntryType 标识目录项类型。
type FSEntryType string

const (
	FSEntryFile    FSEntryType = "file"
	FSEntryDir     FSEntryType = "dir"
	FSEntrySymlink FSEntryType = "symlink"
)

// FSEntry 是目录列表里的一项。
type FSEntry struct {
	Name    string      `json:"name"`
	Type    FSEntryType `json:"type"`
	Size    int64       `json:"size"`
	ModTime int64       `json:"mod_time"` // unix ms
}

// 文件服务 op 常量。
const (
	OpFSList   = "fs.list"
	OpFSRead   = "fs.read"
	OpFSWrite  = "fs.write"
	OpFSStat   = "fs.stat"
	OpFSMkdir  = "fs.mkdir"
	OpFSRemove = "fs.remove"
	OpFSMove   = "fs.move"
)

// FSListReq / FSListRes。
type FSListReq struct {
	Path string `json:"path"` // 客户端本地路径；为 "." 时由客户端解析为工作目录
}
type FSListRes struct {
	Path    string    `json:"path"` // 服务端解析后的绝对路径
	Entries []FSEntry `json:"entries"`
}

// FSReadReq / FSReadRes。
// Encoding 为 "utf8" 时 Content 是 UTF-8 文本；为 "base64" 时是 base64 编码的二进制。
type FSReadReq struct {
	Path string `json:"path"`
}
type FSReadRes struct {
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
}

// FSWriteReq 把内容写入指定路径。
type FSWriteReq struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding"` // "utf8" 或 "base64"
	Content  string `json:"content"`
}
type FSWriteRes struct {
	Size int64 `json:"size"`
}

// FSStatReq / FSStatRes。
type FSStatReq struct {
	Path string `json:"path"`
}
type FSStatRes struct {
	Type    FSEntryType `json:"type"`
	Size    int64       `json:"size"`
	ModTime int64       `json:"mod_time"`
}

// FSMkdirReq 创建目录（含父目录）。
type FSMkdirReq struct {
	Path string `json:"path"`
}

// FSRemoveReq 删除文件或目录。
type FSRemoveReq struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

// FSMoveReq 移动 / 重命名。
type FSMoveReq struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// FSError 是所有 fs op 失败时的统一响应体。
type FSError struct {
	Error string `json:"error"`
}
