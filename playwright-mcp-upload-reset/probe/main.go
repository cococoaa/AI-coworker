// 最小复现探针：只用 Go 标准库，不引入任何第三方依赖。
//
// 目的：给 Playwright MCP 上游一个"不依赖提问者业务代码"的复现物。
// 它做三件事：
//   1. 提供一个页面，页面上有 file input 和"上传"按钮；
//   2. 按钮触发 fetch + FormData（multipart/form-data）POST 到 /upload；
//   3. 服务端把每个请求（含 Content-Length 与解析结果）打到 stdout。
//
// 判定方法：如果浏览器侧报 BROWSER_SESSION_RESET，而 stdout 里没有对应的
// upload 行，说明请求根本没到服务端——问题在客户端/会话层，不在服务端。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
)

const page = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8"><title>upload probe</title>
<style>body{font:14px system-ui;margin:24px}pre{background:#f4f4f4;padding:12px;white-space:pre-wrap}</style>
</head><body>
<h3>multipart 上传探针</h3>
<p><input id="f" type="file"></p>
<p><button id="go">上传</button></p>
<pre id="out">idle</pre>
<script>
document.getElementById('go').addEventListener('click', async () => {
  const out = document.getElementById('out');
  const file = document.getElementById('f').files[0];
  if (!file) { out.textContent = 'no file selected'; return; }
  const fd = new FormData();
  fd.append('invoice', file, file.name);
  fd.append('claim_no', 'R-0001');
  out.textContent = 'posting…';
  try {
    const r = await fetch('/upload', { method: 'POST', body: fd });
    const j = await r.json();
    out.textContent = 'status=' + r.status + ' ' + JSON.stringify(j);
    window.__probe = { ok: true, status: r.status, body: j };
  } catch (e) {
    out.textContent = 'fetch failed: ' + e;
    window.__probe = { ok: false, error: String(e) };
  }
});
</script></body></html>`

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	addr := ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		// 每个请求都记一行：这是判断"请求有没有到服务端"的关键证据。
		log.Printf("REQ /upload method=%s ct=%q len=%d ua=%q",
			r.Method, r.Header.Get("Content-Type"), r.ContentLength, r.Header.Get("User-Agent"))

		if err := r.ParseMultipartForm(4 << 20); err != nil {
			log.Printf("PARSE_ERR %v", err)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		f, hdr, err := r.FormFile("invoice")
		if err != nil {
			log.Printf("FORMFILE_ERR %v", err)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		defer f.Close()
		buf := make([]byte, 64)
		n, _ := f.Read(buf)
		log.Printf("OK file=%q size_hint=%d claim_no=%q", hdr.Filename, hdr.Size, r.FormValue("claim_no"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "filename": hdr.Filename, "size": hdr.Size, "head": string(buf[:n]),
		})
	})

	fmt.Printf("PROBE_URL=http://%s/\n", addr)
	os.Stdout.Sync()
	log.Printf("listening on %s", addr)
	if err := http.Serve(ln, mux); err != nil {
		log.Fatal(err)
	}
}
