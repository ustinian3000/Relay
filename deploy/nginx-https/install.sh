#!/usr/bin/env bash
set -euo pipefail
umask 077

die() { printf '%s\n' "$*" >&2; exit 1; }

if [[ $# -lt 1 || $# -gt 2 ]]; then
    die "Usage: sudo bash $0 PUBLIC_IPV4 [RELAY_PORT]"
fi

public_ip=$1
relay_port=${2:-8317}
[[ $EUID -eq 0 ]] || die 'Run this installer with sudo.'
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || die 'Ubuntu on Linux x86_64 is required.'
. /etc/os-release
[[ ${ID:-} == ubuntu ]] || die 'This installer expects Ubuntu with systemd.'
[[ $relay_port =~ ^[1-9][0-9]{0,4}$ ]] && (( relay_port <= 65535 )) || die 'Invalid Relay port.'
case "$relay_port" in 80|443|18443|18444|18445) die 'Choose a Relay port other than 80, 443, and 18443–18445.' ;; esac

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
stream_site=/etc/nginx/modules-enabled/99-relay-stream.conf
https_site=/etc/nginx/conf.d/relay-https.conf
default_site=/etc/nginx/sites-enabled/default
systemd_dir=/etc/systemd/system
acme_dir=/var/lib/relay-acme
lego=/usr/local/bin/relay-lego
reload_hook=/usr/local/sbin/relay-nginx-cert-reload
lego_version=5.6.0
lego_archive="lego_v${lego_version}_linux_amd64.tar.gz"
lego_sha256=f02fc11b6f56b97e387fa7caa0268f86f5a336953b22458e6430e4651fea439a

nginx_was_installed=false
if [[ -x /usr/sbin/nginx ]]; then nginx_was_installed=true; fi
if ! command -v python3 >/dev/null; then die 'Install python3 before running the installer.'; fi
python3 - "$public_ip" <<'PY' || die 'Pass the public IPv4 address of this server.'
import ipaddress
import sys

try:
    address = ipaddress.IPv4Address(sys.argv[1])
except ValueError:
    sys.exit(1)
sys.exit(not (address.is_global and not address.is_multicast and not address.is_reserved))
PY

workdir=$(mktemp -d /tmp/relay-nginx-https.XXXXXXXX)
stream_added=false
site_added=false
default_moved=false
finish() {
    local result=$?
    trap - EXIT
    if (( result != 0 )); then
        if "$site_added"; then rm -f -- "$https_site"; fi
        if "$stream_added"; then rm -f -- "$stream_site"; fi
        if "$default_moved"; then mv -- "$workdir/nginx-default" "$default_site"; fi
        if "$site_added" || "$stream_added" || "$default_moved"; then
            /usr/sbin/nginx -t && systemctl reload nginx.service || true
        fi
    fi
    rm -rf -- "$workdir"
    exit "$result"
}
trap finish EXIT

for template in relay-stream.conf relay-https.conf relay-acme-renew.service relay-acme-renew.timer reload-nginx.sh; do
    sed -e "s/@PUBLIC_IP@/$public_ip/g" -e "s/@RELAY_PORT@/$relay_port/g" \
        "$script_dir/templates/$template" > "$workdir/$template"
done

check_target() {
    local source=$1 target=$2
    if [[ -L $target ]] || { [[ -e $target ]] && ! cmp -s -- "$source" "$target"; }; then
        die "Existing $target differs; review it before continuing."
    fi
}

check_target "$workdir/relay-stream.conf" "$stream_site"
check_target "$workdir/relay-https.conf" "$https_site"
check_target "$workdir/relay-acme-renew.service" "$systemd_dir/relay-acme-renew.service"
check_target "$workdir/relay-acme-renew.timer" "$systemd_dir/relay-acme-renew.timer"
check_target "$workdir/reload-nginx.sh" "$reload_hook"

DEBIAN_FRONTEND=noninteractive apt-get install -y nginx libnginx-mod-stream curl ca-certificates openssl iproute2
curl --noproxy '*' -fsS --max-time 5 "http://127.0.0.1:$relay_port/healthz" >/dev/null || die 'Relay is not healthy on loopback.'

if [[ ! -e $stream_site && -n $(ss -H -lnt '( sport = :443 or sport = :18443 or sport = :18444 or sport = :18445 )') ]]; then
    die 'A listener already uses 443 or one of the Relay TLS loopback ports (18443–18445).'
fi

if [[ -e $lego || -L $lego ]]; then
    [[ -f $lego && ! -L $lego && $($lego --version) == "lego version $lego_version linux/amd64" ]] || die "Review the existing $lego before continuing."
else
    curl -fsSL --max-time 90 -o "$workdir/$lego_archive" "https://github.com/go-acme/lego/releases/download/v$lego_version/$lego_archive"
    (cd "$workdir" && printf '%s  %s\n' "$lego_sha256" "$lego_archive" | sha256sum --check --status) || die 'lego archive checksum mismatch.'
    tar -xOzf "$workdir/$lego_archive" lego > "$workdir/relay-lego"
    install -m 0755 "$workdir/relay-lego" "$lego"
fi

if ! "$nginx_was_installed" && [[ -L $default_site && $(readlink -f -- "$default_site") == /etc/nginx/sites-available/default ]]; then
    mv -- "$default_site" "$workdir/nginx-default"
    default_moved=true
fi
if [[ ! -e $stream_site ]]; then
    install -m 0644 "$workdir/relay-stream.conf" "$stream_site"
    stream_added=true
fi
/usr/sbin/nginx -t
systemctl enable --now nginx.service
systemctl reload nginx.service

install -d -m 0700 "$acme_dir"
"$lego" run --path "$acme_dir" --account-id relay-ip --domains "$public_ip" --profile shortlived \
    --ipv4only --tls --tls.address 127.0.0.1:18444 --renew-days 3 --accept-tos
cert_base="$acme_dir/certificates/$public_ip"
openssl verify -verify_ip "$public_ip" -untrusted "$cert_base.issuer.crt" "$cert_base.crt"

if [[ ! -e $https_site ]]; then
    install -m 0644 "$workdir/relay-https.conf" "$https_site"
    site_added=true
fi
/usr/sbin/nginx -t
systemctl reload nginx.service
curl --noproxy '*' -fsS --retry 3 --retry-connrefused --connect-timeout 3 --max-time 6 \
    --connect-to "$public_ip:443:127.0.0.1:443" "https://$public_ip/healthz" >/dev/null

install -m 0755 "$workdir/reload-nginx.sh" "$reload_hook"
install -m 0644 "$workdir/relay-acme-renew.service" "$systemd_dir/relay-acme-renew.service"
install -m 0644 "$workdir/relay-acme-renew.timer" "$systemd_dir/relay-acme-renew.timer"
systemd-analyze verify "$systemd_dir/relay-acme-renew.service" "$systemd_dir/relay-acme-renew.timer"
systemctl daemon-reload
systemctl enable --now relay-acme-renew.timer
printf 'Relay HTTPS: https://%s\n' "$public_ip"
printf 'Verify https://%s/healthz from another network before migrating clients.\n' "$public_ip"
