FROM release-ci.daocloud.io/common-ci/dce-copilot-dsh-a2a:0.4.1-ca
USER 0
COPY kagent-dsh /kagent-dsh
COPY start.sh /start.sh
RUN chmod 0755 /kagent-dsh /start.sh
ENTRYPOINT ["/bin/sh", "/start.sh"]
