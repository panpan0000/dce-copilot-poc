# Actor 运行时：DSH + A2A shim + DCE/SRE CLI + skills
# 基础镜像是 DSH 运行时（自行提供，见 docs/replicate.md），构建上下文里额外有：
#   dce-bin（构建脚本按架构下载）、skills/（构建脚本按 tag 克隆）
ARG DSH_BASE_IMAGE
FROM ${DSH_BASE_IMAGE}
USER root
COPY start.sh   /opt/dsh-a2a/start.sh
COPY a2a-shim.js /opt/dsh-a2a/a2a-shim.js
COPY sre-cli.js  /opt/dsh-a2a/sre-cli.js
COPY sre-cli      /usr/local/bin/sre-cli
COPY dce-cli.js  /opt/dsh-a2a/dce-cli.js
COPY dce-bin     /usr/local/bin/dce-bin
COPY dce         /usr/local/bin/dce
COPY skills      /home/agent/.dsh/skills
RUN chmod 0755 /opt/dsh-a2a/start.sh /usr/local/bin/dce /usr/local/bin/dce-bin /usr/local/bin/sre-cli \
 && chmod 0644 /opt/dsh-a2a/a2a-shim.js /opt/dsh-a2a/sre-cli.js /opt/dsh-a2a/dce-cli.js \
 && chown -R 10001:10001 /home/agent/.dsh/skills \
 && chmod -R a+rX /home/agent/.dsh/skills
USER 10001
EXPOSE 80
ENTRYPOINT ["/bin/sh", "/opt/dsh-a2a/start.sh"]

# egress 网关 CA：沙箱访问 api.deepseek.com 时走 atenet-router，它用自己的 CA 终止 TLS
COPY egress-ca.pem /opt/dsh-a2a/egress-ca.pem
