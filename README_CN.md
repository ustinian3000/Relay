# Relay

**让请求稳定路由，让上下文有序交接。**

[English](README.md) · [交接配置](docs/handoff_CN.md) · [网页助手](web/README_CN.md) · [验证范围](docs/validation_CN.md)

Relay 基于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 修改，围绕账号池隔离、会话黏性、额度查询和压缩后交接，补齐多账号网关的状态管理。

本仓库包含最新交接代码和“响应末尾用量覆盖触发值”的修复，是源码仓库，不是此前的便携安装包。交接默认关闭，源码迁入不代表生产服务已经更新。

## 已有功能

| 功能 | 实现 |
| --- | --- |
| 统一接口 | 保留上游模型接入及协议转换能力 |
| SK 隔离账号池 | 每个 Key 只可调度、重试和查询明确授权的账号 |
| 会话黏性 | 同一会话固定账号，子会话按配置继承 |
| 独立出口 | 保留每个凭据自己的代理配置 |
| 额度查询 | 查询当前 SK 池内账号的五小时、周等额度 |
| 压缩后交接 | 持久化状态，收到压缩通知并核对后续请求才试运行 B |
| 失败保留绑定 | B 完整成功才提交换绑，不在同一次失败请求里继续轮换 |
| 客户端 Hook | 提醒、压缩开始/完成通知，以及失败通知补发 |
| 本地网页助手 | 账号/SK 规划、额度展示、SSH 读取与授权向导 |

**通用网关支持多个模型，不等于交接链路都已验收。** 当前交接验证的是 Claude Code 2.1.309、claude-sonnet-4-6、Messages HTTP SSE，尚未验收 Codex CLI 交接。代码和配置使用真实协议名称。

## 目录

```text
cmd/server/                    内核入口
internal/handoff/              持久化交接、摘要证据、流式网关
internal/api/handoff.go        认证与交接接入
sdk/cliproxy/auth/             授权池、调度、额度查询
extensions/key-account-bind/   隔离插件，独立 Go 模块
client/                       客户端 Hook 与示例
web/                          账号管理台与授权向导
tools/testing/                真实客户端 + 模拟上游测试入口
docs/                         使用说明与验证范围
licenses/                     上游许可证
```

Go 模块仍使用上游路径，避免改名导致大量导入变化；仓库名与模块名不同是有意保留的。

## Antigravity 可选配置

如需使用 Antigravity 登录或刷新 Token，请在服务端环境变量或本地 `.env` 中设置
`ANTIGRAVITY_OAUTH_CLIENT_ID` 和 `ANTIGRAVITY_OAUTH_CLIENT_SECRET`，然后启动服务。
使用你有权使用的 OAuth 客户端，并配置所需权限和回调地址，参见 [.env.example](.env.example)。
源码不再内置此类凭据；未配置时，该提供商的登录和刷新会明确报错，其他提供商与交接功能不受影响。
已有 Antigravity 刷新 Token 必须与所配置的 OAuth 客户端匹配，否则需要重新授权。

## 本地启动

需要 Go 1.26.10 或更新版本，以及支持 CGO 的 C 编译器。Hook 和网页助手需要 Python 3.11+。以下命令从仓库根目录执行。

### 1. 编译

Linux：

```bash
mkdir -p bin plugins/linux/amd64 auths
CGO_ENABLED=1 go build -o bin/relay ./cmd/server
(
  cd extensions/key-account-bind
  CGO_ENABLED=1 go build -buildmode=c-shared \
    -o ../../plugins/linux/amd64/key-account-bind.so .
)
```

Windows PowerShell：

```powershell
New-Item -ItemType Directory -Force bin, plugins/windows/amd64, auths
$env:CGO_ENABLED = "1"
go build -o bin/relay.exe ./cmd/server
Push-Location extensions/key-account-bind
go build -buildmode=c-shared -o ../../plugins/windows/amd64/key-account-bind.dll .
Pop-Location
```

插件与内核必须使用相同系统和架构。Windows 需要兼容的 C 编译器，不能直接使用 Linux .so。

原 Docker 脚本仍包含上游镜像默认值，本仓库尚未发布 Relay 镜像；请先按上述源码方式构建。

### 2. 配置

复制 `config.relay.example.yaml` 为 `config.yaml`，然后：

1. 在插件 bindings 中填写自己的随机业务 Key。
2. 将 allow 文件名替换为真实的授权账号 ID。
3. 将对应认证文件放入 `auths/`。
4. 保持 `access.api-keys: []`，业务认证交给隔离插件。

