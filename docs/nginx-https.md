# Public IP HTTPS with Nginx

[中文部署步骤](nginx-https_CN.md)

This installer targets a dedicated Ubuntu x86_64 host running Relay and systemd.
It uses Let's Encrypt TLS-ALPN-01 to issue a trusted certificate for a stable
public IPv4 address. Public TCP `443` must reach the server; port `80` is not
required. Nginx and its stream module share `443` between HTTPS traffic and ACME
validation, and the local ports `18443`–`18445` must be free on first install.
Do not run this installer over an unrelated Nginx site already using `443`.

From the repository root on the server, replace `PUBLIC_IPV4` with the actual
address and set the Relay port if it differs from `8317`:

```bash
sudo apt-get update
sudo bash deploy/nginx-https/install.sh PUBLIC_IPV4 8317
```

Check `https://PUBLIC_IPV4/healthz` from another network without disabling
certificate verification. Claude Code uses `https://PUBLIC_IPV4`; OpenAI-compatible
clients use `https://PUBLIC_IPV4/v1`. Keep existing business keys. After clients
migrate, close public access to the old HTTP port in the cloud firewall or bind
Relay to loopback. The installer does not change firewall rules or Relay config.

The `relay-acme-renew.timer` systemd timer checks every six hours and reloads
Nginx after successful renewal. Check it with `systemctl list-timers relay-acme-renew.timer`
and `sudo journalctl -u relay-acme-renew.service`. Certificate and account keys
are stored privately under `/var/lib/relay-acme` and must stay off Git.
