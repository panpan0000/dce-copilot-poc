'use strict';
// Fixed read-only collector called by the SRE DSH's bash tool; token is never printed.
const fs=require('fs'),https=require('https');
const root='/var/run/secrets/kubernetes.io/serviceaccount/';
const token=fs.readFileSync(root+'token','utf8').trim(),ca=fs.readFileSync(root+'ca.crt');
function get(path){return new Promise((resolve,reject)=>{https.get({hostname:'kubernetes.default.svc',port:443,path,ca,headers:{authorization:'Bearer '+token},timeout:15000},r=>{let raw='';r.on('data',c=>raw+=c);r.on('end',()=>{if(r.statusCode!==200)return reject(Error('Kubernetes HTTP '+r.statusCode+' '+path));try{resolve(JSON.parse(raw))}catch(e){reject(e)}})}).on('error',reject)})}
(async()=>{
 const [n,p,d]=await Promise.all([get('/api/v1/nodes'),get('/api/v1/pods'),get('/apis/apps/v1/deployments')]);
 const out={nodes:n.items.map(x=>({name:x.metadata.name,ready:(x.status.conditions||[]).find(c=>c.type==='Ready')?.status})),pod_total:p.items.length,pods:p.items.slice(0,60).map(x=>({namespace:x.metadata.namespace,name:x.metadata.name,phase:x.status?.phase,ready:(x.status?.containerStatuses||[]).every(c=>c.ready)})),deployment_total:d.items.length,deployments:d.items.slice(0,60).map(x=>({namespace:x.metadata.namespace,name:x.metadata.name,desired:x.spec?.replicas,ready:x.status?.readyReplicas||0})),truncated:p.items.length>60||d.items.length>60};
 console.log(JSON.stringify(out,null,2));
})().catch(e=>{console.error('Kubernetes collection error:',e.message);process.exitCode=1});
