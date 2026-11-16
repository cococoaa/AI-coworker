package llm

import (
	"bytes"
	"io"
	"strings"
)

func trimSpace(s string) string { return strings.TrimSpace(s) }

func upper(s string) string { return strings.ToUpper(s) }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

var _ = schema_placeholder

// schema_placeholder 只是占位，避免误删 import；真正的结构类型在 schema 包里。
const schema_placeholder = 0
