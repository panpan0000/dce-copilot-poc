# 从零构建 DSH 运行时（替代"外部提供的 DSH 基础镜像"），任意架构可构建。
# 之前依赖一个预构建的 watsons-dsh-poc:v3，只有 arm64；这个 Dockerfile 让 x86 也能出。
FROM node:24-bookworm-slim
ARG DSH_VERSION=0.1.1-rc.2
ARG KUBECTL_VERSION=v1.32.9

ENV DEBIAN_FRONTEND=noninteractive HOME=/home/agent
# npm 镜像：默认用国内源，国内构建快很多；需要官方源时 --build-arg NPM_REGISTRY=https://registry.npmjs.org
ARG NPM_REGISTRY=https://registry.npmmirror.com
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git python3 python3-venv jq tini \
 && rm -rf /var/lib/apt/lists/* \
 && curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/$(dpkg --print-architecture)/kubectl" \
      -o /usr/local/bin/kubectl && chmod 0755 /usr/local/bin/kubectl \
 && npm config set registry "${NPM_REGISTRY}" \
 && npm install -g "@deepseek-ai/dsh@${DSH_VERSION}" \
 && npm config delete registry \
 && npm cache clean --force

# 与旧镜像保持同样的用户与目录，Actor 里的脚本不用改
RUN useradd -m -u 10001 -s /bin/bash agent \
 && mkdir -p /home/agent/work /home/agent/.dsh && chown -R 10001:10001 /home/agent

USER 10001
WORKDIR /home/agent/work
CMD ["dsh", "--help"]
