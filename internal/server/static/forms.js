(() => {
  'use strict';
  const App=window.App,forms=[],baselines=new WeakMap();
  const excluded=new Set(['password','password2','ui_password','ui_password2','rclone_extra_args','config_text','content','callback_secret','token']);
  const fields=form=>Array.from(form.elements).filter(el=>el.name&&el.type!=='submit'&&el.type!=='button'&&!el.hasAttribute('data-draft-skip')&&!excluded.has(el.name)&&el.type!=='password');
  const snapshot=form=>fields(form).map(el=>({name:el.name,type:el.type,value:el.value,checked:el.checked}));
  const dirtySnapshot=form=>Array.from(form.elements).filter(el=>el.name&&el.type!=='submit'&&el.type!=='button').map(el=>({name:el.name,value:el.value,checked:el.checked}));
  const key=form=>'rclone.ui.draft:'+location.pathname+location.search;
  function updateDirty(form){const dirty=JSON.stringify(dirtySnapshot(form))!==baselines.get(form);form.dataset.dirty=dirty?'1':'0';const hint=form.querySelector('[data-draft-state]');if(hint)hint.textContent=dirty?'有未保存的更改':'设置将在保存后生效';return dirty;}
  function saveDraft(form){updateDirty(form);if(!form.hasAttribute('data-draft-form'))return;try{sessionStorage.setItem(key(form),JSON.stringify({values:snapshot(form),tab:form.dataset.activeTab||''}));}catch(_){}}
  function applyTab(form,name){
    form.querySelectorAll('[data-form-tab]').forEach(button=>{const active=button.dataset.formTab===name;button.classList.toggle('active',active);button.setAttribute('aria-selected',String(active));});
    form.querySelectorAll('[data-form-panel]').forEach(panel=>panel.hidden=panel.dataset.formPanel!==name);
    form.dataset.activeTab=name;
  }
  function dependencies(form){
    const kind=form.elements.src_kind?.value||'remote';
    form.querySelectorAll('[data-source-kind]').forEach(panel=>panel.hidden=panel.dataset.sourceKind!==kind);
    if(form.elements.src_remote)form.elements.src_remote.required=kind==='remote';
    if(form.elements.src_local_root)form.elements.src_local_root.required=kind==='local';
    const directory=form.elements.group_by_directory,atomic=form.elements.atomic_publish;
    if(directory&&atomic){atomic.disabled=!directory.checked;if(!directory.checked)atomic.checked=false;}
    form.querySelectorAll('[data-atomic-fields]').forEach(panel=>panel.hidden=!atomic?.checked);
    const summary=form.querySelector('[data-flow-summary]');
    if(summary){
      const move=form.elements.transfer_mode?.value==='move';
      summary.textContent=atomic?.checked?'整组传输 → 校验 → 发布目录'+(move?' → 清理源文件':''):directory?.checked?'按影片目录整组'+(move?'移动':'复制'):'自动将同目录的同名分段一起'+(move?'移动':'复制');
    }
    const group=form.elements.limit_group;
    if(group&&form.elements.daily_limit){const input=form.elements.daily_limit;input.readOnly=!!group.value;const hint=form.querySelector('[data-limit-hint]');if(hint)hint.textContent=group.value?'当前使用所选分组的共享配额':'过去 24 小时滚动限额，0 表示不限';}
    form.querySelectorAll('[data-api-fields]').forEach(panel=>panel.hidden=!form.elements.api_enabled?.checked);
  }
  function datalist(input,remote){
    const list=document.getElementById(input.getAttribute('list'));if(!list)return;
    let timer=null,abort=null;
    const load=async()=>{
      const path=input.value;if(!path&&!remote)return;
      const name=remote?.value;if(remote&&!name)return;
      abort?.abort();abort=new AbortController();
      const url=remote?'/api/rclone/dirs?remote='+encodeURIComponent(name)+'&path='+encodeURIComponent(path):'/api/fs/list?path='+encodeURIComponent(path);
      try{const {data}=await App.request(url,{signal:abort.signal});list.replaceChildren(...(data.suggestions||[]).map(value=>{const option=document.createElement('option');option.value=value;return option;}));}
      catch(error){if(error.name!=='AbortError')list.replaceChildren();}
    };
    input.addEventListener('input',()=>{clearTimeout(timer);timer=setTimeout(load,250);});input.addEventListener('focus',load);remote?.addEventListener('change',load);
    window.addEventListener('pagehide',()=>{clearTimeout(timer);abort?.abort();},{once:true});
  }
  App.forms={
    markClean(form){
      if(form.hasAttribute('data-ui-form'))form.querySelectorAll('input[type=password]').forEach(el=>el.value='');
      for(const el of form.elements){
        if('defaultValue'in el)el.defaultValue=el.value;
        if('defaultChecked'in el)el.defaultChecked=el.checked;
        if(el.tagName==='SELECT')for(const option of el.options)option.defaultSelected=option.selected;
      }
      baselines.set(form,JSON.stringify(dirtySnapshot(form)));form.dataset.dirty='0';
      try{sessionStorage.removeItem(key(form));}catch(_){}
      const hint=form.querySelector('[data-draft-state]');if(hint)hint.textContent='设置已保存';
    },
    applyTab
  };
  document.addEventListener('DOMContentLoaded',()=>{
    document.querySelectorAll('[data-edit-form]').forEach(form=>{
      forms.push(form);baselines.set(form,JSON.stringify(dirtySnapshot(form)));
      if(form.hasAttribute('data-draft-form')){
        try{
          const draft=JSON.parse(sessionStorage.getItem(key(form))||'null');
          if(draft?.values){for(const value of draft.values){const el=fields(form).find(f=>f.name===value.name);if(el){if(el.type==='checkbox'||el.type==='radio')el.checked=value.checked;else el.value=value.value;}}if(draft.tab)applyTab(form,draft.tab);}
        }catch(_){}
      }
      dependencies(form);updateDirty(form);
      form.addEventListener('input',()=>saveDraft(form));form.addEventListener('change',()=>{dependencies(form);saveDraft(form);});
      form.querySelectorAll('[data-form-tab]').forEach(button=>button.addEventListener('click',()=>{applyTab(form,button.dataset.formTab);saveDraft(form);}));
      form.addEventListener('invalid',e=>{const panel=e.target.closest('[data-form-panel]');if(panel)applyTab(form,panel.dataset.formPanel);},true);
      form.querySelector('[data-form-reset]')?.addEventListener('click',async()=>{
        if(updateDirty(form)&&!await App.confirm({title:'放弃未保存的更改？',message:'表单将恢复为上次保存的设置。',label:'重置',danger:false}))return;
        form.reset();try{sessionStorage.removeItem(key(form));}catch(_){}dependencies(form);updateDirty(form);
        form.querySelectorAll('.app-field-error').forEach(el=>el.remove());form.querySelectorAll('.is-invalid').forEach(el=>el.classList.remove('is-invalid'));
        const feedback=form.querySelector('[data-form-feedback]');if(feedback)feedback.hidden=true;
      });
      form.querySelectorAll('[data-form-cancel]').forEach(link=>link.addEventListener('click',async event=>{
        if(event.ctrlKey||event.metaKey||event.shiftKey||event.button!==0)return;
        event.preventDefault();
        if(updateDirty(form)&&!await App.confirm({title:'放弃未保存的更改？',message:'当前表单草稿将丢弃。',label:'放弃更改',danger:false}))return;
        App.forms.markClean(form);location.assign(link.href);
      }));
      const local=form.querySelector('[data-local-suggest]');if(local)datalist(local,null);
      form.querySelectorAll('[data-remote-suggest]').forEach(input=>datalist(input,form.elements[input.dataset.remoteSuggest]));
      form.querySelectorAll('[data-preset-select]').forEach(select=>select.addEventListener('change',()=>{const target=form.elements[select.dataset.presetSelect];if(target&&select.value){target.value=Array.from(new Set((target.value+' '+select.value).trim().split(/\s+/).filter(Boolean))).join(' ');select.value='';saveDraft(form);}}));
    });
    document.querySelectorAll('[data-template-navigation]').forEach(select=>select.addEventListener('change',async()=>{
      const form=select.closest('form');if(form?.dataset.dirty==='1'&&!await App.confirm({title:'载入另一条规则？',message:'当前未保存的表单更改会被替换。',label:'载入',danger:false}))return;
      if(select.value){App.forms?.markClean(form);location.assign(select.dataset.templateNavigation+encodeURIComponent(select.value));}
    }));
  });
  window.addEventListener('beforeunload',event=>{if(forms.some(f=>f.dataset.dirty==='1')){event.preventDefault();event.returnValue='';}});
  document.addEventListener('submit',event=>{if(event.target.matches('[data-edit-form]')&&!event.target.hasAttribute('data-ui-form')&&!event.defaultPrevented)App.forms.markClean(event.target);});
})();
