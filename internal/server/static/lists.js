(() => {
  'use strict';
  const App=window.App,controllers=[];
  class ListPage {
    constructor(root){
      this.root=root;this.form=root.querySelector('[data-list-filter]');this.live=root.querySelector('[data-live-list]');
      this.params=new URLSearchParams(location.search);this.abort=null;this.timer=null;this.debounce=null;this.destroyed=false;
      this.form.addEventListener('submit',e=>{e.preventDefault();this.applyForm();});
      this.form.addEventListener('input',e=>{if(e.target.name==='q'){clearTimeout(this.debounce);this.debounce=setTimeout(()=>this.applyForm(),300);}});
      this.form.addEventListener('change',()=>this.applyForm());
      root.addEventListener('click',e=>{
        if(e.target.closest('[data-list-refresh]')){e.preventDefault();this.refresh();}
        const status=e.target.closest('[data-list-status]'),sort=e.target.closest('[data-list-sort]'),page=e.target.closest('[data-page-link]');
        if(status){e.preventDefault();this.form.elements[root.dataset.statusParam].value=status.dataset.listStatus;this.applyForm();}
        if(sort){e.preventDefault();const active=this.form.elements.sort.value===sort.dataset.listSort;this.form.elements.direction.value=active?(this.form.elements.direction.value==='asc'?'desc':'asc'):sort.dataset.sortDirection;this.form.elements.sort.value=sort.dataset.listSort;this.applyForm();}
        if(page){e.preventDefault();this.params=new URL(page.href).searchParams;this.navigate();}
        if(e.target.closest('[data-list-reset]')){e.preventDefault();this.form.reset();for(const el of this.form.elements){if(el.name==='q'||el.name==='rule_id'||el.name==='group'||el.name==='mode'||el.name==='enable'||el.name===root.dataset.statusParam)el.value='';}this.form.elements.sort.value=root.dataset.listPage==='rules'?'name':'created';this.form.elements.direction.value=root.dataset.listPage==='rules'?'asc':'desc';this.applyForm();}
      });
      root.addEventListener('change',e=>{if(e.target.matches('[data-page-size]')){this.form.elements.page_size.value=e.target.value;this.applyForm();}});
      const defaults=new Map(Array.from(this.form.elements).filter(el=>el.name).map(el=>[el.name,el.name==='sort'?(root.dataset.listPage==='rules'?'name':'created'):el.name==='direction'?(root.dataset.listPage==='rules'?'asc':'desc'):el.name==='page_size'?'20':'']));
      window.addEventListener('popstate',()=>{this.params=new URLSearchParams(location.search);for(const el of this.form.elements)if(el.name)el.value=this.params.get(el.name)??defaults.get(el.name);this.refresh();});
      this.updateSort();
      this.schedule();
    }
    applyForm(){
      clearTimeout(this.debounce);
      this.params=new URLSearchParams();for(const [k,v]of new FormData(this.form)){if(v!=='')this.params.set(k,v);}
      this.params.set('page','1');const job=new URL(location.href).searchParams.get('job');if(job)this.params.set('job',job);
      this.navigate();
    }
    navigate(){
      const query=this.params.toString(),target=this.root.dataset.listUrl+(query?'?'+query:'');
      if(target!==location.pathname+location.search)history.pushState({},'',target);
      this.refresh();
    }
    schedule(){clearTimeout(this.timer);if(!this.destroyed)this.timer=setTimeout(()=>this.refresh(true),5000);}
    updateSort(){
      for(const button of this.root.querySelectorAll('[data-list-sort]')){
        const active=button.dataset.listSort===this.form.elements.sort.value;
        button.classList.toggle('is-active',active);button.dataset.sortLabel=active?(this.form.elements.direction.value==='asc'?'↑':'↓'):'';
        button.closest('th')?.setAttribute('aria-sort',active?(this.form.elements.direction.value==='asc'?'ascending':'descending'):'none');
      }
    }
    async refresh(automatic=false,reportError=true){
      if(this.destroyed)return false;
      if(automatic&&(this.loading||App.pendingActions||document.visibilityState!=='visible'||document.querySelector('dialog[open]')||document.querySelector('[data-row-menu][open]'))){this.schedule();return;}
      clearTimeout(this.timer);
      this.abort?.abort();const abort=this.abort=new AbortController();this.loading=true;
      if(!automatic)this.root.querySelector('[data-refresh-state]').textContent='正在更新…';
      const params=new URLSearchParams(this.params);params.set('partial','1');
      try{
        const {data}=await App.request(this.root.dataset.listUrl+'?'+params,{signal:abort.signal});
        if(abort.signal.aborted)return;
        if(typeof data.html!=='string')throw new Error('列表返回了无法识别的结果');
        const doc=new DOMParser().parseFromString(data.html,'text/html');
        const oldY=window.scrollY;
        if(App.patchRows(this.live,doc)){
          for(const selector of ['[data-list-tabs]','#listPagination']){const next=doc.querySelector(selector),old=this.live.querySelector(selector);if(next&&old)old.replaceWith(next);}
        }else this.live.replaceChildren(...doc.body.childNodes);
        this.updateSort();
        window.scrollTo({top:oldY,behavior:'instant'});
        if(data.page&&String(data.page)!==this.params.get('page')){this.params.set('page',data.page);const u=new URL(location.href);u.searchParams.set('page',data.page);history.replaceState({},'',u);}
        const state=this.root.querySelector('[data-refresh-state]');state.classList.remove('is-stale');state.replaceChildren();const dot=document.createElement('span');dot.className='app-health-dot';state.append(dot,document.createTextNode('刚刚更新'));
        return true;
      }catch(error){
        if(error.name==='AbortError')return;
        const state=this.root.querySelector('[data-refresh-state]');state.classList.add('is-stale');state.textContent=error.login?'登录已失效，请重新登录':'更新失败，保留上次结果';
        const retry=document.createElement(error.login?'a':'button');retry.className='btn btn-xs btn-ghost';retry.textContent=error.login?'重新登录':'重试刷新';
        if(error.login)retry.href='/login?next='+encodeURIComponent(location.pathname+location.search);else{retry.type='button';retry.dataset.listRefresh='';}state.append(retry);
        if(!automatic&&reportError)App.toast(error.message,true);
        return false;
      }finally{if(this.abort===abort){this.loading=false;this.schedule();}}
    }
    resume(){this.destroyed=false;this.params=new URLSearchParams(location.search);this.refresh();}
    destroy(){this.destroyed=true;clearTimeout(this.timer);clearTimeout(this.debounce);this.abort?.abort();}
  }
  App.refreshLists=async()=>{const results=await Promise.all(controllers.map(c=>c.refresh(false,false)));return results.every(result=>result!==false);};
  document.addEventListener('DOMContentLoaded',()=>document.querySelectorAll('[data-list-page]').forEach(root=>controllers.push(new ListPage(root))));
  window.addEventListener('pagehide',()=>controllers.forEach(c=>c.destroy()));
  window.addEventListener('pageshow',event=>{if(event.persisted)controllers.forEach(c=>c.resume());});
})();
