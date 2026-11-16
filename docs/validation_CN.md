# 验证范围

[English](validation.md)

## 已有结果

2026-11-15 在独立测试实例完成真实双账号短文本验证：

- 手动 /compact 后交接成功。
- 中转触发客户端原生自动压缩后交接成功。
- B 正确复述 A 收到的四项测试信息。
- 后续一轮仍绑定 B，代次保持 1，没有重复交接。

组合为 Claude Code 2.1.309、claude-sonnet-4-6、Messages HTTP SSE；只验证了单方向、主会话、短文本，真实回复的 thinking token 为 0。测试没有替换生产内核，结束后删除了临时 SK、认证副本和状态。

真实流暴露了末尾 message_delta.usage.input_tokens 覆盖开头触发值的问题。先用模拟上游和原生客户端复现，再让同一触发响应内的用量更新保持一致下限，真实自动交接随后通过。

修复后的本地回归为 24 个通过事件（含子测试），0 失败，启用了竞态检测。此前模拟还覆盖持久化、隔离、并发、迟到旧请求、通知补发、取消、半截流、候选撤销及重试上限。

原始真实账号、服务器信息、SSH 操作脚本和原始对话日志不随仓库发布。

## 本地复现

2026-11-17，Antigravity OAuth 环境变量配置改动已通过内部认证、SDK 认证、管理接口和运行时执行器四个包的针对性测试，以及 Windows 内核编译。模拟网络验证了正常凭据传递，以及缺少配置时在发请求前拒绝操作；本轮未使用真实账号，相关测试已加入 CI。

基础测试见根目录说明，有兼容 C 工具链时可增加 -race。隔离插件应在 extensions/key-account-bind 独立运行 go test ./...。

原生客户端模拟测试为显式启用：

1. 设置 HANDOFF_NATIVE_TEST_SCRIPT，指向本仓库 tools/testing/native_client_probe.py。
2. 设置 HANDOFF_CLIENT_BINARY，指向 Claude Code 2.1.309 可执行文件。
3. 执行：

```bash
go test ./internal/api -run '^TestHandoffNativeClient$' -count=1 -v
```

Go 测试启动回环地址服务和模拟账号，模型回复由模拟执行器提供。探针核对客户端版本并使用临时配置，不需要真实账号。

默认输出目录 test-reports/native-client，可由 HANDOFF_REPORT_DIR 更改；该目录不应提交到 Git。

## 尚未证明

未验证 Codex CLI 交接、其他客户端版本、带签名 thinking 的跨账号传递、真实反向交接、真实长工具链、所有旧子代理透明续跑或平台授权。

真实测试通过显式 prepare/trigger 请求交接，并没有把账号额度耗尽以触发所有自然阈值。成功复述信息证明的是本次条件下的技术兼容，不是账号安全承诺。
