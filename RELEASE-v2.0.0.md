# wild-work v2.0.0 发布说明

> **重磅重构**：弃用 wails/WebView2，改为「系统托盘 daemon + 浏览器 Web UI」轻量架构，内存占用更小、跨平台更简单。支持 WorkBuddy、TraeWork、Qoder 三渠道账号聚合。

---

## 🎉 新功能

### 1. 轻量级架构重构
- ✅ **移除 wails/WebView2 依赖**：不再需要 WebView2 运行时，安装包体积更小
- ✅ **纯静态 Web UI**：HTML/CSS/JS 通过 `go:embed` 打包进单文件，无前端构建链
- ✅ **系统托盘增强**：固定菜单（打开主界面 / 查看日志 / 退出），双击托盘快速打开面板
- ✅ **无头模式**：Linux 服务器可用 `--no-tray` 参数启动，阻塞运行等待 Ctrl+C

### 2. 新增 Qoder 渠道
- ✅ **Qoder 账号集成**：支持 OAuth + 设备注册登录流程
- ✅ **COSY 网关聊天**：实现 `/algo/api/v2/agent_chat_generation` 端点调用
- ✅ **思考开关透传**：自动将 `reasoning_effort`/`thinking` 请求参数映射到上游
- ✅ **嵌套 SSE 解析**：处理 `data:{"body":"<json>"}` 格式的流式响应

### 3. TraeWork 体验优化
- ✅ **积分明细分组显示**：按 `group_name` 区分"每日签到"、"每月登录积分"、"用户福利"等来源
- ✅ **PKCE + AuthCode 登录**：升级新版 PKCE 流程，兼容官方客户端 0.1.52+
- ✅ **签到 9074 修复**：设备指纹头补全 + device_id 持久化为 15 位数字
- ✅ **SOLOHeaders 鉴权**：Cloud-IDE-JWT 动态生成，token 保活机制完善

### 4. 粘性路由优化
- ✅ **会话缓存利用率提升**：优先复用上次成功账号，连续成功 50 次后再轮换
- ✅ **错误冷却机制**：遭遇 400/401 等错误时清除粘性记录，避免无效重试
- ✅ **状态机完善**：冷却/禁用/解冻状态自动管理，减少人工干预

### 5. 开发者体验
- ✅ **Pi models.json 示例**：提供完整配置模板，支持 WorkBuddy/TraeWork/Qoder 模型列表
- ✅ **developer→system 角色转换**：自动将下游 Agent 的 `<developer>` 改写为 `<system>`，避免内容过滤
- ✅ **定价缓存持久化**：`data/pricing-cache.json` 存储费率，启动加载 + 1 小时自动刷新

---

## 🔧 重大重构

### 架构调整
```diff
- workbuddy-wild (v0.2.x): wails + WebView2 GUI
+ wild-work (v2.0.x): 系统托盘 daemon + 浏览器 Web UI

模块重组:
- cmd/workbuddy-wild/     → cmd/wild-work/
- internal/workbuddy/     → internal/upstream/ (通用 upstream 接口)
- internal/traework/      → 保留 (TraeWork 专用)
- internal/qoder/         → 新增 (Qoder 专用)
- internal/login/         → 拆分 login_trae/, login_qoder/
- internal/systray/       → 保留 + platform build tag 拆分
```

### 数据迁移
- ✅ **旧 state.json 自动迁移**：启动时自动拆分到 `state-workbuddy.json` / `state-traework.json` / `state-qoder.json`
- ✅ **config.listen 兼容**：支持新对象格式 `{"host":"port"}` + 旧字符串格式 `":7863"`
- ✅ **auth 文件格式统一**：嵌套形 `{auth:{...},account:{...}}`，Parse 与 SaveAtomic 同步更新

---

## 🐛 Bug Fix

### 核心功能
- ✅ **批量操作 404 修复**：`refresh_all` / `checkin_all` 按钮需传 body 触发 POST，否则返回 404
- ✅ **TraeWork struct 语法错误**：补全 `var resp struct {` 缺失声明
- ✅ **Go 版本兼容性**：修正 `go.mod` 从 `1.25` 恢复为 `1.25.0`（CI 兼容）
- ✅ **GitHub Actions 产物路径**：fix download-artifact 顺序，先 checkout 再下载 artifact

### 稳定性
- ✅ **托盘 panic 处理**：无桌面 Linux 环境下托盘初始化失败时优雅 exit，提示 `--no-tray` 参数
- ✅ **CGO_ENABLED=0 交叉编译**：Windows 构建无需 cgo，WSL 可交叉编译 amd64
- ✅ **上游错误透传**：HTTP ≥400 直接返回原始响应体，不包装错误码

---

## 📦 多平台构建

| 平台 | 架构 | 状态 | 说明 |
|------|------|------|------|
| Windows | amd64 | ✅ 已测试 | `.exe` 单文件，双击启动 |
| macOS | arm64 | ⚠️ CI 构建 | M1/M2/M3芯片，需真机测试 |
| macOS | amd64 | ⚠️ CI 构建 | Intel 芯片，需真机测试 |
| Linux | amd64 | ⚠️ CI 构建 | Ubuntu/Debian，建议 `--no-tray` |
| Linux | arm64 | ⚠️ CI 构建 | Raspberry Pi/ARM 服务器，需测试 |

> ⚠️ **注意**：macOS/Linux 版本为 GitHub Actions 自动化构建，未经真机深度测试，请试用者重点反馈相关平台问题。

---

## 📊 技术栈

- **语言**: Go 1.25.0
- **托盘库**: energye/systray v1.0.3 (纯 Go 生成图标)
- **前端**: 纯静态 HTML/CSS/JS (go:embed)
- **构建**: WSL 交叉编译 (Windows) / GitHub Actions (macOS/Linux)
- **协议**: OpenAI 兼容 API (`/v1/chat/completions`, `/v1/models`)

---

## 🔄 升级指南

### 从 v0.2.4 升级
1. **停止旧版**：关闭 workbuddy-wild.exe 进程
2. **备份数据**（可选）：复制 `config.json` 和 `auths/` 目录
3. **替换二进制**：下载 wild-work.exe 覆盖旧文件
4. **首次启动**：自动迁移旧数据，无需额外操作

### 配置变更
- **API Key**: 默认仍为 `WildWorkAPI`（未设鉴权）
- **监听端口**: 默认 `127.0.0.1:7863`
- **Web UI 地址**: `http://127.0.0.1:7863/`

---

## 📝 已知问题

1. **macOS/Linux 未充分测试**：建议先在 Windows 验证核心功能
2. **TraeWork token 过期**：session_dead (code 1001) 错误需重新登录
3. **Qoder 无签到活动**：仅做 token keepalive，无额度奖励
4. **Sticky Routing 冷却**：连续失败后粘性记录会清除，需手动刷新账号

---

## 🙏 致谢

本项目基于 [workbuddy-wild](https://github.com/rockswang/workbuddy-wild) (v0.2.4) 重构，感谢社区对原项目的贡献。

---

**发布日期**: 2026-08-26  
**作者**: rockswang  
**仓库**: https://github.com/rockswang/wild-work
