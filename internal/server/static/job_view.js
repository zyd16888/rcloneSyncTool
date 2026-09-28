(() => {
  const page = document.getElementById('jobPage');
  if (!page) return;
  const id = page.dataset.jobId;
  const element = name => document.getElementById(name);
  const text = (name, value) => { element(name).textContent = value ?? '-'; };
  const statusNames = { pending:'等待执行', blocked:'等待条件恢复', running:'执行中', done:'已完成', failed:'失败', terminated:'已停止' };
  const phaseNames = { preparing:'准备清单', copying:'传输', verifying:'整组校验', publishing:'发布目录', cleanup:'清理源文件', completed:'完成' };
  function bytes(n) { const units=['B','KiB','MiB','GiB','TiB','PiB']; let value=Math.max(0,Number(n)||0),i=0; while(value>=1024&&i<units.length-1){value/=1024;i++;} return (i?value.toFixed(1):value)+' '+units[i]; }
  function time(value) { if(!value || value.startsWith('0001'))return '-'; const d=new Date(value);return Number.isNaN(d.getTime())?'-':d.toLocaleString(); }
  function cell(row,value) { const td=document.createElement('td');td.textContent=value;td.style.wordBreak='break-all';row.appendChild(td);return td; }
  let finished=false, cursor='', refreshing=false;
  async function manifest(append=false) {
    const r=await fetch('/api/job/files?id='+encodeURIComponent(id)+(append?'&after='+encodeURIComponent(cursor):''));
    if(!r.ok)return;const d=await r.json();const body=element('manifestBody');if(!append)body.replaceChildren();
    for(const f of d.files||[]){const row=document.createElement('tr');cell(row,f.Path);cell(row,statusNames[f.State]||({pending:'未完成',done:'已验证完成',failed:'待重试'}[f.State])||f.State);cell(row,bytes(f.Size));cell(row,f.LastError||'');body.appendChild(row);}
    cursor=d.next||'';element('moreFiles').style.display=cursor?'':'none';
  }
  async function transfers() {
    const r=await fetch('/api/job/transfers?id='+encodeURIComponent(id));if(!r.ok)return;const d=await r.json();const body=element('transfersBody');body.replaceChildren();
    const list=d.transfers||[];text('transfers',list.length);text('transfersHint',d.error?'暂时无法获取传输状态：'+d.error:(!d.running?'任务未在传输':'当前没有传输文件，可能正在检查、校验或发布'));
    for(const t of list){const row=document.createElement('tr');cell(row,t.name||'');const progressCell=cell(row,'');const progress=document.createElement('progress');progress.className='progress progress-primary w-40';progress.max=100;progress.value=t.size>0?Math.min(100,Math.max(0,t.bytes*100/t.size)):0;progressCell.appendChild(progress);cell(row,bytes(t.bytes)+' / '+bytes(t.size));cell(row,bytes(t.speed)+'/s');cell(row,t.eta>0?Math.ceil(t.eta)+'s':'-');body.appendChild(row);}
  }
  async function refresh() {
    if(refreshing)return;refreshing=true;
    try {
      const r=await fetch('/api/job?id='+encodeURIComponent(id));if(!r.ok)return;const d=await r.json();const j=d.job;finished=['done','failed','terminated'].includes(j.Status);
      text('jobStatus',statusNames[j.Status]||j.Status);text('jobPhase',phaseNames[j.Phase]||j.Phase||'-');text('jobStarted',time(j.StartedAt));text('jobEnded',time(j.EndedAt));text('jobError',j.Error||'');
      text('bytes',bytes(j.BytesDone));text('speed',finished?bytes(j.AvgSpeed)+'/s 平均':bytes(j.AvgSpeed)+'/s');text('doneFiles',(d.doneCount||0)+' / '+(d.filesTotal||0));text('failedFiles',d.filesFailed||0);text('errors',d.hasMetric?d.metric.Errors:0);
      element('terminateForm').style.display=finished?'none':'';element('retryForm').style.display=['failed','terminated'].includes(j.Status)?'':'none';
      await Promise.all([manifest(),transfers()]);
    } catch (_) { text('transfersHint','状态更新暂时失败，稍后重试'); }
    finally {refreshing=false;if(!finished)setTimeout(refresh,2000);}
  }
  element('moreFiles').addEventListener('click',()=>manifest(true));
  const log=element('logBox');const stream=new EventSource('/api/job/log/stream?id='+encodeURIComponent(id));
  stream.addEventListener('log',event=>{log.textContent=(log.textContent+'\n'+event.data).slice(-200000);log.scrollTop=log.scrollHeight;});
  stream.addEventListener('done',()=>stream.close());
  window.addEventListener('pagehide',()=>stream.close());
  refresh();
})();