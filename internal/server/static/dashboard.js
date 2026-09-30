(() => {
  let timer=null,stopped=false,abort=null;
  const points=[],canvas=document.getElementById('rtSpeed'),rule=document.getElementById('rtRule'),windowSize=document.getElementById('rtWindow');
  let lastSnapshot=Date.now(),lastUpdated=Date.now();
  const chart=canvas?createLineChart(canvas,{padding:50,zeroBaseline:true,color:getComputedStyle(document.documentElement).getPropertyValue('--app-primary').trim()}):null;
  function draw(){
    if(!canvas||!canvas.closest('details').open)return;
    const width=canvas.clientWidth;canvas.width=Math.max(1,width)*devicePixelRatio;canvas.height=240*devicePixelRatio;
    chart.draw(points,v=>App.speed(v));
  }
  async function tick(){
    if(stopped)return;
    if(document.visibilityState!=='visible'){timer=setTimeout(tick,3000);return;}
    clearTimeout(timer);abort?.abort();const request=abort=new AbortController();
    try{
      const {data}=await App.request('/api/stats/now?rule_id='+encodeURIComponent(rule?.value||''),{signal:request.signal});
      if(request.signal.aborted)return;
      const text=(id,value)=>{const el=document.getElementById(id);if(el)el.textContent=value;};
      text('statSpeed',App.speed(data.globalSpeedTotal));text('statRunning',data.globalRunningJobs);text('statToday',App.bytes(data.bytesToday));text('stat24h',App.bytes(data.bytes24h));
      if(data.statusCounts){text('statBlocked',data.statusCounts.blocked||0);text('statPending',data.statusCounts.pending||0);text('statFailed',data.statusCounts.attention||0);}
      points.push({x:data.ts,y:Math.max(0,data.speedTotal)});const cutoff=Date.now()-Number(windowSize.value)*1000;while(points.length&&points[0].x<cutoff)points.shift();draw();
      if(Date.now()-lastSnapshot>=5000&&!document.querySelector('dialog[open]')){
        const {data:snapshot}=await App.request('/?partial=1',{signal:request.signal});
        if(request.signal.aborted)return;
        const current=document.querySelector('[data-dashboard-live]'),next=new DOMParser().parseFromString(snapshot.html,'text/html'),y=window.scrollY;
        if(App.patchRows(current,next))current.querySelector('[data-dashboard-usage]').replaceWith(next.querySelector('[data-dashboard-usage]'));
        else current.replaceChildren(...next.body.childNodes);
        window.scrollTo({top:y,behavior:'instant'});lastSnapshot=Date.now();
        lastUpdated=Date.now();const state=document.querySelector('[data-dashboard-state]');state.textContent='刚刚更新';state.classList.remove('is-stale');
      }
    }catch(error){if(error.name!=='AbortError'){
      lastSnapshot=0;const state=document.querySelector('[data-dashboard-state]');state.classList.add('is-stale');state.textContent=(error.login?'登录已失效，请重新登录':'更新失败，保留上次结果')+' · 最后更新 '+new Date(lastUpdated).toLocaleTimeString('zh-CN',{hour12:false});
      if(error.login){const link=document.createElement('a');link.className='link';link.href='/login?next=%2F';link.textContent='重新登录';state.append(link);}
    }}
    finally{if(!stopped&&abort===request)timer=setTimeout(tick,3000);}
  }
  rule?.addEventListener('change',()=>{points.length=0;draw();});
  windowSize?.addEventListener('change',draw);document.querySelector('[data-dashboard-chart]')?.addEventListener('toggle',draw);window.addEventListener('resize',draw);
  new MutationObserver(draw).observe(document.documentElement,{attributes:true,attributeFilter:['data-theme']});
  window.addEventListener('pagehide',()=>{stopped=true;clearTimeout(timer);abort?.abort();});
  window.addEventListener('pageshow',event=>{if(event.persisted){stopped=false;lastSnapshot=0;tick();}});
  document.addEventListener('DOMContentLoaded',tick);
})();
