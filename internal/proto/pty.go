package proto

// PTY op 常量。
//
// 交互模型：
//   - pty.create 在一条新 yamux stream 上发起：opener 写 StreamHeader，再写一帧
//     PTYCreateReq。client 启动 pty 后回写一帧 PTYCreateRes。此后该 stream 转为
//     双向数据通道：双方用 tunnelmux.WriteFrame / ReadFrame 收发原始字节。
//   - pty.resize 是一条独立短 RPC：opener 写 header + PTYResizeReq，client 回写
//     空 JSON 响应 `{}` 表示成功（或 PTYError）。
//   - pty.close 同样是独立短 RPC，client kill 子进程并清理资源。stream 关闭也
//     会触发 client 端清理（pty 进程随 stdin EOF 退出）。
const (
	OpPTYCreate = "pty.create"
	OpPTYResize = "pty.resize"
	OpPTYClose  = "pty.close"
)

// PTYCreateReq 是 pty.create 的请求体。
type PTYCreateReq struct {
	Shell string            `json:"shell"`             // 可执行名或绝对路径
	Cwd   string            `json:"cwd,omitempty"`     // 工作目录；空则继承客户端进程
	Cols  int               `json:"cols"`              // 0 视为 80
	Rows  int               `json:"rows"`              // 0 视为 24
	Env   map[string]string `json:"env,omitempty"`     // 追加/覆盖继承的环境变量
}

// PTYCreateRes 是 pty.create 的响应体。响应写入后，stream 进入数据帧阶段。
type PTYCreateRes struct {
	PtyID string `json:"pty_id"`
}

// PTYResizeReq 调整 pty 大小。
type PTYResizeReq struct {
	PtyID string `json:"pty_id"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
}

// PTYCloseReq 关闭 pty（kill 子进程）。
type PTYCloseReq struct {
	PtyID string `json:"pty_id"`
}

// PTYError 是 pty.* 失败时的统一响应体。
type PTYError struct {
	Error string `json:"error"`
}
