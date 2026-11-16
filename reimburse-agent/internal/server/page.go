package server

import _ "embed"

//go:embed web/index.html
var indexHTML []byte

// PageHTML 返回单页界面的 HTML。
func PageHTML() []byte { return indexHTML }
