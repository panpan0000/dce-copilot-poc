# kagent controller + Actor 唤醒竞态补丁（见 kagent-resume-race.patch）
# 构建方式：先按 ../build-controller.sh 产出 codex-kagent-controller-fixed，
# 放到本目录再 build（补丁需要 Go 1.26.3 与官方镜像一致的 ldflags）。
FROM ghcr.io/kagent-dev/kagent/controller:0.10.0-beta6
COPY --chown=65532:65532 codex-kagent-controller-fixed /app
