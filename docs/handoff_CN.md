# 交接配置

[English](handoff.md)

## Design Rationale

交接绑定按“已认证 SK 的 caller scope + 会话族”持久化，独立于上游一小时黏性缓存。隔离插件先确定授权池，中转只在池内选择账号。压缩完成通知和后续摘要采用证据共同构成交接边界，不能只看一个摘要前缀。

旧请求结束后才允许 B 首次试运行；首条回复完整成功后才提交 B。失败保留 A，不自动回放给 A，也不在同一请求中继续试其他账号。

## 前提

- 已编译相同平台的内核和隔离插件。
- 一个业务 Key 至少授权两个账号，不能跨 SK 借用账号。
- B 可用、五小时和周额度均已知、最低剩余至少 20%，并支持本会话登记过的模型。
- 使用已验证的 Claude Code 2.1.309、claude-sonnet-4-6、Messages HTTP SSE。

## 1. 生成 caller scope

它是业务 Key 的版本化哈希，不是邮箱、账号 ID 或客户端随意填写的值：

```bash
python -c 'import getpass,hashlib; key=getpass.getpass("Client SK: ").strip(); print(hashlib.sha256(("cli-proxy-api:caller-scope:v1\0"+key).encode()).hexdigest())'
```

## 2. 修改配置

合并到现有 routing，保留原有其他字段：

```yaml
routing:
  handoff:
    enabled: true
    caller-scopes:
      - "替换为上一步得到的哈希"
    state-file: "./state/handoff.db"
```

重启服务后生效。同一个数据库不能同时供多个内核进程使用。未加入范围的 SK 继续采用原来的黏性/故障切换策略。

## 3. 安装 Hook

在使用客户端的项目目录下，把本仓库的 `client/handoff_hook.py` 复制为 `.claude/hooks/relay_handoff.py`。

将 `client/settings.example.json` 的 hooks 合并进该项目 `.claude/settings.json`，不要覆盖已有设置。从项目根目录启动客户端；示例里的脚本路径相对于该目录。

向客户端进程设置：

| 环境变量 | 内容 |
| --- | --- |
| `HANDOFF_BASE_URL` | 服务根地址，如 `http://127.0.0.1:8317`，不要加 `/v1` |
| `HANDOFF_API_KEY` | 与推理请求一致的业务 SK |
| `HANDOFF_OUTBOX_DIR` | 私有通知重试目录，可选 |

需要 Python 3.11+。系统命令只有 python3 时，修改示例 Hook 命令里的 python。实际运行目录不同，应按客户端支持的项目根目录机制调整脚本路径。

## 默认阈值

| 情况 | 行为 |
| --- | --- |
| 没有合格 B | 留在 A，不放大用量 |
| 额度已用 80% | 待交接，等待原生压缩 |
| 额度已用 90% | 对一条主会话响应放大客户端用量，触发原生压缩 |
| 额度已用 97% | Hook 拦截普通提问，提示先压缩 |
| A 已耗尽或不可用 | 报错，不自动换号 |
| B 完整成功 | 提交换绑，代次加一 |
| B 失败 | 保留 A，候选冷却，等待恢复操作 |

阈值是 `internal/handoff/engine.go` 中的编译默认值，当前 YAML 只暴露启用范围和数据库路径。

单代只选择一条主响应做触发。该响应中，开头及末尾重复上报的 input_tokens 保持至少 180000，避免末尾真实值覆盖触发值。其他响应不持续放大。它会影响客户端累计 token/费用显示，不能据此推算真实计费。

## 接口

接口复用业务 SK 认证，服务端计算 caller scope：

| 接口 | 作用 |
| --- | --- |
| `GET /v1/handoff/status?session_id=...` | 当前绑定、额度、代次、阶段和候选状态 |
| `POST /v1/handoff/pre-compact` | 登记本代压缩开始 |
| `POST /v1/handoff/compact` | PostCompact 提交本代摘要 |
| `POST /v1/handoff/manual` | 手动请求交接或恢复 |

普通通知按需携带 session_id、generation、notice_id、compact_summary；手动接口携带 session_id 和 action。

| action | 含义 |
| --- | --- |
| `prepare` | 主动请求交接，不伪造真实额度 |
| `trigger` | 主动请求交接，并在下一条合适响应触发原生压缩 |
| `retry` | 失败后在次数上限内重试 |
| `cancel` | 取消待交接，不会让耗尽账号恢复额度 |
| `acknowledge-drain` | 确认前一个进程已停止后，处理到期遗留租约；不能清除当前进程持有的租约 |

这些操作不能绕过授权池、候选可用性和摘要核对。

## 存储与限制

- BoltDB 为明文存储，不含账号凭据和完整请求历史，但包含标识、摘要哈希及短期回复重放。
- 首条成功 B 回复最多 1 MiB，逻辑保留五分钟，总重放容量约 64 MiB；清理不等于安全擦除。
- 当前响应网关总期限 90 秒，B 首条缓冲上限 1 MiB，请求体上限 16 MiB，十秒发送一次 ping。
- ping 只能缓解空闲超时，不能延长客户端总期限。
- 旧子会话或不兼容 thinking 状态会被阻止直接跨账号发送。
- 绑定不会因为一小时缓存到期而自动删除。不要通过删除数据库强制换号。
