package handlers

import (
	"net"

	"github.com/lujiang/lujiang/internal/proto"
)

// HandlePing 立即响应 pong。保留作 P1 引入的隧道健康探测。
func HandlePing(stream net.Conn, _ proto.StreamHeader) error {
	pong, err := proto.StreamHeader{Op: proto.OpPong}.MarshalLine()
	if err != nil {
		return err
	}
	_, err = stream.Write(pong)
	return err
}
