const $ = (id) => document.getElementById(id);
chrome.storage.local.get(['dceOrigin', 'gatewayBase']).then((c) => {
  $('dce').value = c.dceOrigin || '';
  $('gw').value = c.gatewayBase || '';
});
$('save').addEventListener('click', async () => {
  await chrome.storage.local.set({
    dceOrigin: $('dce').value.trim().replace(/\/$/, ''),
    gatewayBase: $('gw').value.trim().replace(/\/$/, ''),
  });
  $('msg').textContent = '已保存，刷新 DCE 页面生效';
});
