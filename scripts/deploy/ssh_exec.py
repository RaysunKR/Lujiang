#!/usr/bin/env python3
"""SSH command helper for Lujiang deployment.

Usage:
    python ssh_exec.py <host> <user> <password> "<command>"

Runs a shell command on the remote host and streams stdout/stderr.
Exit code propagates. Used by other deploy scripts to keep paramiko
setup in one place.
"""
import sys
import paramiko


def main() -> int:
    if len(sys.argv) != 5:
        print("usage: ssh_exec.py <host> <user> <password> <command>", file=sys.stderr)
        return 2
    host, user, password, command = sys.argv[1:5]
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(
        hostname=host,
        username=user,
        password=password,
        look_for_keys=False,
        allow_agent=False,
        timeout=15,
    )
    stdin, stdout, stderr = client.exec_command(command, get_pty=False, timeout=600)
    exit_code = stdout.channel.recv_exit_status()
    out = stdout.read().decode("utf-8", errors="replace")
    err = stderr.read().decode("utf-8", errors="replace")
    if out:
        sys.stdout.write(out)
    if err:
        sys.stderr.write(err)
    client.close()
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
