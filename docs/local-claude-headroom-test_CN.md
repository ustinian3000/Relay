# 本地 Claude + Headroom 测试记录

## 测试目的

本记录对应 2026-11-18（日本时间）的本地迁移测试：将服务器上的一个 Claude 账号停用，在 Windows 本地 Relay 中使用，并让请求经过本地 Headroom。

测试不把账号 Token、SK、住宅代理用户名、住宅代理密码或 SSH 私钥写入仓库。真实配置位于被 `.gitignore` 忽略的本地文件中。

## 链路

```text
Claude 客户端
    │
    ▼
Headroom 0.44.0
127.0.0.1:8788
    │  ANTHROPIC_TARGET_API_URL
    ▼
Relay
127.0.0.1:8318
    │  按认证文件中的 proxy_url 出口
    ▼
Claude 上游
```

本轮本地账号使用固定住宅出口。代理认证信息只保存在本地认证 JSON 中，不记录在本文档。

## 本地文件

| 文件 | 作用 | 是否提交 |
| --- | --- | --- |
| `config.local.yaml` | 从服务器配置生成的本地测试配置，端口为 `8318` | 否，已被忽略 |
| `auths/claude-<account>.json` | 本地 Claude 认证文件 | 否，`auths/` 已被忽略 |
| `runtime/local-claude-test/run.ps1` | 启动 Relay 并运行代理守护 | 否，`runtime/` 已被忽略 |
| `runtime/local-claude-test/expected-proxies.json` | 启动时记录代理指纹和出口 IP | 否 |
| `runtime/headroom-test/start.ps1` | 启动 Headroom 测试实例 | 否，`runtime/` 已被忽略 |

本地 `config.local.yaml` 的关键设置如下，敏感值不在示例中展开：

```yaml
server:
  host: 127.0.0.1
  port: 8318

oauth:
  auth-dir: ./auths

requests:
  # 认证文件缺少代理时，故意指向不可用的本地端口，避免回落直连。
  proxy-url: http://127.0.0.1:1

plugins:
  dir: ./plugins
```

账号认证文件中的 `proxy_url` 优先级高于全局代理。全局不可用端口只负责兜底失败，确保缺少账号专属代理时请求直接失败。

## 失败即停守护

`runtime/local-claude-test/run.ps1` 启动 Relay 前会为每个 Claude 认证文件记录：

- `proxy_url` 的 SHA-256 指纹；
- 代理主机和端口；
- 通过该代理访问 `api.ipify.org` 得到的出口 IP。

守护循环每 30 秒检查一次：

1. Relay 进程是否仍在运行；
2. 认证文件中的代理地址是否被改动；
3. 代理出口 IP 是否改变；
4. 代理是否能建立 HTTPS 连接。

任一检查失败，守护进程会停止本地 `8318`。不会切换其他代理，也不会回落直连。测试中曾出现一次住宅代理 `CONNECT` 失败，`8318` 按预期自动停止；代理恢复后重新启动成功。

启动本地 Relay：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\runtime\local-claude-test\run.ps1
```

停止本地 Relay：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\runtime\local-claude-test\run.ps1 -Stop
```

## Headroom 配置

本轮使用 Headroom `0.44.0`，安装在 `runtime/headroom-venv`，监听 `127.0.0.1:8788`。

启动参数：

```text
mode=cache
savings_profile=coding
stateless=true
rate_limit=false
kompress=disabled
anthropic_api_url=http://127.0.0.1:8318
offline=true
```

`HEADROOM_OFFLINE=1` 用于关闭 Headroom 的外部更新、遥测和模型下载；本地请求转发到 `8318` 不受影响。本轮关闭 Kompress，先验证基础压缩和 Claude 转发链路。

启动 Headroom：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\runtime\headroom-test\start.ps1
```

健康检查：

```powershell
Invoke-WebRequest http://127.0.0.1:8788/health
```

## 测试结果

### 已通过

- 本地 Relay 在 `127.0.0.1:8318` 正常启动。
- 两个迁移认证文件可以被本地 Relay 加载。
- 本地账号代理出口检查通过，出口地址与预期配置一致。
- Headroom 在 `127.0.0.1:8788` 正常启动，健康检查为 `healthy`。
- Headroom `/v1/models` 请求可以转发到本地 Relay。
- 固定工具 JSON 样本从 `5901` token 压缩到 `2174` token，节省 `3727` token，压缩率约 `63.16%`。
- 服务器端账号设置为 `disabled: true` 后，原服务器请求返回 `auth_not_found`，不再调度该账号。
- 本地账号经过 Headroom 请求时，已经进入本地 Relay 和账号专属代理链路。
- 额度恢复后，使用模型 `claude-haiku-4-5-20251001` 完成真实请求，返回 `local-headroom-ok`，HTTP 状态为 `200`。
- 真实请求 usage 为输入 `109` token、输出 `9` token，Headroom、Relay 和代理守护均保持正常。
- 服务器端认证文件已移出活动认证目录，服务器同一 SK 请求返回 `auth_not_found`。

### 测试过程中的失败与恢复

额度恢复后的第一次请求遇到住宅代理 `CONNECT` 中止。现在的守护行为已经调整为账号级隔离：Relay `8318` 和 Headroom `8788` 保持运行，只将对应认证文件临时标记为 `disabled: true`，请求不会进入该账号，也不会直连或切换到未授权兜底。住宅代理恢复并连续检查通过后，认证文件自动恢复原内容。

模拟出口不匹配测试已通过：Relay 端口在故障期间仍保持监听，故障账号被禁用；恢复基线后账号自动恢复，禁用备份被清理。

同一 OAuth 认证文件不能让服务器和本地两个进程同时刷新。迁移时先从服务器取得最新认证状态，再将账号文件移出服务器活动认证目录，最后把最新文件复制到本地并启用；否则刷新令牌轮换可能导致其中一侧出现 `refresh token unauthorized`。
## 排障顺序

1. 先检查 `runtime/local-claude-test/guard-failure.txt`。
2. 检查 `runtime/local-claude-test/relay.err.log`。
3. 检查 `runtime/headroom-test/headroom.err.log`。
4. 访问 `http://127.0.0.1:8318/` 和 `http://127.0.0.1:8788/health`。
5. 确认账号 JSON 的 `disabled` 为 `false`，且 `proxy_url` 没有变化。
6. 确认账号没有处于五小时额度或周额度冷却。

不要把 `auths/`、`config.local.yaml`、`runtime/` 日志或任何包含 Token 的备份文件提交到 Git。



