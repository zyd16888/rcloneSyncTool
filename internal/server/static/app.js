(() => {
  'use strict';
  const App = window.App = {};
  const iconNames = new Set(['sync','dashboard','tasks','cloud','settings','file','key','log','plus','search','refresh','edit','more','pause','play','stop','check','close','copy','arrow-right','arrow-left','chevron-right','chevron-left','chevron-down','sort','filter','sun','moon','menu','logout','info','warning','download','folder','scan','shield']);
  App.icon = name => {
    const svg = document.createElementNS('http://www.w3.org/2000/svg','svg');
    svg.setAttribute('class','app-icon'); svg.setAttribute('viewBox','0 0 24 24');
    svg.setAttribute('fill','none'); svg.setAttribute('stroke','currentColor'); svg.setAttribute('stroke-width','1.8');
    svg.setAttribute('stroke-linecap','round');svg.setAttribute('stroke-linejoin','round');svg.setAttribute('aria-hidden','true');
    const use=document.createElementNS(svg.namespaceURI,'use');use.setAttribute('href','/static/icons.svg#'+(iconNames.has(name)?name:'info'));svg.append(use);return svg;
  };
  App.bytes = n => {
    const units=['B','KiB','MiB','GiB','TiB','PiB'];let value=Math.max(0,Number(n)||0),i=0;
    while(value>=1024&&i<units.length-1){value/=1024;i++;}
    return (i?value.toFixed(1):Math.round(value))+' '+units[i];
  };
  App.speed = n => App.bytes(n)+'/s';
  App.time = value => {
    if(!value||String(value).startsWith('0001'))return '—';
    const d=new Date(value);return Number.isNaN(d.getTime())?'—':d.toLocaleString('zh-CN',{hour12:false});
  };
  App.duration = seconds => {
    seconds=Math.max(0,Math.ceil(Number(seconds)||0));
    if(seconds<60)return seconds+' 秒';
    if(seconds<3600)return Math.floor(seconds/60)+' 分 '+seconds%60+' 秒';
    return Math.floor(seconds/3600)+' 小时 '+Math.floor(seconds%3600/60)+' 分';
  };
  App.patchRows = (current,next) => {
    const rows=current.querySelector('[data-list-rows]'),incoming=next.querySelector('[data-list-rows]');
    if(!rows||!incoming)return false;
    const old=new Map(Array.from(rows.children,row=>[row.dataset.rowId,row]));
    rows.replaceChildren(...Array.from(incoming.children,row=>{const previous=old.get(row.dataset.rowId);return previous&&previous.innerHTML===row.innerHTML?previous:row;}));
    return true;
  };
  App.toast = (message,error=false) => {
    const stack=document.getElementById('appToasts');if(!stack)return;
    const host=document.querySelector('#appConfirm[open]')||Array.from(document.querySelectorAll('dialog[open]')).at(-1)||document.body;
    if(stack.parentElement!==host)host.append(stack);
    const item=document.createElement('div');item.className='app-toast'+(error?' is-error':'');item.setAttribute('role',error?'alert':'status');
    const mark=document.createElement('span');mark.className='app-toast-icon';mark.append(App.icon(error?'warning':'check'));
    const text=document.createElement('span');text.className='app-wrap';text.textContent=message;
    const close=document.createElement('button');close.type='button';close.className='btn btn-xs btn-ghost';close.setAttribute('aria-label','关闭提示');close.append(App.icon('close'));close.addEventListener('click',()=>item.remove());
    item.append(mark,text,close);stack.append(item);while(stack.children.length>4)stack.firstElementChild.remove();
    setTimeout(()=>item.remove(),error?10000:6500);
  };
  App.request = async (url,options={}) => {
    const target=new URL(url,location.origin);
    if(target.origin!==location.origin)throw new Error('请求地址无效');
    const headers=new Headers(options.headers||{});headers.set('X-Requested-With','XMLHttpRequest');
    if(!headers.has('Accept'))headers.set('Accept','application/json');
    const response=await fetch(target,{...options,headers,credentials:'same-origin'});
    if(response.status===401||(response.redirected&&new URL(response.url).pathname==='/login')){const error=new Error('登录已失效，请重新登录');error.login=true;throw error;}
    const isJSON=(response.headers.get('Content-Type')||'').includes('application/json');
    const data=isJSON?await response.json():await response.text();
    if(!response.ok){
      const message=isJSON?(data.message||data.error?.message||data.error||'请求失败'):String(data).slice(0,1200);
      const error=new Error(typeof message==='string'?message:'请求失败');error.field=isJSON?data.field:'';throw error;
    }
    return {data,response};
  };
  let confirmResolve=null;
  App.confirm = ({title,message,label='确认',danger=true}) => new Promise(resolve=>{
    const dialog=document.getElementById('appConfirm');if(!dialog||confirmResolve){resolve(false);return;}
    confirmResolve=resolve;document.getElementById('appConfirmTitle').textContent=title;
    document.getElementById('appConfirmMessage').textContent=message;
    const yes=document.getElementById('appConfirmOK');yes.textContent=label;yes.classList.toggle('btn-error',danger);yes.classList.toggle('btn-primary',!danger);
    dialog.showModal();document.getElementById('appConfirmCancel').focus();
  });
  function finishConfirm(result){
    const resolve=confirmResolve;confirmResolve=null;document.getElementById('appConfirm')?.close();resolve?.(result);
  }
  App.copy = async value => {
    try{
      if(navigator.clipboard?.writeText){await navigator.clipboard.writeText(value);return;}
      const input=document.createElement('textarea');input.value=value;input.style.position='fixed';input.style.opacity='0';document.body.append(input);input.select();
      const copied=document.execCommand('copy');input.remove();if(!copied)throw new Error();
    }catch(_){throw new Error('复制失败，请选中文本后复制');}
  };
  let floatingMenu=null;
  App.closeMenus=()=>{
    if(!floatingMenu)return;
    const {details,menu}=floatingMenu,restoreFocus=menu.contains(document.activeElement);floatingMenu=null;details.append(menu);menu.style.cssText='';details.open=false;
    if(restoreFocus)details.querySelector('summary')?.focus({preventScroll:true});
  };
  function placeMenu(details){
    App.closeMenus();
    const menu=details.querySelector('.app-menu-list'),anchor=details.querySelector('summary');if(!menu||!anchor)return;
    details.open=true;const bounds=anchor.getBoundingClientRect();menu.style.position='fixed';menu.style.zIndex='70';menu.style.bottom='auto';document.body.append(menu);
    const height=menu.offsetHeight,width=menu.offsetWidth;
    menu.style.left=Math.max(8,Math.min(innerWidth-width-8,bounds.right-width))+'px';
    menu.style.right='auto';menu.style.top=Math.max(8,bounds.bottom+height+8>innerHeight?bounds.top-height-5:bounds.bottom+5)+'px';
    floatingMenu={details,menu};
    menu.querySelector('a[href],button:not([disabled])')?.focus({preventScroll:true});
  }
  document.addEventListener('toggle',event=>{
    const details=event.target;if(!details.matches?.('[data-row-menu]'))return;
    if(details.open){if(floatingMenu?.details!==details)placeMenu(details);}else if(floatingMenu?.details===details)App.closeMenus();
  },true);
  document.addEventListener('click',event=>{
    if(floatingMenu&&!event.target.closest('.app-menu-list')&&!event.target.closest('[data-row-menu]'))App.closeMenus();
    const copy=event.target.closest('[data-copy]');
    if(copy){event.preventDefault();App.copy(copy.dataset.copy||'').then(()=>App.toast((copy.dataset.copyLabel||'内容')+'已复制')).catch(e=>App.toast(e.message,true));}
  });
  document.addEventListener('keydown',event=>{if(event.key==='Escape')App.closeMenus();});
  window.addEventListener('resize',App.closeMenus);window.addEventListener('scroll',App.closeMenus,{passive:true});

  const pending=new WeakSet();
  App.pendingActions=0;
  document.addEventListener('submit',async event=>{
    const form=event.target;
    if(!form.matches('form')||form.method.toLowerCase()!=='post')return;
    const enhanced=form.hasAttribute('data-ui-form');
    const confirmTitle=form.dataset.confirmTitle;
    if(!enhanced&&!confirmTitle)return;
    if(form.dataset.confirmed==='1'){delete form.dataset.confirmed;App.forms?.markClean(form);return;}
    event.preventDefault();
    if(pending.has(form))return;
    if(!form.reportValidity())return;
    pending.add(form);const submitter=event.submitter;
    App.closeMenus();
    if(confirmTitle&&!await App.confirm({title:confirmTitle,message:form.dataset.confirmMessage||'',label:form.dataset.confirmLabel||'确认',danger:form.dataset.confirmDanger!=='0'})){pending.delete(form);return;}
    if(!enhanced){form.dataset.confirmed='1';pending.delete(form);form.requestSubmit(submitter);return;}
    const buttons=Array.from(form.querySelectorAll('button[type="submit"]'));
    const states=buttons.map(b=>({button:b,disabled:b.disabled,html:b.innerHTML}));
    buttons.forEach(b=>{b.disabled=true;b.setAttribute('aria-busy','true');if(b===submitter)b.textContent='提交中…';});
    form.querySelectorAll('.app-field-error').forEach(el=>el.remove());form.querySelectorAll('.is-invalid').forEach(el=>el.classList.remove('is-invalid'));
    const previousFeedback=form.querySelector('[data-form-feedback]');if(previousFeedback)previousFeedback.hidden=true;
    const submitted=App.forms?.capture(form);App.pendingActions++;
    try{
      const body=new URLSearchParams();for(const [key,value]of new FormData(form)){if(typeof value==='string')body.append(key,value);}
      const {data,response}=await App.request(form.action,{method:'POST',body});
      if(typeof data!=='object'){
        if(response.redirected){if(!App.forms?.markSaved(form,submitted))location.assign(response.url);return;}
        throw new Error('操作返回了无法识别的结果，请刷新确认状态');
      }
      const dirty=App.forms?.markSaved(form,submitted,data.values);App.toast(data.message||'操作已完成');
      if(data.config_path_display!==undefined){const path=form.querySelector('[data-config-path]');if(path)path.textContent='当前路径：'+data.config_path_display;}
      if(dirty){
        let feedback=form.querySelector('[data-form-feedback]');
        if(!feedback){feedback=document.createElement('div');feedback.dataset.formFeedback='';form.append(feedback);}
        feedback.className='alert alert-info app-form-feedback';feedback.textContent='本次提交已保存，提交后的修改仍未保存。';feedback.hidden=false;
        if(data.next){const next=new URL(data.next,location.origin);if(next.origin===location.origin&&next.href!==location.href){const link=document.createElement('a');link.className='link';link.href=next.href;link.textContent='查看已提交结果';feedback.append(link);}}
        return;
      }
      if(form.hasAttribute('data-refresh-page')){location.assign(data.next||location.href);return;}
      form.dispatchEvent(new CustomEvent('app:action-success',{bubbles:true,detail:data}));
      if(data.next){
        const next=new URL(data.next,location.origin);
        if(next.origin!==location.origin)throw new Error('返回地址无效');
        if(next.pathname==='/jobs/view'&&document.getElementById('jobDrawer')?.open){await App.openJob(next.searchParams.get('id'));return;}
        if(next.pathname!==location.pathname||(next.pathname==='/jobs/view'&&next.searchParams.get('id')!==new URL(location.href).searchParams.get('id'))){location.assign(next);return;}
      }
      if(await App.refreshLists?.()===false)App.toast('操作已成功，列表更新失败，请点击“重试刷新”，无需重复提交。',true);
    }catch(error){
      if(error.name==='AbortError')return;
      let feedback=form.querySelector('[data-form-feedback]');
      if(!feedback&&form.hasAttribute('data-edit-form')){feedback=document.createElement('div');feedback.dataset.formFeedback='';feedback.className='alert alert-error app-form-feedback';feedback.setAttribute('role','alert');form.append(feedback);}
      if(feedback){feedback.className='alert alert-error app-form-feedback';feedback.textContent=error.message;feedback.hidden=false;}
      const field=Array.from(form.elements).find(el=>el.name===error.field);
      if(field){const panel=field.closest('[data-form-panel]');if(panel)App.forms?.applyTab(form,panel.dataset.formPanel);field.closest('.app-field')?.classList.add('is-invalid');field.focus();const message=document.createElement('span');message.className='app-field-error';message.textContent=error.message;field.closest('.app-field')?.append(message);}
      App.toast(error.message,true);
    }finally{
      states.forEach(({button,disabled,html})=>{button.disabled=disabled;button.removeAttribute('aria-busy');button.innerHTML=html;});pending.delete(form);App.pendingActions--;
    }
  });

  let detailController=null,drawerAbort=null,currentJob='';
  App.openJob=async(id,push=true)=>{
    if(!id)return;
    const dialog=document.getElementById('jobDrawer'),body=document.getElementById('jobDrawerBody');if(!dialog||!body)return;
    detailController?.destroy();detailController=null;drawerAbort?.abort();drawerAbort=new AbortController();currentJob=id;
    body.replaceChildren();const loading=document.createElement('div');loading.className='app-drawer-loading';loading.textContent='正在读取任务详情…';body.append(loading);
    if(!dialog.open)dialog.showModal();
    if(push){const u=new URL(location.href);u.searchParams.set('job',id);history.pushState({job:id},'',u);}
    try{
      const {data}=await App.request('/jobs/view?id='+encodeURIComponent(id)+'&partial=1',{headers:{Accept:'text/html'},signal:drawerAbort.signal});
      if(currentJob!==id)return;
      const doc=new DOMParser().parseFromString(data,'text/html');const root=doc.querySelector('[data-job-detail]');
      if(!root)throw new Error('暂时无法读取任务详情');
      body.replaceChildren(root);detailController=new App.JobDetail(root);
    }catch(error){if(error.name!=='AbortError'){loading.textContent=error.message;App.toast(error.message,true);}}
  };
  App.closeJob=(replace=true)=>{
    detailController?.destroy();detailController=null;drawerAbort?.abort();currentJob='';document.getElementById('jobDrawerBody')?.replaceChildren();
    const dialog=document.getElementById('jobDrawer');if(dialog?.open)dialog.close();
    if(replace){const u=new URL(location.href);u.searchParams.delete('job');history.replaceState({},'',u);}
    App.refreshLists?.();
  };
  document.addEventListener('click',event=>{
    const link=event.target.closest('[data-open-job]');
    if(link&&!event.ctrlKey&&!event.metaKey&&!event.shiftKey&&event.button===0){event.preventDefault();App.openJob(link.dataset.openJob);}
    if(event.target.closest('[data-close-job]'))App.closeJob();
  });
  window.addEventListener('popstate',()=>{const id=new URL(location.href).searchParams.get('job');if(id&&id!==currentJob)App.openJob(id,false);else if(!id&&currentJob)App.closeJob(false);});
  window.addEventListener('pagehide',()=>{detailController?.destroy();drawerAbort?.abort();});
  window.addEventListener('pageshow',event=>{if(event.persisted){const id=new URL(location.href).searchParams.get('job');if(id)App.openJob(id,false);}});

  document.addEventListener('DOMContentLoaded',()=>{
    const theme=document.getElementById('btnThemeToggle'),html=document.documentElement;
    const updateTheme=()=>{const dark=html.dataset.theme==='dark';theme?.replaceChildren(App.icon(dark?'sun':'moon'));theme?.setAttribute('aria-label',dark?'切换浅色模式':'切换深色模式');};
    updateTheme();theme?.addEventListener('click',()=>{html.dataset.theme=html.dataset.theme==='dark'?'winter':'dark';try{localStorage.setItem('ui.theme',html.dataset.theme);}catch(_){}updateTheme();});
    document.getElementById('btnSidebarToggle')?.addEventListener('click',()=>{const collapsed=html.classList.toggle('sidebar-collapsed');try{localStorage.setItem('ui.sidebarCollapsed',collapsed?'1':'0');}catch(_){}});
    document.getElementById('appConfirmOK')?.addEventListener('click',()=>finishConfirm(true));
    document.getElementById('appConfirmCancel')?.addEventListener('click',()=>finishConfirm(false));
    document.getElementById('appConfirm')?.addEventListener('cancel',event=>{event.preventDefault();finishConfirm(false);});
    document.querySelectorAll('dialog').forEach(dialog=>dialog.addEventListener('close',()=>{
      const stack=document.getElementById('appToasts');if(stack&&dialog.contains(stack))document.body.append(stack);
    }));
    document.getElementById('jobDrawer')?.addEventListener('cancel',event=>{event.preventDefault();App.closeJob();});
    document.getElementById('jobDrawer')?.addEventListener('click',event=>{if(event.target===event.currentTarget){const b=event.currentTarget.getBoundingClientRect();if(event.clientX<b.left||event.clientX>b.right)App.closeJob();}});
    const id=new URL(location.href).searchParams.get('job');if(id&&App.JobDetail)App.openJob(id,false);
  });
})();
