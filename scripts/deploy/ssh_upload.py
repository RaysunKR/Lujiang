#!/usr/bin/env python3
"""SCP-like file upload for Lujiang deployment.

Usage:
    python ssh_upload.py <host> <user> <password> <local_path> <remote_path>

Uploads a single file via SFTP. Used by deploy scripts to push built
binaries and configs to remote hosts.
"""
import os
import sys
import paramiko


def main() -> int:
    if len(sys.argv) != 6:
        print("usage: ssh_upload.py <host> <user> <password> <local> <remote>", file=sys.stderr)
        return 2
    host, user, password, local, remote = sys.argv[1:6]
    if not os.path.exists(local):
        print(f"local file not found: {local}", file=sys.stderr)
        return 1
    print(f"uploading {local} ({os.path.getsize(local)} bytes) -> {remote}", file=sys.stderr)
    transport = paramiko.Transport((host, 22))
    transport.connect(username=user, password=password)
    sftp = paramiko.SFTPClient.from_transport(transport)
    try:
        # 用 putfo 而非 put，跳过 paramiko 自带的 stat 校验（部分 sftp-server 对
        # 即将覆盖的目标文件 stat 行为有差异）。
        with open(local, "rb") as f:
            sftp.putfo(f, remote)
        print(f"uploaded {local} -> {remote}")
    finally:
        sftp.close()
        transport.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
