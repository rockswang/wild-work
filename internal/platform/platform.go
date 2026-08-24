// Package platform 跨平台能力封装：浏览器/开机自启/消息框/日志/工作区。
//
// 按 build tag 拆分实现（各平台保持同名导出函数，接口不变量）：
//   - platform_windows.go  （GOOS=windows）
//   - platform_darwin.go   （GOOS=darwin）
//   - platform_other.go    （其余平台，简化实现）
//
// 上层（internal/app、cmd/wild-work）只依赖本包导出的函数，不接触任何平台代码。
package platform
