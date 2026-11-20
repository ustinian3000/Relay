# Claude Desktop 3P 请求与额度验证记录

## 验证时间

2026-11-20，日本时间。

## 验证目的

确认 Claude Desktop 通过本地 Relay 接入时，是否被识别为 `claude-desktop-3p`，以及请求是否携带 Anthropic 缓存字段。

## 本地链路

```text
Claude Desktop 第三方推理模式
    │
    ▼
Relay 127.0.0.1:8318
    │
    ▼
Claude OAuth 账号专属代理
```

详细请求记录只在本次短时验证期间开启，验证结束后已关闭。仓库不记录账号 Token、SK、代理认证信息或对话正文。

## 请求入口判定

请求记录显示了以下客户端特征：

```text
User-Agent: claude-cli/2.1.326 (external, claude-desktop-3p, agent-sdk/0.3.325)
X-App: cli
Anthropic-Beta: claude-code-20250219,...
```

本地配置的 `upstream.claude.header-defaults.native-entrypoints` 包含 `claude-desktop-3p`，因此 Relay 会把满足强信号的该入口按允许的原生客户端入口处理。

## 缓存字段结果

这次短消息测试在约两秒内产生了两次请求。上游响应中的缓存统计如下：

| 请求 | 新输入 | 新建缓存 | 读取缓存 |
| --- | ---: | ---: | ---: |
| 第一次 | 504 | 28 | 92,536 |
| 第二次 | 26 | 957 | 91,579 |

结果表明请求已经携带并命中了缓存。一次短消息也可能触发多次请求，Desktop 的系统上下文和工作区上下文会使缓存读取量明显变大。

## 额度观察

测试前记录的五小时额度使用率为 39%，测试后查询为 77%。之后确认这段期间账号还在 Claude 网页端使用过，第一次额度跳升包含网页端消耗，因此不能把这 38 个百分点归因于 Claude Desktop 3P。周额度使用率查询为 84%。

结合 CLI 的对照测试，当前没有证据表明 Claude Desktop 3P 的额度消耗明显高于 CLI。缓存命中字段也证明这次请求走过缓存路径。后续比较需要在同一账号、同一时间窗口内隔离网页端、CLI 和 Desktop 请求。

## 当前结论

- Claude Desktop 请求已经被识别为 `claude-desktop-3p`。
- Relay 没有把该请求当作普通未知客户端处理。
- 上游响应明确返回了大量 `cache_read_input_tokens`。
- 本次额度变化受到网页端使用干扰，不能用作 Desktop 与 CLI 的差异证据。
- 当前对照结果未发现 Desktop 3P 比 CLI 消耗更多。
- 详细请求记录已关闭，后续默认不记录对话正文。

## 配置位置

本地测试配置位于被 Git 忽略的 `config.local.yaml`，相关配置形态如下：

```yaml
upstream:
  claude:
    header-defaults:
      native-entrypoints:
        - claude-desktop
        - claude-desktop-3p
```

该配置只影响本地 Relay 的客户端入口判定，不包含账号凭据。
