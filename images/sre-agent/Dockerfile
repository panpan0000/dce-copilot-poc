# 独立 SRE Agent：DSH + 只做巡检的适配器（不是用户 Actor 的一部分）
ARG DSH_BASE_IMAGE
FROM ${DSH_BASE_IMAGE}
USER root
COPY sre-agent.js cluster-inspect.js /app/
COPY start.sh /opt/sre/start.sh
RUN chmod 0755 /opt/sre/start.sh && chmod 0644 /app/*.js && chown -R 10001:10001 /app
USER 10001
EXPOSE 8080
ENTRYPOINT ["/bin/sh", "/opt/sre/start.sh"]
