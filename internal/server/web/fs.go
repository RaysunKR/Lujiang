package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/lujiang/lujiang/internal/proto"
	"github.com/lujiang/lujiang/internal/server/tunnel"
	"github.com/lujiang/lujiang/internal/tunnelmux"
)

// fsRouter 暴露 /api/fs/{clientID}/{op} 路由，把请求转发到客户端的 yamux stream。
func fsRouter(reg *tunnel.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/fs/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			writeError(w, http.StatusBadRequest, "expected /api/fs/{clientID}/{op}")
			return
		}
		clientID, opSuffix := parts[0], parts[1]
		op := "fs." + opSuffix

		cc, ok := reg.Lookup(clientID)
		if !ok {
			writeError(w, http.StatusNotFound, "client not online")
			return
		}

		stream, err := cc.OpenStream()
		if err != nil {
			writeError(w, http.StatusBadGateway, "open stream: "+err.Error())
			return
		}
		defer stream.Close()

		// 写 StreamHeader。
		hdrLine, _ := proto.StreamHeader{Op: op}.MarshalLine()
		if _, err := stream.Write(hdrLine); err != nil {
			writeError(w, http.StatusBadGateway, "write header: "+err.Error())
			return
		}

		// 按 op 拼请求体。
		reqPayload, err := buildFSRequest(op, r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := tunnelmux.WriteFrame(stream, reqPayload); err != nil {
			writeError(w, http.StatusBadGateway, "write request: "+err.Error())
			return
		}

		// 读响应帧，原样回写浏览器。
		respPayload, err := tunnelmux.ReadFrame(stream)
		if err != nil {
			writeError(w, http.StatusBadGateway, "read response: "+err.Error())
			return
		}

		// 检查是否 FSError：若是，映射为合适 HTTP 状态。
		var probe proto.FSError
		if json.Unmarshal(respPayload, &probe) == nil && probe.Error != "" {
			// 但 FSWriteRes/FSStatRes 等无 error 字段，反序列化后 probe.Error 为空。
			// 这里只有当 FSError 显式存在且 Error 非空时才视为错误。
			// 进一步：判断字段是否真的存在需要 unmarshal 到 map。
			var anyResp map[string]any
			if json.Unmarshal(respPayload, &anyResp) == nil {
				if errMsg, ok := anyResp["error"].(string); ok && errMsg != "" {
					writeError(w, http.StatusInternalServerError, errMsg)
					return
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, strings.NewReader(string(respPayload)))
	})
}

func buildFSRequest(op string, r *http.Request) ([]byte, error) {
	q := r.URL.Query()
	switch op {
	case proto.OpFSList:
		return json.Marshal(proto.FSListReq{Path: q.Get("path")})
	case proto.OpFSStat:
		return json.Marshal(proto.FSStatReq{Path: q.Get("path")})
	case proto.OpFSRead:
		return json.Marshal(proto.FSReadReq{Path: q.Get("path")})
	case proto.OpFSMkdir:
		return json.Marshal(proto.FSMkdirReq{Path: q.Get("path")})
	case proto.OpFSRemove:
		return json.Marshal(proto.FSRemoveReq{
			Path:      q.Get("path"),
			Recursive: q.Get("recursive") == "true",
		})
	case proto.OpFSWrite, proto.OpFSMove:
		// body payload — 直接透传 JSON 给 client handler。
		return io.ReadAll(r.Body)
	default:
		return nil, errUnsupportedOp
	}
}
