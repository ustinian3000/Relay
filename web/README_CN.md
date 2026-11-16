# 本地网页助手

[English](README.md)

两套网页程序共享 runtime_contract.py 和 ssh_keys.py，迁移时请保留 web 目录结构。

在仓库根目录运行：

```powershell
python -m pip install -r web/account-manager/requirements.txt
python web/account-manager/app.py --core bin/relay.exe --port 18766
```

另开终端：

```powershell
python web/claude-login/app.py --port 18765
```

Linux 内核路径改为 bin/relay。添加 --no-open 可以不自动打开浏览器；原助手占用端口时可改 --port，不必关闭原程序。

## 两个页面的区别

- 账号/SK 管理台：本地规划、配置导出、额度查询，以及通过 SSH 只读同步服务器快照。
- 授权向导：独立的登录流程，最后确认导入时可能写入服务器认证文件及所选账号池。

支持 SSH 私钥路径、PEM 内容上传/拖拽、SSH 密码。主机密钥核验独立于账号认证，不能因为在其他 SSH 软件里确认过就假定这里也已信任。

## 目录与依赖

- 本地默认数据目录为 web/account-manager/.local-data，可以用 --data-dir 指定其他私有目录。
- 管理台继承的默认内核路径指向便携版 runtime，源码使用时应显式传 --core。
- 远端脚本目前按 SSH 用户主目录下 tools/cliproxyapi/config.yaml 查找生产配置，并依赖 v8 布局及对应内核/插件接口。
- 远端 Python 需要 PyYAML；本机 SSH 密码/上传私钥功能需要 requirements.txt 中的 Paramiko。
- 不是任意服务器目录都能直接使用的通用部署界面，也不是可以直接开放公网的多用户管理平台。

仓库不包含以前的连接记录、账号快照、上传密钥和本地状态备份。
