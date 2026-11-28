#!/bin/sh
# 立刻起 A2A 适配进程（:8081/:80 必须先于探针），DSH 并行启动
export HOME=/home/agent
export DSH_HOME=/home/agent/.dsh
dsh --profile web --no-open --host 127.0.0.1 --port 18790 >/tmp/dsh.log 2>&1 &
exec /kagent-dsh
