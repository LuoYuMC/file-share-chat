package main

import "bytes"

// appVersion 程序版本号（显示在管理页面底部、启动信息中）。
// 从 v0.1.0 开始记录；之后每次更改默认第三位加 1（如 v0.1.0 → v0.1.1），有特殊说明的按说明修改。
const appVersion = "v0.1.0"

// versionPlaceholder 页面中的版本号占位符，返回页面时替换为 appVersion
const versionPlaceholder = "{{APP_VERSION}}"

// renderPage 返回替换了版本号的页面 HTML
func renderPage() []byte {
	return bytes.ReplaceAll(indexHTML, []byte(versionPlaceholder), []byte(appVersion))
}
