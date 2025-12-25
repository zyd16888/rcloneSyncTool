# rclone-syncd

一个通用的 `rclone` 同步守护进程（Go），支持 **remote → remote**（任意 rclone 支持的后端），多规则并行、每规则 `copy/move`、每个任务独占 RC 端口隔离统计，并提供本地 Web 管理台用于配置与监控。

## 功能

- 多条规则：`src_remote:/src_path → dst_remote:/dst_path`
- 传输模式：`copy` 或 `move`（每条规则可配置）
- 统计隔离：每个任务启动独立 `rclone` 进程 + 独占 `--rc-addr 127.0.0.1:<port>`
- 全局并发限制：`global_max_jobs`（0 表示不限制）+ 每规则 `max_parallel_jobs`
- 实时监控：任务列表、速度/流量、任务详情实时日志
- 外部 rclone 配置：直接复用你现有的 `rclone.conf`（只依赖 remote 名称）

---

## 快速开始

### 方式 A：本地运行（二进制）

- 必须安装 `rclone`，并确保在 `PATH` 中
- Go 1.22+（本地开发/运行时需要；使用 Release 二进制不需要 Go）

```bash
./rclone-syncd -listen 127.0.0.1:8080 -data ./data
```

浏览器打开：`http://127.0.0.1:8080`

### 方式 B：Docker（推荐）

镜像内自带 `rclone`，并默认使用 `RCLONE_CONFIG=/data/rclone.conf`（与数据库/日志同目录，方便持久化）。

#### 直接使用 GHCR 镜像

```bash
# 官方 rclone（默认 tag）
docker pull ghcr.io/zyd16888/rclonesynctool:latest

# wiserain rclone（带 115 支持）
docker pull ghcr.io/zyd16888/rclonesynctool:latest-115
```

运行（持久化到当前目录 `./data`）：

```bash
mkdir -p ./data
touch ./data/rclone.conf
docker run --rm -p 8080:8080 -v "$(pwd)/data:/data" ghcr.io/zyd16888/rclonesynctool:latest
```

如需使用 115 版本，把镜像 tag 改为 `:latest-115`（或发布版本号的 `:vX.Y.Z-115`）：

```bash
docker run --rm -p 8080:8080 -v "$(pwd)/data:/data" ghcr.io/zyd16888/rclonesynctool:latest-115
```

容器内默认监听 `0.0.0.0:8080`。如需改端口：

- 只改端口：`-e PORT=9090`（等价于 `-listen 0.0.0.0:9090`，同时把端口映射改为 `-p 9090:9090`）
- 完整指定：`-e LISTEN_ADDR=127.0.0.1:8080` 或 `-e RCLONE_SYNCD_LISTEN=0.0.0.0:8080`

启动后：

1. 首次打开会进入「登录/初始化密码」
2. 打开页面「rclone 配置」，粘贴/编辑 `rclone.conf`（注意：不会自动创建新文件，必须已存在）
3. 打开「远程列表」确认能列出 remotes

#### 本地构建镜像

```bash
docker build -t rclone-syncd:latest .
```

### 方式 C：Docker Compose

```bash
mkdir -p ./data && touch ./data/rclone.conf
docker compose up -d
```

项目根目录自带 `docker-compose.yml`，数据默认映射到 `./data`（包含：SQLite、日志、`rclone.conf`）。

改宿主机端口（示例 9090）：

```bash
HOST_PORT=9090 docker compose up -d
```

### 数据目录说明（Docker / 本地一致）

`data` 目录会生成/使用：

- `115togd.db`：SQLite（规则/任务/状态）
- `logs/`：rclone 任务日志
- `rclone.conf`：rclone 配置文件（Docker 默认路径 `/data/rclone.conf`）

## 忘记密码（重置）

用命令行直接重置管理台密码（会写入 `data/115togd.db`，并让现有登录 cookie 失效）：

```bash
./rclone-syncd passwd -data ./data "新密码"
```

推荐避免出现在 shell history（从 stdin 读）：

```bash
echo "新密码" | ./rclone-syncd passwd -data ./data -stdin
```

Docker 中重置密码（推荐从 stdin）：

```bash
echo "新密码" | docker run --rm -i -v "$(pwd)/data:/data" ghcr.io/zyd16888/rclonesynctool:latest passwd -data /data -stdin
```

可选参数：

- `-listen`：Web 监听地址（例如 `127.0.0.1:8080` 或 `0.0.0.0:8080`）
- `-data`：数据目录（SQLite、日志等）

## 配置（rclone.conf）

- 推荐：打开页面「rclone 配置」直接编辑当前生效的配置文件（不会自动创建新文件）
- 或者在「系统设置」里填 `rclone_config_path` 指定配置文件路径（非 Docker 场景更常用）
- 设置完成后点击「检测 rclone」，或打开「远程列表」确认 remotes 正常

## 界面截图

把截图放到 `docs/screenshots/`，然后替换下面这些占位图：

![登录/初始化密码](https://github.com/user-attachments/assets/25384944-9f60-4378-a6a7-27dc6a6d7a1a)

![rclone 配置编辑](https://github.com/user-attachments/assets/ff1f296b-ed74-41a0-963c-8dd5231d6675)

![同步规则](https://github.com/user-attachments/assets/e3d2eeff-9201-4777-8576-7c3729957bfb)

![任务列表](https://github.com/user-attachments/assets/f73a4ea2-127b-42da-8e7a-5d2cf7372546)



## 打包发布（GitHub Actions）

对仓库打 tag（`v*`）会自动构建多平台并发布到 GitHub Releases：

```bash
git tag v0.1.0
git push origin v0.1.0
```

产物命名：`rclone-syncd_<os>_<arch>.tar.gz`（Windows 为 `.zip`）。

同时会构建并推送 Docker 镜像到 GHCR：

- 官方 rclone：
  - `ghcr.io/<owner>/<repo>:vX.Y.Z`
  - `ghcr.io/<owner>/<repo>:latest`
- wiserain rclone（带 115 支持）：
  - `ghcr.io/<owner>/<repo>:vX.Y.Z-115`
  - `ghcr.io/<owner>/<repo>:latest-115`
  - 说明：镜像内通过 `install.sh` 安装 wiserain rclone；会尽量构建 `linux/amd64` + `linux/arm64`，若 wiserain release 未提供 arm64，则回退为 `linux/arm/v6` 或仅构建 `linux/amd64`

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
