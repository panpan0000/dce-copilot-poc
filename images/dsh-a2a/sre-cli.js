// 只做两件事：查看最新巡检报告、触发一次固定巡检。没有任意转发/执行能力。
const cmd = process.argv[2];
if (cmd !== 'report' && cmd !== 'inspect') {
  console.error('用法: node /opt/dsh-a2a/sre-cli.js report|inspect');
  process.exit(2);
}
(async () => {
  const r = await fetch('http://127.0.0.1:8099/sre/' + cmd, { method: cmd === 'report' ? 'GET' : 'POST' });
  const body = await r.text();
  console.log('HTTP ' + r.status);
  if (r.status === 404) console.log('该操作不在 SRE 的授权范围内（只有巡检与查看报告两个接口）。');
  else console.log(body);
})().catch((e) => { console.error('调用失败: ' + e.message); process.exitCode = 1; });
