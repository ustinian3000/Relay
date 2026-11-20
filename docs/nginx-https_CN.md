# 用公网 IP 给 Relay 配置 Nginx + HTTPS

[English](nginx-https.md)

## 部署前

- 适用于 Ubuntu x86_64（systemd）。Relay 已在同一台服务器运行，默认端口 `8317`，服务器上执行 `curl http://127.0.0.1:8317/healthz` 应返回 `{"status":"ok"}`。
- 使用固定公网 IPv4。在云安全组放行入站 `TCP 443`，并允许出站 HTTPS。如果服务器启用了 UFW，也执行 `sudo ufw allow 443/tcp`。这个版本**不用开放 `80`**。
- `443` 及本机 `18443–18445` 要可用。若服务器已有其他站点监听 `443`，先合并 Nginx 配置，不要直接运行安装脚本。现有已正常工作的 Relay HTTPS 实例不用重新安装。

## 安装和切换

1. 把仓库放到服务器，在仓库根目录运行。把引号里的文字替换成服务器的真实公网 IPv4，最后一个参数是 Relay 的端口：

   ```bash
   sudo apt-get update
   sudo bash deploy/nginx-https/install.sh '你的公网IPv4' 8317
   ```

   脚本安装 Nginx 和固定版本的 lego（校验安装包 SHA-256），通过 `443` 签发 Let’s Encrypt 公网 IP 证书，并启用 systemd 自动续期。Nginx 直接转发 Relay 业务接口，支持流式响应；管理页和管理接口不经这个入口发布。脚本不改 Relay 的配置、账号、SK 或旧端口。

2. 在自己的电脑上运行 `curl`（Windows PowerShell 可用 `curl.exe`）：

   ```bash
   curl -fsS https://你的公网IPv4/healthz
   ```

   返回 `{"status":"ok"}` 就说明 HTTPS 链和入口可用；直接打开 `/` 返回 `404` 是正常的。不要用 `-k` 跳过证书校验。

3. 通知客户端改地址，**SK 不变**：Claude Code 的 `ANTHROPIC_BASE_URL` 填 `https://你的公网IPv4`；OpenAI 兼容程序的 Base URL 填 `https://你的公网IPv4/v1`。程序会自行拼上 `/chat/completions` 等路径。所有客户端切换后，再移除云安全组中旧 Relay 端口的公网入站规则，或把 Relay 改为只监听 `127.0.0.1`。

## 续期与排查

证书免费、有效期不到一周。`relay-acme-renew.timer` 每 6 小时检查，续期成功后重新加载 Nginx。证书验证连接和业务流量共用公网 `443`，由 Nginx 按 TLS 协议标记分别转发，所以续期无需停止 HTTPS 服务。

```bash
systemctl list-timers relay-acme-renew.timer
sudo journalctl -u relay-acme-renew.service -n 30 --no-pager
sudo nginx -t
```

- **签发或续期连接超时：** 核对公网 IP，以及该实例实际绑定的安全组和系统防火墙是否放行 `443`。
- **接口返回 `404`：** 检查客户端类型和 Base URL，尤其是 OpenAI 兼容地址有没有 `/v1`。
- **接口返回 `401`：** 请求已到 Relay，检查业务 SK 和客户端的认证配置。

证书和 ACME 账户保存在服务器的 `/var/lib/relay-acme`，包含私钥，请勿复制进仓库。