仓库不包含真实账号、Token、住宅代理密码和 SSH 私钥。Relay 示例仅监听本机，管理页面默认关闭；上游完整 `config.example.yaml` 的默认值不同。

### 3. 运行

Linux：

```bash
./bin/relay --config config.yaml --local-model
```

Windows：

```powershell
.\bin\relay.exe --config config.yaml --local-model
```

示例服务地址为 `http://127.0.0.1:8317`，业务接口位于 `/v1`。

多人通过公网使用时，参见 [Nginx + 公网 IP HTTPS](docs/nginx-https_CN.md)。可以不用域名，证书会自动续期。

### 4. 按需开启交接

先读[交接配置](docs/handoff_CN.md)，再填写 caller scope 并安装 Hook。添加第二个账号不会自动启用交接，不同 SK 也不会互相借用账号。

默认账号额度阈值为：80% 等待、90% 主动触发、97% Hook 拦截普通提问。它们不是上下文占用阈值。

## 网页助手

在两个终端分别运行：

```powershell
python -m pip install -r web/account-manager/requirements.txt
python web/account-manager/app.py --core bin/relay.exe --port 18766
python web/claude-login/app.py --port 18765
```

Linux 使用 `bin/relay`。网页助手只监听本机，已有相同端口的助手时请更换端口。

支持私钥路径、PEM 文件上传/拖拽及 SSH 密码。账号管理台主要做本地规划和只读读取服务器；授权向导最后确认可能写入服务器凭据和目标账号池。远端辅助脚本仍沿用现有部署布局，见[网页助手说明](web/README_CN.md)。

## 验证

```bash
go test ./internal/handoff
go test ./internal/api -run 'TestHandoff(Core|Routes)'
go test ./sdk/cliproxy/auth -run 'Test(ScopedPluginAffinity|AccountUsage)'
go test ./internal/config -run TestHandoffConfig
go test ./...
python -m unittest discover -s client -p "test_*.py"
python -m unittest discover -s web/account-manager -p "test_*.py"
python -m unittest discover -s web/claude-login -p "test_local_app.py"
python -m unittest discover -s web/claude-login -p "test_ssh_transport.py"
```

开发测试依赖见 requirements-dev.txt。远端辅助脚本测试使用 Linux 的 fcntl；在 Linux 安装这些依赖后，再运行 `python -m unittest discover -s web/claude-login -p "test_*.py"`，即可包含该组测试。

2026-11-15 已通过独立实例的真实双账号短文本验证：手动压缩交接、自动压缩交接、信息接续、继续对话不重复交接。真实账号、请求日志和服务器操作脚本不纳入仓库。[验证边界与模拟复现](docs/validation_CN.md)。本地 Claude + Headroom 测试记录见[本地测试记录](docs/local-claude-headroom-test_CN.md)，Claude Desktop 3P 请求与额度验证见[Claude Desktop 3P 验证记录](docs/claude-desktop-3p-cache-validation_CN.md)。

## 当前边界

- 交接仍是实验功能，不能把短文本验收扩展为所有客户端、上下文格式均可用。
- 启用交接的 SK 只允许已验证的 Messages SSE 推理路径。
- 带签名 thinking 等不兼容状态会阻止跨账号发送。
- 旧子会话可能需要自己的压缩证据或重建，不保证全部透明续跑。
- 当前响应网关有 90 秒总期限，B 首条缓冲上限 1 MiB。
- 用量改写可能使客户端累计 token 和费用显示失真，真实额度以独立查询为准。
- 数据库及短期回复重放未加密，应使用私有目录且不提交到 Git。
- 修改交接启用范围或数据库路径后需要重启。

技术可用、出口固定或测试通过不代表获得服务商授权，也不能保证账号不受限制。请按实际服务协议接入和使用。

## 来源与许可证

保留现有仓库的 [Apache-2.0 LICENSE](LICENSE)。Relay 新增部分按此提供；继承的 CLIProxyAPI 源码保留 [MIT 声明](licenses/CLIProxyAPI-MIT.txt)，隔离插件保留[自身 MIT 许可证](extensions/key-account-bind/LICENSE)，详见 [NOTICE](NOTICE)。

上游原说明保留为 [English](README_UPSTREAM.md)、[中文](README_UPSTREAM_CN.md)、[日本語](README_UPSTREAM_JA.md)。其发布版本和功能宣传不代表 Relay 已完成相同验证。

