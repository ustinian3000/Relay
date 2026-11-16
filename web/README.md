# Local web tools

[中文](README_CN.md)

Both applications run locally and share `runtime_contract.py` and `ssh_keys.py`. Keep the directory structure intact.

```bash
python -m pip install -r web/account-manager/requirements.txt
python web/account-manager/app.py --core bin/relay.exe --port 18766
python web/claude-login/app.py --port 18765
```

Use `bin/relay` on Linux. Add `--no-open` to suppress automatic browser opening. An existing installation may already occupy these ports; choose other ports rather than terminating it.

- Account manager: local account/SK planning and exports, quota views, and read-only production snapshots.
- Enrollment wizard: a separate authorization workflow. Its final confirmed import can update server credential files and the selected account pool.
- Authentication: SSH private-key path, uploaded PEM content, or SSH password. Server host-key verification is separate from the login method.
- The manager defaults to `web/account-manager/.local-data`. Use `--data-dir` to select a private directory. These files are excluded from Git.
- The manager's inherited default core path expects a portable `runtime/` folder. Pass `--core` explicitly when using this source checkout.

The SSH helpers currently assume the existing remote layout: `tools/cliproxyapi/config.yaml` under the SSH user's home directory, a v8 configuration, and the matching kernel/plugin interfaces. The remote Python environment needs PyYAML. This is not yet a generic arbitrary-directory deployment UI.

These are development web tools, not a public multi-user management service. Real credentials, connection state, and previously uploaded keys are not distributed in this repository.
