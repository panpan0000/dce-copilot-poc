SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

help: ## 列出所有目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

config: ## 生成 .env（若不存在）
	@test -f .env || (cp config.env.example .env && echo "已生成 .env，请修改后再继续")

check: ## 校验配置项与依赖命令
	@scripts/check.sh

build: ## 构建 4 个镜像（dsh-a2a / sre-agent / adapter / controller-fix）
	@scripts/build.sh

bootstrap: ## 安装基础组件（Gateway API + kgateway + substrate + kagent）
	@scripts/bootstrap.sh

deploy: ## 部署应用侧资源（SandboxAgent/WorkerPool/网关/adapter/SRE）
	@scripts/deploy.sh

extension: ## 生成扩展的 config.js 并打包
	@scripts/extension.sh

verify: ## 跑一遍端到端用例，输出 pass/fail
	@scripts/verify.sh

netpol-test: ## 验证 NetworkPolicy 真的生效（不通过则说明 CNI 不执行）
	@scripts/netpol-test.sh

clean: ## 清理构建产物
	@rm -rf build dist
