//go:build !devdump

// devdump_off.go 发布版实现：零开销桩。
//
// 无 `-tags devdump` 时**整个包退化为这几个常量函数**——Wrap 原样返回入参
// （内联后调用点等于不存在），Enable/Enabled 恒为 false。不含任何 io/net/文件
// 逻辑，编译产物中不会出现 dump 代码，也不会因环境变量被意外开启。
//
// 这与「运行期开关」是互补的两层：构建标签保证发布版**没有能力**，
// 环境变量保证开发版默认**不落盘**（按需开启）。
package devdump

import "net/http"

// Enable 发布版为空操作。
func Enable(string) {}

// Disable 发布版为空操作。
func Disable() {}

// Refresh 发布版为空操作。
func Refresh() {}

// Enabled 发布版恒为 false。
func Enabled() bool { return false }

// Wrap 原样返回入参（发布版不改变任何 Transport）。
func Wrap(client *http.Client, _ string) *http.Client { return client }
