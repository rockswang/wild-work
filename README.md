# wild-work

> 多渠道账号聚合桌面工具——把 WorkBuddy(CodeBuddy)、TraeWork、Qoder 的多个账号聚合成一个 OpenAI 兼容 API，双击启动，浏览器管理。

[![GitHub](https://img.shields.io/badge/GitHub-rockswang%2Fworkbuddy--wild-blue)](https://github.com/rockswang/workbuddy-wild)

## 功能

- **OpenAI 兼容代理**：`/v1/chat/completions`、`/v1/models`，支持流式/非流式，模型前缀路由
- **三渠道聚合**：WorkBuddy(CodeBuddy) + TraeWork + Qoder，按余额自动挑号
- **自动签到**：每日定时签到领额度，token 保活，冷却状态机
- **Web 管理面板**：账号管理（添加/签到/刷新/停用/删除）、积分明细、渠道费率、API 配置
- **系统托盘**：常驻右下角，双击打开面板，右键菜单操作
- **跨平台**：Windows（完整支持）、macOS（代码已就绪，CI 构建）、Linux（无头模式）

## 项目渊源

本项目是 [workbuddy-wild](https://github.com/rockswang/workbuddy-wild)（v0.2.x）的重构版，去掉 wails/WebView2 GUI，改为「系统托盘 + 系统浏览器 Web UI」的轻量架构，内存占用更小、跨平台更简单。

## 使用方式

### Windows

1. 从 [Releases](https://github.com/rockswang/workbuddy-wild/releases) 下载 `wild-work.exe`
2. 放到任意目录，双击启动
3. 右下角出现 W 图标，**双击托盘图标** → 浏览器打开 Web 管理面板
4. 在面板中点击「+ WorkBuddy」或「+ TraeWork」或「+ Qoder」添加账号
5. 将 AI 客户端（如 Pi、ChatGPT-Next-Web）指向：

```
Base URL: http://127.0.0.1:7863/v1
API Key:  WildWorkAPI
```

6. 模型 ID 需带渠道前缀，例如：`workbuddy/glm-5.3`、`traework/DeepSeek-V4-Pro`、`qoder/qwen3.8-max`

### 托盘菜单

- **打开主界面**：在浏览器中打开管理面板
- **查看日志**：用记事本打开运行日志
- **退出**：退出程序

### 无头模式（Linux 服务器）

```bash
./wild-work --no-tray
```

启动后打印 API 地址、Key 等信息，阻塞运行，Ctrl+C 退出。

## 配置说明（以 Pi 为例）

在 Pi 中配置 wild-work 作为模型提供方：

1. 打开 Pi 设置 → 模型
2. 添加自定义 Provider：
   - **名称**：wild-work
   - **Base URL**：`http://127.0.0.1:7863/v1`
   - **API Key**：`WildWorkAPI`
3. 在模型列表中启用带前缀的模型，如 `workbuddy/auto`、`qoder/qwen3.8-max`

## 面板操作指南

所有操作在 Web 管理面板中完成，按直觉操作即可：

- **添加账号**：点击渠道按钮 → 确认对话框 → 浏览器窗口登录 → 自动完成
- **账号管理**：卡片显示积分、签到状态；图标按钮操作（签到 ✓ / 刷新 ↻ / 停用 ⏸ / 删除 ✕）
- **积分明细**：鼠标悬停积分数字显示套餐明细
- **刷新积分**：面板顶部按钮，批量刷新全部账号余额
- **渠道费率**：点击「刷新费率」从上游拉取最新模型定价
- **API 配置**：点击页面顶部 API 地址或 Key 修改

## License

MIT — 仅供个人学习使用，请遵守各上游平台服务条款。