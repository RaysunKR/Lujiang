#!/usr/bin/env bash
# 在 VM 上跑的 setup 脚本：建用户、目录、装 service。
# 通过 ssh_exec.py 远程执行，参数从 stdin 喂 sudo 密码。
set -e

SUDO_PWD="$1"

echo "$SUDO_PWD" | sudo -S -p "" bash -c '
    id lujiang >/dev/null 2>&1 || useradd -r -m -d /var/lib/lujiang -s /usr/sbin/nologin lujiang
    mkdir -p /opt/lujiang /var/lib/lujiang
    chown -R lujiang:lujiang /var/lib/lujiang
    chmod 755 /opt/lujiang
'
echo "setup ok"
ls -la /opt/lujiang
