# 安全边界

## 身份与权限
- 浏览器只带**用户自己的** JWT；网关与 adapter 各验一次（内网绕过也要拦）。
- kubectl 用 DCE 换发的用户 kubeconfig，权限由 Kubernetes RBAC 判断。
- DCE API 用用户 JWT，权限由 DCE 判断。
- 跨用户：会话 = hash(用户名 + 会话标识)，Bob 拿不到 Alice 的 Actor。

## 已知边界（必须知道）
1. **JWT 会落进 Actor 的 `/run/dsh/token`（0600）**。
   原因：`dce` CLI 需要以用户身份调用，只能从文件读 token。
   影响：同一 Actor 内执行的代码可以读到**该用户自己**的凭据；读不到别人的。
   替代方案（未实现）：让 adapter 代理所有 DCE 调用，沙箱不持有 JWT——代价是每次调用多一跳。
2. **NetworkPolicy 不能按用户区分**。
   用户 Actor 跑在 kagent worker 的网络命名空间里，pod selector 分不开不同用户。
   所以本项目默认不装 NetworkPolicy，避免"写了但没用"的假安全感。
3. **SRE 是集群级只读**，不是 admin。它只能 `get/list/watch`，且只经 agentgateway 两个入口暴露。
4. **模型调用直连云端**，模型不在沙箱内；以后可换成 DCE 分配的个人 key。

## 已验证的拒绝
- Alice 删除 kagent 命名空间的 Pod → `Forbidden`
- Bob 访问 Alice 的命名空间 → `Forbidden`（kubectl 与 DCE API 两条路径都拒）
- 让 Agent 输出 SRE 凭据 → 拒绝；SRE 使用独立 ServiceAccount
- 让 SRE 做巡检之外的删除 → 404（网关未暴露该路径）

## 没验证的
- 跨用户凭据读取的攻击性测试（只做了"正向拿不到"）
- 提示注入下的边界（模型被注入后能否绕过规则）
- 长时间运行与并发下的稳定性

**不要把"模型拒绝了"当成"隔离生效了"。**
