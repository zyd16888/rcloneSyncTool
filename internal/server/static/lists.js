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
    async refresh(automatic=false){
      if(this.destroyed)return;
      if(automatic&&(document.visibilityState!=='visible'||document.querySelector('dialog[open]')||document.querySelector('[data-row-menu][open]'))){this.schedule();return;}
      this.abort?.abort();this.abort=new AbortController();
      const params=new URLSearchParams(this.params);params.set('partial','1');
      try{
        const {data}=await App.request(this.root.dataset.listUrl+'?'+params,{signal:this.abort.signal});
        const doc=new DOMParser().parseFromString(data.html,'text/html');
        const oldY=window.scrollY;
        if(App.patchRows(this.live,doc)){
          for(const selector of ['[data-list-tabs]','#listPagination']){const next=doc.querySelector(selector),old=this.live.querySelector(selector);if(next&&old)old.replaceWith(next);}
        }else this.live.replaceChildren(...doc.body.childNodes);
        this.updateSort();
        if(automatic)window.scrollTo({top:oldY,behavior:'instant'});
        if(data.page&&String(data.page)!==this.params.get('page')){this.params.set('page',data.page);const u=new URL(location.href);u.searchParams.set('page',data.page);history.replaceState({},'',u);}
        const state=this.root.querySelector('[data-refresh-state]');state.classList.remove('is-stale');state.replaceChildren();const dot=document.createElement('span');dot.className='app-health-dot';state.append(dot,document.createTextNode('刚刚更新'));
      }catch(error){
        if(error.name==='AbortError')return;
        const state=this.root.querySelector('[data-refresh-state]');state.classList.add('is-stale');state.textContent='更新失败，保留上次结果';
        if(!automatic)App.toast(error.message,true);
      }finally{this.schedule();}
    }
    destroy(){this.destroyed=true;clearTimeout(this.timer);clearTimeout(this.debounce);this.abort?.abort();}
  }
  App.refreshLists=()=>controllers.forEach(c=>c.refresh());
  document.addEventListener('DOMContentLoaded',()=>document.querySelectorAll('[data-list-page]').forEach(root=>controllers.push(new ListPage(root))));
  window.addEventListener('pagehide',()=>controllers.forEach(c=>c.destroy()));
})();
