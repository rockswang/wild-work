//go:build devdump

package devdump

// IsDevBuild 报告当前是否为「开发期 dump 构建」（带 `-tags devdump` = true）。
//
// 生产代码**不依赖**它（生产路径只调 Wrap/Enabled，桩实现里恒惰性）；
// 它只服务于测试与启动日志，让两种构建下的断言/提示能区分。
// 无 tag 版本见 build_off.go。
const IsDevBuild = true
