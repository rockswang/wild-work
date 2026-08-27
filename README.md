# wild-work

> 多渠道账号聚合桌面工具——把 WorkBuddy(CodeBuddy)、TraeWork、Qoder 的多个账号聚合成一个 OpenAI 兼容 API，双击启动，浏览器管理。

[![GitHub](https://img.shields.io/badge/GitHub-rockswang%2Fworkbuddy--wild-blue)](https://github.com/rockswang/workbuddy-wild)

## 功能

- **OpenAI 兼容代理**：`/v1/chat/completions`、`/v1/models`，支持流式/非流式，模型前缀路由
- **三渠道聚合**：WorkBuddy(CodeBuddy) + TraeWork + Qoder，模型前缀路由，粘性路由优先复用账号以提升会话缓存利用率
- **自动签到**：每日定时签到领额度，token 保活，冷却状态机
- **Web 管理面板**：账号管理（添加/签到/刷新/停用/删除）、积分明细、渠道费率、API 配置
- **系统托盘**：常驻右下角，双击打开面板，右键菜单操作
- **跨平台**：Windows（完整支持）、macOS（代码已就绪，CI 构建）、Linux（无头模式）
- **developer→system 角色转换**：自动将下游 Agent 发送的 `<developer>` 角色改写为 `<system>`，避免上游触发内容过滤

## 项目渊源

本项目是 [workbuddy-wild](https://github.com/rockswang/workbuddy-wild)（v0.2.x）的重构版，去掉 wails/WebView2 GUI，改为「系统托盘 + 系统浏览器 Web UI」的轻量架构，内存占用更小、跨平台更简单。

## 使用方式

### Windows

1. 从 [Releases](https://github.com/rockswang/workbuddy-wild/releases) 下载 `wild-work.exe`
2. 放到任意目录，双击启动
3. 右下角出现 W 图标，**双击托盘图标** → 浏览器打开 Web 管理面板
4. 在面板中点击「+ WorkBuddy」或「+ TraeWork」或「+ Qoder」添加账号
5. 根据下方配置说明接入你的 AI 客户端

### 托盘菜单

- **打开主界面**：在浏览器中打开管理面板
- **查看日志**：用记事本打开运行日志
- **退出**：退出程序

### 无头模式（Linux 服务器）

```bash
./wild-work --no-tray
```

启动后打印 API 地址、Key 等信息，阻塞运行，Ctrl+C 退出。

## 配置 AI 客户端

### 1. 获取模型列表

wild-work 的 `/v1/models` 端点返回当前所有可用模型。在终端中执行：

```bash
curl -s -H "Authorization: Bearer WildWorkAPI" http://127.0.0.1:7863/v1/models
```

### 2. 配置 Pi（models.json）

Pi 不支持自动拉取模型列表，需要手动编辑 `~/.pi/agent/models.json`（Windows 路径 `C:\Users\<用户名>\.pi\agent\models.json`），在 `providers` 中加入 wild-work 配置：

```json
{
  "providers": {
    "wild-work": {
      "name": "wild-work",
      "api": "openai-completions",
      "baseUrl": "http://127.0.0.1:7863/v1",
      "apiKey": "WildWorkAPI",
      "models": [
        {
          "id": "workbuddy/auto",
          "name": "自动路由 (WorkBuddy)",
          "reasoning": false,
          "input": ["text"],
          "contextWindow": 168000,
          "maxTokens": 32000,
          "compat": { "maxTokensField": "max_tokens" }
        },
        {
          "id": "workbuddy/deepseek-v4-pro",
          "name": "DeepSeek V4 Pro (WorkBuddy)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 50000,
          "compat": {
            "thinkingFormat": "deepseek",
            "supportsReasoningEffort": true,
            "maxTokensField": "max_tokens"
          }
        },
        {
          "id": "workbuddy/glm-5.3",
          "name": "GLM-5.3 (WorkBuddy)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 48000,
          "compat": { "maxTokensField": "max_tokens" }
        },
        {
          "id": "traework/DeepSeek-V4-Pro",
          "name": "DeepSeek V4 Pro (TraeWork)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 50000,
          "compat": {
            "thinkingFormat": "deepseek",
            "supportsReasoningEffort": true,
            "maxTokensField": "max_tokens"
          }
        },
        {
          "id": "traework/glm-5.2",
          "name": "GLM-5.2 (TraeWork)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 48000,
          "compat": { "maxTokensField": "max_tokens" }
        },
        {
          "id": "qoder/qwen3.8-max",
          "name": "Qwen3.8-Max (Qoder)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 180000,
          "maxTokens": 32000,
          "compat": { "maxTokensField": "max_tokens" }
        }
      ]
    }
  }
}
```

> 上面只列出了部分常用模型，完整列表请通过 `/v1/models` 端点获取后自行添加。

### 3. 其他客户端

支持 OpenAI 兼容 API 的客户端均可接入：

```
Base URL: http://127.0.0.1:7863/v1
API Key:  WildWorkAPI
```

模型 ID 需带渠道前缀：`workbuddy/<model>`、`traework/<model>`、`qoder/<model>`。

## 面板操作指南

所有操作在 Web 管理面板中完成，按直觉操作即可：

- **添加账号**：点击渠道按钮 → 确认对话框 → 浏览器窗口登录 → 自动完成
- **账号管理**：卡片显示积分、签到状态；图标按钮操作（签到 ✓ / 刷新 ↻ / 停用 ⏸ / 删除 ✕）
- **积分明细**：鼠标悬停积分数字显示套餐明细
- **刷新积分**：面板顶部按钮，批量刷新全部账号余额
- **渠道费率**：点击「刷新费率」从上游拉取最新模型定价
- **API 配置**：点击页面顶部 API 地址或 Key 修改

## 常见问题

### 1. Windows 提示“未知发布者”或报毒？

本工具没有数字签名，因此可能会被 Windows 安全中心或杀毒软件拦截。如果报毒，请将本工具添加到排除项，方法如下：

```
设置 → 更新和安全 → Windows 安全中心 → 打开 Windows 安全中心 → 病毒和威胁防护 → “病毒和威胁防护”设置/管理设置 → 排除项/添加或删除排除项 → 添加排除项（文件或文件夹），把 exe 或所在目录加进去。
```

如果对本仓库代码不放心，可以克隆到本地让 Agent 帮你审查一遍然后本地构建。

### 2. TraeWork 通用积分不能用

已知问题，原上游项目也没解决这个问题，建议多开几个 WorkBuddy 账号，速度快不排队。

### 3. TraeWork DeepSeek V4 Flash 模型响应慢

在官方客户端里这个模型也会排队，我的办法是 `ds4f` 用 WorkBuddy 的，`ds4p` 用 TraeWork 的。

如果对本仓库发布包不放心，可以自行基于源码构建（见 [DEVELOPMENT.md](DEVELOPMENT.md)）。

## License

MIT — 仅供个人学习使用，请遵守各上游平台服务条款。
