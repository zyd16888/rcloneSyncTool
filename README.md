# rclone-syncd

一个轻量级的 `rclone` 同步任务管理工具（Go），提供 Web 界面来管理和监控 rclone 任务。

支持 **Remote → Remote** 及 **Local → Remote** 的自动同步，具备多规则管理、队列调度、流量限制等功能。

## 核心功能

- **Web 管理界面**：直观配置 rclone，无需手写复杂命令。
- **灵活规则**：支持 `copy` 或 `move` 模式，可配置扫描间隔、最小文件大小、并发数等。
- **流量限制分组**：独创 **Limit Groups** 功能，可将多个规则聚合（例如多个文件夹同步到同一个 Google Drive），共享每日传输配额（如 750G），精准防超限。
- **任务调度**：内置队列系统，支持全局并发控制和单规则并发控制。
- **实时监控**：仪表盘展示实时速度、今日/24小时流量统计、任务日志流。
- **独立环境**：每个任务运行在独立的 rclone 进程中，互不干扰。

## 快速开始

### 方式 A：Docker（推荐）

镜像内置 rclone，开箱即用。

```bash
mkdir -p ./data
touch ./data/rclone.conf

docker run -d \
  --name rclone-syncd \
  --restart unless-stopped \
  -p 8080:8080 \
  -v "$(pwd)/data:/data" \
  ghcr.io/zyd16888/rclonesynctool:latest
```

> **提示**：如需支持 115 网盘（使用 wiserain 修改版 rclone），请使用标签 `:latest-115`。

访问 `http://localhost:8080`，首次登录需设置密码。

### 方式 B：Docker Compose

```yaml
version: '3' 
services:
  app:
    image: ghcr.io/zyd16888/rclonesynctool:latest
    container_name: rclone-syncd
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
```

### 方式 C：直接运行

需自行安装 `rclone` 并确保在系统 PATH 中。

```bash
# 下载对应平台的二进制文件 release
./rclone-syncd -listen 127.0.0.1:8080 -data ./data
```

### 方式 D：Systemd（Linux 后台运行）

适合在 VPS 或 Linux 服务器上作为服务长期运行。

1.  **准备用户和目录**：
    ```bash
    sudo useradd --system --no-create-home --shell /usr/sbin/nologin rclone-syncd || true
    sudo mkdir -p /var/lib/rclone-syncd
    sudo chown -R rclone-syncd:rclone-syncd /var/lib/rclone-syncd
    ```
2.  **安装二进制**：将下载的 `rclone-syncd` 放到 `/usr/local/bin/` 并赋予执行权限。
3.  **创建服务文件** `/etc/systemd/system/rclone-syncd.service`：
    ```ini
    [Unit]
    Description=rclone-syncd service
    After=network-online.target

    [Service]
    Type=simple
    User=rclone-syncd
    Group=rclone-syncd
    ExecStart=/usr/local/bin/rclone-syncd -listen 127.0.0.1:8080 -data /var/lib/rclone-syncd
    Restart=on-failure
    RestartSec=3
    # 安全加固（可选）
    NoNewPrivileges=true
    PrivateTmp=true
    ProtectSystem=strict
    ReadWritePaths=/var/lib/rclone-syncd

    [Install]
    WantedBy=multi-user.target
    ```
4.  **启动服务**：
    ```bash
    sudo systemctl daemon-reload
    sudo systemctl enable --now rclone-syncd
    sudo systemctl status rclone-syncd
    ```

## 使用指南

1.  **配置 Rclone**：
    *   进入 **rclone 配置** 页面，粘贴或编辑你的 `rclone.conf` 内容。
    *   或者直接挂载已有的 `rclone.conf` 到 `/data/rclone.conf`。
2.  **创建规则**：
    *   进入 **同步规则** -> **新建规则**。
    *   填写源路径（Remote 或 Local）和目标路径（Remote）。
    *   设置模式（Copy/Move）和参数。
3.  **设置限流（可选）**：
    *   进入 **管理限流分组**，创建一个分组（如 `gdrive_main`），设置每日上限（如 `740G`）。
    *   在编辑规则时，选择该分组。所有关联该分组的规则将共享流量配额，超限自动暂停新任务。
4.  **监控**：
    *   在 **概览** 页查看实时速度和流量统计。
    *   在 **任务列表** 查看正在运行或已完成的任务详情。

## 重置密码

如果忘记 Web 登录密码，可通过命令行重置：

**Docker 环境：**
```bash
echo "新密码" | docker exec -i rclone-syncd /app/rclone-syncd passwd -data /data -stdin
```

**本地环境：**
```bash
echo "新密码" | ./rclone-syncd passwd -data ./data -stdin
```

## 截图预览

*(此处保留原有截图链接或更新)*

![登录/初始化密码](https://github.com/user-attachments/assets/25384944-9f60-4378-a6a7-27dc6a6d7a1a)
![同步规则](https://github.com/user-attachments/assets/e3d2eeff-9201-4777-8576-7c3729957bfb)