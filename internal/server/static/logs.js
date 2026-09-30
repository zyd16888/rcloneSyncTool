(() => {
  'use strict';
  const App=window.App;
  App.LogPanel=class {
    constructor(root,url){
      this.root=root;this.url=url;this.box=root.querySelector('[data-log-box]');this.level=root.querySelector('[data-log-level]');this.search=root.querySelector('[data-log-search]');this.follow=root.querySelector('[data-log-follow]');
      this.lines=[];this.nextID=0;this.source=null;this.pending='';this.newCount=0;this.replay=false;
      this.level.addEventListener('change',()=>this.render());this.search.addEventListener('input',()=>this.render());
      this.follow.addEventListener('change',()=>{if(this.follow.checked)this.toBottom();});
      this.box.addEventListener('scroll',()=>{if(this.box.scrollHeight-this.box.scrollTop-this.box.clientHeight>35)this.follow.checked=false;},{passive:true});
      root.querySelector('[data-log-new]').addEventListener('click',()=>{this.follow.checked=true;this.toBottom();});
      root.querySelector('[data-log-clear]').addEventListener('click',()=>{this.lines=[];this.box.replaceChildren();this.newCount=0;this.updateNew();});
      root.querySelector('[data-log-copy]').addEventListener('click',()=>App.copy(this.visible().map(x=>x.text).join('\n')).then(()=>App.toast('当前日志已复制')).catch(e=>App.toast(e.message,true)));
      root.querySelector('[data-log-export]').addEventListener('click',()=>{
        const url=URL.createObjectURL(new Blob([this.visible().map(x=>x.text).join('\n')],{type:'text/plain;charset=utf-8'}));
        const link=document.createElement('a');link.href=url;link.download='rclone-log-'+Date.now()+'.txt';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
      });
    }
    start(){
      if(this.source)return;
      this.source=new EventSource(this.url);
      this.source.addEventListener('init',()=>{this.replay=true;this.pending='';this.state('日志已连接');});
      this.source.addEventListener('log',e=>this.append(e.data));
      this.source.addEventListener('done',()=>{if(this.pending){this.addLines([this.pending]);this.pending='';}this.source.close();this.state('任务已结束，日志已完整读取');});
      this.source.onerror=()=>this.state('日志连接暂时中断，正在重连');
    }
    state(text){this.root.querySelector('[data-log-state]').textContent=text;}
    append(chunk){
      this.pending+=chunk;const parts=this.pending.split('\n');this.pending=parts.pop();
      let values=parts.filter(Boolean);
      if(this.replay){
        const previous=this.lines.map(x=>x.raw);let overlap=0;
        for(let n=Math.min(previous.length,values.length,400);n>0;n--){if(previous.slice(-n).every((v,i)=>v===values[i])){overlap=n;break;}}
        values=values.slice(overlap);this.replay=false;
      }
      this.addLines(values);
    }
    addLines(values){
      if(!values.length)return;
      for(const raw of values){
        let text=raw.replace(/\r$/,''),level='info';
        try{
          const data=JSON.parse(text);
          if(data&&typeof data==='object'){
            level=String(data.level||'info').toLowerCase();
            const time=data.time?new Date(data.time).toLocaleTimeString('zh-CN',{hour12:false}):'';
            text=[time,level.toUpperCase(),data.object||data.source||'',data.msg||data.message||text].filter(Boolean).join('  ');
          }
        }catch(_){if(/\b(error|failed|失败|错误)\b/i.test(text))level='error';else if(/\b(warn|warning|警告)\b/i.test(text))level='warning';}
        if(level==='warn')level='warning';
        this.lines.push({id:++this.nextID,raw,text,level});
      }
      const trimmed=Math.max(0,this.lines.length-2000);if(trimmed)this.lines.splice(0,trimmed);
      this.render();
      if(!this.follow.checked){this.newCount+=values.length;this.updateNew();}
    }
    visible(){
      const q=this.search.value.toLowerCase(),level=this.level.value;
      return this.lines.filter(x=>(!level||x.level===level)&&(!q||x.text.toLowerCase().includes(q)));
    }
    render(){
      const y=this.box.scrollTop;
      const existing=new Map(Array.from(this.box.children,node=>[Number(node.dataset.logId),node]));
      const nodes=this.visible().map(line=>{
        let node=existing.get(line.id);
        if(!node){node=document.createElement('span');node.dataset.logId=line.id;node.className='app-log-line'+(line.level==='error'?' is-error':line.level==='warning'?' is-warning':'');node.textContent=line.text;}
        return node;
      });
      this.box.replaceChildren(...nodes);
      if(this.follow.checked)this.toBottom();else this.box.scrollTop=y;
    }
    updateNew(){const button=this.root.querySelector('[data-log-new]');button.hidden=this.newCount===0;button.textContent='查看 '+this.newCount+' 条新日志';}
    toBottom(){this.box.scrollTop=this.box.scrollHeight;this.newCount=0;this.updateNew();}
    destroy(){this.source?.close();this.source=null;}
  };
  const panels=[];
  document.addEventListener('DOMContentLoaded',()=>{
    document.querySelectorAll('[data-log-url]').forEach(container=>{
      const root=container.matches('[data-log-panel]')?container:container.querySelector('[data-log-panel]');
      if(root){const panel=new App.LogPanel(root,container.dataset.logUrl);panels.push(panel);panel.start();}
    });
  });
  window.addEventListener('pagehide',()=>panels.forEach(p=>p.destroy()));
  window.addEventListener('pageshow',event=>{if(event.persisted)panels.forEach(p=>p.start());});
})();
