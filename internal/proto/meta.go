package proto

// ClientMeta 是客户端注册时上报的自身元数据，服务端转发给 Web 端用于展示。
type ClientMeta struct {
	ID       string   `json:"id"`
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Shells   []string `json:"shells,omitempty"`
}
