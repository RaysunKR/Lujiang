// Package auth 实现服务端 Web 登录（bcrypt 校验 + HMAC 签名 cookie）
// 与客户端 token 校验。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// SessionLifetime 是 session cookie 的有效期。
const SessionLifetime = 7 * 24 * time.Hour

// ErrInvalid 是 cookie 校验失败时返回的统一错误（不向外暴露具体原因）。
var ErrInvalid = errors.New("auth: invalid session")

// WebAuth 负责签发与校验 Web 端 session cookie。
type WebAuth struct {
	secret   []byte
	secure   bool // cookie 是否带 Secure（HTTPS 才能开）
	users    map[string][]byte
}

// NewWebAuth 创建一个 WebAuth。secret 为空时会 panic。
func NewWebAuth(secret []byte, secure bool, users map[string]string) *WebAuth {
	hashes := make(map[string][]byte, len(users))
	for u, h := range users {
		hashes[u] = []byte(h)
	}
	if len(secret) == 0 {
		panic("auth: secret must be non-empty")
	}
	return &WebAuth{secret: secret, secure: secure, users: hashes}
}

// VerifyPassword 校验用户名密码；成功返回 username。
func (a *WebAuth) VerifyPassword(username, password string) (string, error) {
	hash, ok := a.users[username]
	if !ok {
		return "", fmt.Errorf("auth: unknown user")
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		return "", fmt.Errorf("auth: bad password: %w", err)
	}
	return username, nil
}

// IssueCookie 给 response 设置一个新签发的 session cookie。
func (a *WebAuth) IssueCookie(w http.ResponseWriter, username string) {
	exp := time.Now().Add(SessionLifetime).Unix()
	value := a.sign(username, exp)
	http.SetCookie(w, &http.Cookie{
		Name:     "lj_session",
		Value:    value,
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		MaxAge:   int(SessionLifetime.Seconds()),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie 立即作废 session cookie。
func (a *WebAuth) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "lj_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// VerifyRequest 从 request 里取出并校验 session cookie。返回 username。
func (a *WebAuth) VerifyRequest(r *http.Request) (string, error) {
	c, err := r.Cookie("lj_session")
	if err != nil {
		return "", ErrInvalid
	}
	return a.parse(c.Value)
}

// sign 生成 base64(2-byte-username-len | username | 8-byte exp | 32-byte hmac)。
// HMAC 输出可能本身含 NUL，因此不能用分隔符；改用显式长度。
func (a *WebAuth) sign(username string, exp int64) string {
	if len(username) > 0xFFFF {
		// 防御性兜底；用户名都是配置文件里的固定字符串。
		return ""
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(username))
	_ = binary.Write(mac, binary.BigEndian, exp)
	sum := mac.Sum(nil)

	var ulen [2]byte
	binary.BigEndian.PutUint16(ulen[:], uint16(len(username)))
	var expBuf [8]byte
	binary.BigEndian.PutUint64(expBuf[:], uint64(exp))

	payload := make([]byte, 0, 2+len(username)+8+len(sum))
	payload = append(payload, ulen[:]...)
	payload = append(payload, username...)
	payload = append(payload, expBuf[:]...)
	payload = append(payload, sum...)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func (a *WebAuth) parse(value string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", ErrInvalid
	}
	if len(raw) < 2+8+sha256.Size {
		return "", ErrInvalid
	}
	ulen := int(binary.BigEndian.Uint16(raw[:2]))
	if len(raw) != 2+ulen+8+sha256.Size {
		return "", ErrInvalid
	}
	username := string(raw[2 : 2+ulen])
	exp := int64(binary.BigEndian.Uint64(raw[2+ulen : 2+ulen+8]))
	macBytes := raw[2+ulen+8:]
	if time.Now().Unix() >= exp {
		return "", ErrInvalid
	}

	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(username))
	_ = binary.Write(mac, binary.BigEndian, exp)
	if !hmac.Equal(mac.Sum(nil), macBytes) {
		return "", ErrInvalid
	}
	return username, nil
}
