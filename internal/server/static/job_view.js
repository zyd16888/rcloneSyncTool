(() => {
  'use strict';
  const App=window.App;
  App.JobDetail=class {
    constructor(root){
      this.root=root;this.id=root.dataset.jobId;this.abort=new AbortController();this.timer=null;this.destroyed=false;this.busy=false;this.pageCursors=[''];this.files=new Map();this.next='';this.transfers=new Map();this.view=null;this.stepSignature='';
      this.log=new App.LogPanel(root.querySelector('[data-log-panel]'),'/api/job/log/stream?id='+encodeURIComponent(this.id));
      root.querySelectorAll('[data-detail-tab]').forEach(button=>button.addEventListener('click',()=>{
        root.querySelectorAll('[data-detail-tab]').forEach(b=>{const selected=b===button;b.classList.toggle('active',selected);b.setAttribute('aria-selected',selected);});
        root.querySelectorAll('[data-detail-panel]').forEach(p=>p.hidden=p.dataset.detailPanel!==button.dataset.detailTab);
        if(button.dataset.detailTab==='logs')this.log.start();
      }));
      root.querySelector('[data-more-files]').addEventListener('click',()=>this.loadMore());
      this.actionHandler=()=>this.refresh();root.addEventListener('app:action-success',this.actionHandler);
      this.refresh();
    }
    set(name,value){const el=this.root.querySelector('[data-job-value="'+name+'"]');if(el)el.textContent=value??'—';}
    update(v){
      this.view=v;this.set('title',v.title);this.set('rule',v.rule_label);this.set('status',v.status_label);
      const status=this.root.querySelector('[data-job-status]');status.className='status status-'+v.status;
      this.set('ready',v.progress_known?App.bytes(v.ready_bytes)+' / '+App.bytes(v.planned_bytes):'正在确认清单');
      this.set('percent',v.progress_known?v.percent.toFixed(1)+'%':'—');
      this.set('actual',App.bytes(v.actual_bytes));this.set('reused',v.reused_bytes>0?App.bytes(v.reused_bytes):'—');
      this.set('speed',v.speed>0?App.speed(v.speed):'—');this.set('average',App.speed(v.average_speed));
      this.set('eta',v.eta>0?'约 '+App.duration(v.eta):v.status==='running'&&v.phase==='verifying'?'正在校验':v.status==='running'&&v.phase==='publishing'?'正在发布':'—');
      this.set('file-count',v.files_done+' / '+v.files_total);this.set('source',v.source||'—');this.set('destination',v.destination||'—');
      this.set('created',App.time(v.created_at));this.set('started',App.time(v.started_at));this.set('ended',App.time(v.ended_at));this.set('mode',v.mode==='move'?'移动':'复制');
      this.set('origin',v.origin==='api'?'API 提交':v.origin==='manual'?'手动运行':'规则调度');this.set('retry',v.retry_of||'—');this.set('stage',v.stage_path||'—');
      const error=this.root.querySelector('[data-job-error]');error.hidden=!v.error;error.textContent=v.error;
      this.root.querySelector('[data-job-stop]').hidden=!v.can_stop;this.root.querySelector('[data-job-retry]').hidden=!v.can_retry;
      this.root.querySelector('[data-job-retry-pending]').hidden=!v.retry_pending;
      this.root.querySelector('[data-job-average]').hidden=!v.terminal;
      this.root.querySelector('[data-job-current]').hidden=v.terminal;
      const progress=this.root.querySelector('[data-job-progress]');progress.className='app-progress'+(v.status==='done'?' is-done':v.status==='failed'?' is-failed':'');progress.querySelector('span').style.width=(v.progress_known?v.percent:0)+'%';progress.setAttribute('aria-valuenow',String(v.percent));
      const signature=JSON.stringify(v.steps);
      if(signature!==this.stepSignature){this.stepSignature=signature;const steps=this.root.querySelector('[data-job-steps]');steps.replaceChildren(...v.steps.map((step,i)=>{const item=document.createElement('div');item.className='app-step '+step.state;const number=document.createElement('b');number.textContent=step.state==='is-complete'?'✓':String(i+1);const label=document.createElement('span');label.textContent=step.name;item.append(number,label);return item;}));}
      this.renderFiles();
    }
    async loadMore(){
      if(this.loadingMore||this.destroyed)return;
      this.loadingMore=true;
      const more=this.root.querySelector('[data-more-files]');more.disabled=true;more.textContent='正在加载…';
      let cursor='';
      try{
        await this.manifestPromise;
        if(this.destroyed||!this.next)return;
        cursor=this.next;this.pageCursors.push(cursor);await this.manifest();
      }catch(error){
        if(cursor)this.pageCursors=this.pageCursors.filter(value=>value!==cursor);
        if(error.name!=='AbortError'&&!this.destroyed)App.toast('加载文件失败，请重试：'+error.message,true);
      }finally{this.loadingMore=false;more.disabled=false;more.textContent='加载更多文件';}
    }
    manifest(){
      if(this.manifestPromise)return this.manifestPromise;
      this.manifestPromise=this.fetchManifest().finally(()=>{this.manifestPromise=null;});
      return this.manifestPromise;
    }
    async fetchManifest(){
      const responses=await Promise.all(this.pageCursors.map(after=>App.request('/api/job/files?id='+encodeURIComponent(this.id)+(after?'&after='+encodeURIComponent(after):''),{signal:this.abort.signal})));
      if(this.destroyed)return;
      const nextFiles=new Map();
      for(const {data}of responses)for(const file of data.files||[])nextFiles.set(file.Path,file);
      this.files=nextFiles;this.next=responses[responses.length-1]?.data.next||'';
      const more=this.root.querySelector('[data-more-files]');more.hidden=!this.next;more.textContent='加载更多文件';
      this.renderFiles();
    }
    renderFiles(){
      const container=this.root.querySelector('[data-job-files]');const old=new Map(Array.from(container.children,n=>[n.dataset.filePath,n]));
      const nodes=[];
      for(const file of this.files.values()){
        const transfer=this.transfers.get(file.Path);
        const state=file.State==='done'?'done':transfer?'running':file.State==='failed'?'failed':'pending';
        const text=file.State==='done'?'已验证完成':transfer?'传输中':file.State==='failed'?'待重试':'待整组完成';
        const signature=JSON.stringify([file.Size,file.State,file.LastError,transfer?.bytes,transfer?.speed,transfer?.eta]);
        let node=old.get(file.Path);
        if(!node||node.dataset.signature!==signature){
          node=document.createElement('div');node.className='app-file-row';node.dataset.filePath=file.Path;node.dataset.signature=signature;
          const top=document.createElement('div');top.className='app-file-top';const name=document.createElement('span');name.className='app-file-name';name.textContent=file.Path;
          const badge=document.createElement('span');badge.className='status status-'+state;badge.textContent=text;top.append(name,badge);
          const meta=document.createElement('div');meta.className='app-file-meta';const size=document.createElement('span');size.textContent=transfer?App.bytes(transfer.bytes)+' / '+App.bytes(transfer.size):App.bytes(file.Size);
          const rate=document.createElement('span');rate.textContent=transfer?.speed>0?App.speed(transfer.speed):file.LastError||'';meta.append(size,rate);node.append(top,meta);
          if(transfer){const bar=document.createElement('div');bar.className='app-progress';const fill=document.createElement('span');fill.style.width=(transfer.size>0?Math.min(100,transfer.bytes*100/transfer.size):0)+'%';bar.append(fill);node.append(bar);}
        }
        nodes.push(node);
      }
      if(!nodes.length){const empty=document.createElement('div');empty.className='app-empty';empty.textContent=this.view?.files_total?'正在读取文件清单':'任务尚未生成文件清单';nodes.push(empty);}
      container.replaceChildren(...nodes);
      this.set('manifest-count','已加载 '+this.files.size+' / '+(this.view?.files_total||0)+' 个文件');
    }
    async refresh(){
      if(this.busy||this.destroyed)return;
      this.busy=true;
      try{
        const {data}=await App.request('/api/job?id='+encodeURIComponent(this.id),{signal:this.abort.signal});
        if(this.destroyed)return;this.update(data.view);
        const requests=[this.manifest()];
        if(data.view.status==='running'&&data.view.phase==='copying'){
          requests.push(App.request('/api/job/transfers?id='+encodeURIComponent(this.id),{signal:this.abort.signal}).then(({data})=>{if(this.destroyed)return;this.transfers=new Map((data.transfers||[]).map(t=>[t.name,t]));this.renderFiles();}));
        }else{this.transfers.clear();this.renderFiles();}
        await Promise.all(requests);this.set('updated','刚刚更新');
      }catch(error){if(error.name!=='AbortError'&&!this.destroyed)this.set('updated','更新暂时失败，保留上次结果');}
      finally{this.busy=false;if(!this.destroyed&&!this.view?.terminal)this.timer=setTimeout(()=>this.refresh(),2000);}
    }
    destroy(){this.destroyed=true;clearTimeout(this.timer);this.abort.abort();this.log.destroy();this.root.removeEventListener('app:action-success',this.actionHandler);}
  };
  const pages=[];
  document.addEventListener('DOMContentLoaded',()=>document.querySelectorAll('main [data-job-detail]').forEach(root=>pages.push(new App.JobDetail(root))));
  window.addEventListener('pagehide',()=>pages.forEach(page=>page.destroy()));
})();
