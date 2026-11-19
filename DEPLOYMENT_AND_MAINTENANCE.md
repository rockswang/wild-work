# Wild-Work 线上部署与运维维护手册

本文档详细记录了 `wild-work` 在生产服务器（`aipark`）上的架构拓扑、容器配置、持久化路径、安全凭据、访问方式以及后续代码更新维护流程。

---

## 1. 核心架构与服务信息

| 配置项 | 参数值 | 说明 |
| :--- | :--- | :--- |
| **服务器主机名** | `aipark` (生产服务器) | 本地可通过 `ssh aipark` 免密连接 |
| **服务器部署目录** | `/opt/wild-work` | 存放代码、Compose 文件与环境变量 |
| **本地源码目录** | `/Users/mac/WorkSpace/ProxyAI/wild-work` | 本地开发与代码提交主目录 |
| **Docker 容器名称** | `wild-work` | 单实例运行 |
| **Docker 镜像名称** | `wild-work:local` | 基于源码 Dockerfile 本地构建 |
| **运行用户 (UID/GID)**| `10001:10001` | 非 root 权限安全运行，`no-new-privileges` |
| **重启策略** | `unless-stopped` | 宿主机重启或服务异常退出时自动拉起 |

---

## 2. Docker 网络拓扑与端口映射

### 2.1 网络名称
容器同时加入两个 Docker 网络：
1. **`app-network` (外部网络，核心通信链路)**：
   * **网络模式**：`external: true`
   * **容器内部 IP**：`172.19.0.5`
   * **容器 DNS 别名**：`wild-work`
   * **作用**：与 New-API 网关处于同一个隔离子网，New-API（渠道 #44）通过内网地址 `http://wild-work:7863` 直接高速寻址。
2. **`wild-work_default`**：
   * 容器的默认隔离网桥。

### 2.2 端口与公网安全策略
* **宿主机绑定端口**：`127.0.0.1:7863:7863`
* **安全设计**：
  * **禁止直接暴露在公网 `0.0.0.0`**，杜绝未授权扫描与恶意爆破。
  * 仅监听宿主机回环地址 `127.0.0.1`，外部管理员统一通过 **SSH 加密端口转发** 安全访问管理面板。

---

## 3. 持久化数据目录映射

宿主机持久化目录挂载至容器内部 `/data`：

| 宿主机路径 | 容器内挂载路径 | 说明 |
| :--- | :--- | :--- |
| `/opt/wild-work/runtime` | `/data` | 持久化根目录 |
| `/opt/wild-work/runtime/auths/` | `/data/auths/` | 账号鉴权凭据存储（如千问办公 Token） |
| `/opt/wild-work/runtime/data/state.json` | `/data/data/state.json` | 模型状态、动态配置与渠道映射缓存 |
| `/opt/wild-work/runtime/config.json` | `/data/config.json` | 基础运行时配置文件 |
| `/opt/wild-work/runtime/sanitize.toml` | `/data/sanitize.toml` | 敏感信息过滤与上游数据清洗规则 |

---

## 4. 账户密码与鉴权凭据

环境变量保存在服务器 `/opt/wild-work/.env`：

```bash
# New-API 渠道通信使用的 API-Key（对应 Channel #44 鉴权 Key）
WILDWORK_API_KEY=45adeaf8d341ceac34edcc11e4d72101

# Web 管理后台管理员密码
WILDWORK_ADMIN_PASSWORD=vzLVGS6pT_H_M5J5WYP7aHTe
```

### New-API 关联渠道配置（生产环境）
* **渠道 ID**：`#44`
* **渠道名称**：`千问办公 (wild-work)`
* **代理地址 (Base URL)**：`http://wild-work:7863`
* **绑定分组**：`国模`
* **模型池**：纯净 14 款官方标准模型名称（已剔除内部 `qwenwork/*` 前缀）

---

## 5. Web 管理后台访问方式

由于端口未向公网直接开放，访问后台需建立 SSH 隧道：

### 步骤 1：本地终端建立安全隧道
在本地电脑终端执行以下命令（保持窗口开启）：
```bash
ssh -N -L 7863:127.0.0.1:7863 aipark
```

### 步骤 2：浏览器打开
在本地浏览器中直接访问：
```text
http://127.0.0.1:7863
```

### 步骤 3：登录
* 在弹出的面板登录框中输入管理密码：
  ```text
  vzLVGS6pT_H_M5J5WYP7aHTe
  ```
* 登录后可查看当前挂载的账号状态、Token 余额、实时调用统计及日志。

---

## 6. GitHub 仓库与分支信息

* **个人 Fork 仓库（部署源）**：`https://github.com/hijqc/wild-work.git`
* **官方上游仓库**：`https://github.com/rockswang/wild-work.git`
* **当前核心开发与部署分支**：`feat/qwenwork-all-models`
* **已提交的上游 PR**：[rockswang/wild-work#229](https://github.com/rockswang/wild-work/pull/229)
  * 标题：`feat(qwenwork): 支持桌面端全量模型动态获取与别名映射，并规范化 usage chunk 避免严格客户端断连`

---

## 7. 后续更新与热部署方式

本地完成代码修改并验证后，可通过以下两种方式部署上线：

### 方式 A：一键全自动部署脚本（推荐）
在本地项目根目录运行：
```bash
cd /Users/mac/WorkSpace/ProxyAI/wild-work
./deploy.sh
```
**脚本自动化执行流程**：
1. 检查本地是否有未 commit 的脏代码；
2. 自动 push 当前分支至 GitHub（`hijqc/wild-work`）；
3. SSH 连接生产服务器 `aipark`，拉取最新代码：`git pull origin <branch>`；
4. 执行 `docker compose build && docker compose up -d` 重新构建并平滑重启容器；
5. 调用 `http://127.0.0.1:7863/healthz` 检查健康状态，确保 HTTP 200 上线成功。

---

### 方式 B：手动逐步更新维护
若需要手动逐步调试，可依次执行：

1. **本地推送**：
   ```bash
   git add .
   git commit -m "your commit message"
   git push origin feat/qwenwork-all-models
   ```

2. **登录服务器并拉取构建**：
   ```bash
   ssh aipark
   cd /opt/wild-work
   git pull origin feat/qwenwork-all-models
   docker compose build
   docker compose up -d
   ```

3. **查看运行日志与状态**：
   ```bash
   # 查看实时日志
   docker logs wild-work -f --tail 100

   # 健康检查
   curl -i http://127.0.0.1:7863/healthz
   ```
