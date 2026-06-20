# Git Bash / MSYS 会把 /opt/... 当 Windows 路径转换；部署时关掉。
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'
