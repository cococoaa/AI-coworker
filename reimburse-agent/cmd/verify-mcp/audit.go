package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"reimburse/internal/mcpauth"
)

// isLoopbackAddr 判断监听地址是否只对本机开放。
// 0.0.0.0、空主机、局域网或公网地址都算跨机；只有 127.0.0.1、::1、localhost 算本机。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 没有端口时整个字符串当主机名
	}
	switch strings.ToLower(host) {
	case "", "0.0.0.0", "::", "[::]":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// --- 审计日志 ---
//
// 每次 MCP 请求记一行 JSON：时间、调用方（令牌名）、来源 IP、方法、状态码、耗时。
// 不记令牌本身，也不记请求体——请求体里有发票号、税号等业务数据，日志不必留。

var auditMu sync.Mutex

type auditRecord struct {
	At     string `json:"at"`
	Client string `json:"client,omitempty"`
	IP     string `json:"ip"`
	Method string `json:"method"`
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
}

func writeAudit(path string, rec auditRecord) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// statusWriter 记录响应状态码。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush 保证 SSE 流式响应不被缓冲破坏。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// auditLogMiddleware 包住 MCP 处理器，记录每次请求。
//
// 调用顺序：auditLogMiddleware( verifier.Middleware()( handler ) )
// 审计在最外层，才有机会读到鉴权写成子 context 的令牌信息；
// 若反过来，外层拿到的是原始请求，令牌名永远为空。
// 401 的请求也会被记录，用来发现"有人拿错令牌在试"。
func auditLogMiddleware(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		// 挂 sink 后再交给内层：鉴权通过时会把调用方写进 sink，外层据此记录 client。
		inner, s := mcpauth.WithSink(r)
		next.ServeHTTP(sw, inner)
		client := s.Client()
		writeAudit(path, auditRecord{
			At: t0.Format(time.RFC3339), Client: client, IP: remoteIP(r),
			Method: r.Method, Status: sw.status, MS: time.Since(t0).Milliseconds(),
		})
	})
}

func remoteIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if i := strings.Index(h, ","); i > 0 {
			h = h[:i]
		}
		return strings.TrimSpace(h)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
