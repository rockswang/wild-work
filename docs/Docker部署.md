# Docker 部署

适合 7×24 小时开机的设备（NAS、软路由、云服务器），把 wild-work 当作常驻 API 中转服务。

## 快速开始

1. 准备一台装好 Docker 的机器，把本仓库代码拉下来（或只拿 `Dockerfile` + `docker-compose.yml` 两个文件也行）。

2. 编辑 `docker-compose.yml`，把 `WILDWORK_ADMIN_PASSWORD` 改成你自己的强密码（不少于 8 位）。
   监听 `0.0.0.0` 时程序强制要求设置管理员密码，否则容器启动后会直接退出并提示。

3. 启动：

   ```bash
   docker compose up -d --build
   ```

4. 浏览器打开 `http://设备IP:7863`，用管理员密码登录面板，添加账号即可。

## 数据持久化

`./auths`（账号凭据）与 `./data`（运行状态、积分流水）已映射到宿主机目录，
容器删除重建后数据不丢失。备份时拷走这两个目录即可。

## 常用命令

```bash
docker compose logs -f          # 看日志
docker compose down             # 停止并删除容器（数据保留）
docker compose up -d --build    # 更新代码后重新构建启动
```

## 环境变量

| 变量 | 说明 | 默认值 |
|---|---|---|
| `WILDWORK_LISTEN` | 监听地址，容器内固定 `0.0.0.0:7863` | `0.0.0.0:7863` |
| `WILDWORK_ADMIN_PASSWORD` | ★必填，管理面板密码 | 无 |
| `WILDWORK_API_KEY` | 下游调用 `/v1` 接口的密钥 | `WildWorkAPI` |
| `WILDWORK_REGION` | 区域 `cn` / `intl` | `cn` |
| `TZ` | 时区，定时签到按此时区触发 | `Asia/Shanghai` |

完整变量列表见 `internal/config/config.go` 的 `applyEnv`（`WILDWORK_AUTH_DIR`、`WILDWORK_STATE_FILE` 等）。

## 已知限制

- 部分渠道的登录流程需要拉起浏览器走 CDP 自动捕获凭据（例如智谱清言），容器内无桌面浏览器，
  这类渠道建议先在桌面端登录，再把 `auths/` 目录拷到宿主机对应位置，容器可直接复用已登录凭据。
- 导入型渠道（例如 Loomy，从本机已登录客户端读取凭据）同样需要先在宿主机准备好凭据。
