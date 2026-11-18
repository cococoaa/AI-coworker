package server

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/chat.js
var chatJS []byte

//go:embed web/review-tools.js
var reviewToolsJS []byte

func serveReviewToolsJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(reviewToolsJS)
}

func serveChatJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(chatJS)
}

// PageHTML 返回单页界面的 HTML。
func PageHTML() []byte { return indexHTML }
