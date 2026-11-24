// 以当前用户身份调 DCE OpenAPI。JWT 不在沙箱里，只在 shim 内存中；这里只跟本地代理说话。
const [method, path, body] = process.argv.slice(2);
if (!['GET', 'POST', 'PUT', 'PATCH', 'DELETE'].includes(method) || !/^\/(apis|kapis)\//.test(path || '')) {
  console.error('用法: node /opt/dsh-a2a/dce-cli.js GET /apis/ghippo.io/v1alpha1/current-user');
  process.exit(2);
}
(async () => {
  const r = await fetch('http://127.0.0.1:8099/dce' + path, {
    method,
    headers: body ? { 'content-type': 'application/json' } : {},
    body,
  });
  const text = await r.text();
  console.log('DCE HTTP ' + r.status);
  console.log(text.length > 4000 ? text.slice(0, 4000) + '\n…（截断）' : text);
  if (!r.ok) process.exitCode = 1;
})().catch((e) => { console.error('调用失败: ' + e.message); process.exitCode = 1; });
