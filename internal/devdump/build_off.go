//go:build !devdump

package devdump

// IsDevBuild 报告当前是否为「开发期 dump 构建」：无 tag 时为 false。
// 仅测试与启动日志使用；生产路径不依赖它（生产只调 Wrap/Enabled）。
const IsDevBuild = false
