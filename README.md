# rclone-syncd

一个通用的 `rclone` 同步守护进程（Go），支持 **remote → remote**（任意 rclone 支持的后端），多规则并行、每规则 `copy/move`、每个任务独占 RC 端口隔离统计，并提供本地 Web 管理台用于配置与监控。

## 功能

- 多条规则：`src_remote:/src_path → dst_remote:/dst_path`
- 传输模式：`copy` 或 `move`（每条规则可配置）
- 统计隔离：每个任务启动独立 `rclone` 进程 + 独占 `--rc-addr 127.0.0.1:<port>`
- 全局并发限制：`global_max_jobs`（0 表示不限制）+ 每规则 `max_parallel_jobs`
- 实时监控：任务列表、速度/流量、任务详情实时日志
- 外部 rclone 配置：直接复用你现有的 `rclone.conf`（只依赖 remote 名称）

## 依赖

- 必须安装 `rclone`，并确保在 `PATH` 中
- Go 1.22+（本地开发/运行时需要；使用 Release 二进制不需要 Go）

## 运行

```bash
./rclone-syncd -listen 127.0.0.1:8080 -data ./data
```

浏览器打开：`http://127.0.0.1:8080`

可选参数：

- `-listen`：Web 监听地址（例如 `127.0.0.1:8080` 或 `0.0.0.0:8080`）
- `-data`：数据目录（SQLite、日志等）

数据目录会生成：

- `data/115togd.db`（SQLite，运行状态/规则/任务等）
- `data/logs/`（rclone 日志）

## 配置（外部 rclone.conf）

1. 打开「系统设置」
2. 填 `rclone_config_path`：
   - 留空：使用 rclone 默认配置路径
   - 或填写你的 rclone.conf 绝对路径
3. 点击「检测 rclone」确认能列出 remotes
4. 在「同步规则」里创建规则，填写 `src_remote/dst_remote`（remote 名称）与路径

## 打包发布（GitHub Actions）

对仓库打 tag（`v*`）会自动构建多平台并发布到 GitHub Releases：

```bash
git tag v0.1.0
git push origin v0.1.0
```

产物命名：`rclone-syncd_<os>_<arch>.tar.gz`（Windows 为 `.zip`）。

## systemd（Linux 后台运行）

示例：将二进制放到 `/usr/local/bin/rclone-syncd`，数据目录放到 `/var/lib/rclone-syncd`。

1) 创建用户与目录（可选但推荐）：

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin rclone-syncd || true
sudo mkdir -p /var/lib/rclone-syncd
sudo chown -R rclone-syncd:rclone-syncd /var/lib/rclone-syncd
```

2) 创建 service 文件：`/etc/systemd/system/rclone-syncd.service`

```ini
[Unit]
Description=rclone remote-to-remote sync daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=rclone-syncd
Group=rclone-syncd
ExecStart=/usr/local/bin/rclone-syncd -listen 127.0.0.1:8080 -data /var/lib/rclone-syncd
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/rclone-syncd

[Install]
WantedBy=multi-user.target
```

3) 启动并设置开机自启：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now rclone-syncd
sudo systemctl status rclone-syncd
```

提示：Web 监听地址可通过 `-listen` 修改；如需远程访问，建议用 Nginx/Caddy 做 HTTPS 反代并加认证。
