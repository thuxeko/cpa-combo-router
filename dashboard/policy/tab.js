// The error policy panel edits state.errorPolicy. The Go side is authoritative:
// this file mirrors the built-in table only so an unconfigured plugin shows a
// complete picture instead of an empty form.
const POLICY_ACTIONS=[
  {value:'fallback',label:'Thử đích kế tiếp'},
  {value:'stop',label:'Dừng lại'},
  {value:'fallback_no_penalty',label:'Thử tiếp, không phạt'}
];
// Mirrors defaultErrorPolicy() in errorpolicy.go.
function builtinErrorPolicy(){
  return{
    default:{action:'fallback',cooldown_seconds:30},
    rules:[
      {status:[429],action:'fallback',cooldown_seconds:20,honor_retry_after:true,max_cooldown_seconds:900},
      {status:[401,402,403,408],action:'fallback',cooldown_seconds:600},
      {status:[500,501,502,503,504,505,506,507,508,510,511,520,521,522,523,524,525,526],action:'fallback',cooldown_seconds:5,backoff:'exponential',max_cooldown_seconds:300},
      {status:[400,405,406,409,411,413,414,415,422],action:'stop'},
      {status:[499],action:'stop'}
    ]
  };
}
function policyActionLabel(value){
  const match=POLICY_ACTIONS.find(entry=>entry.value===value);
  return match?match.label:value;
}
function policyActionSelect(selected){
  const select=document.createElement('select');
  select.className='policy-select';
  POLICY_ACTIONS.forEach(entry=>{
    const option=document.createElement('option');
    option.value=entry.value;
    option.textContent=entry.label;
    if(entry.value===selected)option.selected=true;
    select.appendChild(option);
  });
  return select;
}
function normalizePolicyRule(value){
  const rule=value&&typeof value==='object'?value:{};
  const statuses=Array.isArray(rule.status)?rule.status.map(Number).filter(status=>Number.isInteger(status)&&status>=100&&status<=599):[];
  const action=POLICY_ACTIONS.some(entry=>entry.value===rule.action)?rule.action:'fallback';
  return{
    statuses,
    statusText:statuses.join(', '),
    action,
    cooldown_seconds:Number.isInteger(rule.cooldown_seconds)?String(rule.cooldown_seconds):'0',
    honor_retry_after:Boolean(rule.honor_retry_after),
    backoff:rule.backoff==='exponential',
    max_cooldown_seconds:Number.isInteger(rule.max_cooldown_seconds)?rule.max_cooldown_seconds:0
  };
}
function normalizeErrorPolicy(config){
  const wire=config&&typeof config==='object'?config.error_policy:null;
  const configured=Boolean(wire&&typeof wire==='object'&&(wire.default||(Array.isArray(wire.rules)&&wire.rules.length)));
  const source=configured?wire:builtinErrorPolicy();
  const fallbackDefault={action:'fallback',cooldown_seconds:30};
  const wireDefault=source&&typeof source.default==='object'&&source.default?source.default:fallbackDefault;
  const defaultAction=POLICY_ACTIONS.some(entry=>entry.value===wireDefault.action)?wireDefault.action:'fallback';
  const rules=Array.isArray(source.rules)?source.rules.map(normalizePolicyRule):[];
  return{
    configured,
    defaultAction,
    defaultCooldown:Number.isInteger(wireDefault.cooldown_seconds)?String(wireDefault.cooldown_seconds):'30',
    rules
  };
}
function normalizeAttemptTimeout(config){
  const value=config&&typeof config==='object'?config.attempt_timeout_seconds:null;
  return Number.isInteger(value)&&value>=0?String(value):'45';
}
// parseStatusList turns "429, 500-504" into [429,500,501,502,503,504]. It returns
// the invalid fragments so the panel can name what it could not read.
function parseStatusList(text){
  const statuses=[];
  const invalid=[];
  String(text||'').split(',').forEach(part=>{
    const token=part.trim();
    if(!token)return;
    const range=/^(\d{3})\s*-\s*(\d{3})$/.exec(token);
    if(range){
      const start=Number(range[1]),end=Number(range[2]);
      if(start<100||end>599||start>end){invalid.push(token);return}
      for(let status=start;status<=end;status++)statuses.push(status);
      return;
    }
    const single=/^\d{3}$/.exec(token);
    if(single){statuses.push(Number(token));return}
    invalid.push(token);
  });
  return{statuses:Array.from(new Set(statuses)).sort((left,right)=>left-right),invalid};
}
// compactStatusList reverses parseStatusList so the text box shows "500-504"
// instead of twenty separate numbers.
function compactStatusList(statuses){
  const sorted=Array.from(new Set(statuses)).sort((left,right)=>left-right);
  const parts=[];
  let index=0;
  while(index<sorted.length){
    let end=index;
    while(end+1<sorted.length&&sorted[end+1]===sorted[end]+1)end++;
    if(end-index>=2)parts.push(sorted[index]+'-'+sorted[end]);
    else for(let cursor=index;cursor<=end;cursor++)parts.push(String(sorted[cursor]));
    index=end+1;
  }
  return parts.join(', ');
}
function policyCooldownEnabled(rule){return rule.action==='fallback'}
function createPolicyRuleRow(rule,index){
  const row=document.getElementById('policy-rule-template').content.firstElementChild.cloneNode(true);
  row.dataset.ruleIndex=String(index);
  row.querySelector('[data-policy-field="status"]').value=rule.statusText;
  const action=row.querySelector('[data-policy-field="action"]');
  const replacement=policyActionSelect(rule.action);
  action.replaceWith(replacement);
  replacement.dataset.policyField='action';
  replacement.setAttribute('aria-label','Hành động cho quy tắc '+(index+1));
  row.querySelector('[data-policy-field="cooldown_seconds"]').value=rule.cooldown_seconds;
  row.querySelector('[data-policy-field="honor_retry_after"]').checked=rule.honor_retry_after;
  row.querySelector('[data-policy-field="backoff"]').checked=rule.backoff;
  applyPolicyRowState(row,rule);
  return row;
}
// A "stop" rule never penalises a target, so its cooldown and flags are disabled
// rather than silently ignored by the server.
function applyPolicyRowState(row,rule){
  const enabled=policyCooldownEnabled(rule);
  row.dataset.action=rule.action;
  row.classList.toggle('is-stop',!enabled);
  row.querySelector('[data-policy-field="cooldown_seconds"]').disabled=!enabled;
  row.querySelector('[data-policy-field="honor_retry_after"]').disabled=!enabled;
  row.querySelector('[data-policy-field="backoff"]').disabled=!enabled;
}
function renderPolicy(){
  const policy=state.errorPolicy;
  if(!policy)return;
  policyCountEl.textContent=policy.rules.length+' quy tắc';
  policyRulesEl.replaceChildren();
  policy.rules.forEach((rule,index)=>policyRulesEl.appendChild(createPolicyRuleRow(rule,index)));
  policyDefaultActionEl.value=policy.defaultAction;
  policyDefaultCooldownEl.value=policy.defaultCooldown;
  updatePolicyDefaultSummary();
  updatePolicyWarnings();
}
function updatePolicyDefaultSummary(){
  const policy=state.errorPolicy;
  if(!policy)return;
  if(policy.defaultAction==='fallback'){
    policyDefaultSummaryEl.textContent='chờ '+policy.defaultCooldown+' giây';
  }else{
    policyDefaultSummaryEl.textContent=policyActionLabel(policy.defaultAction).toLowerCase();
  }
  const enabled=policyCooldownEnabled({action:policy.defaultAction});
  policyDefaultCooldownEl.disabled=!enabled;
  policyDefaultRowEl.classList.toggle('is-stop',!enabled);
}
// Client-side warnings mirror policyWarnings() in Go. The server repeats them at
// save time; showing them live just avoids a round trip.
function policyWarnings(){
  const policy=state.errorPolicy;
  if(!policy)return[];
  const warnings=[];
  policy.rules.forEach(rule=>{
    if(rule.action==='stop'&&rule.statuses.includes(429))warnings.push('429 đang đặt là dừng lại: gặp giới hạn tốc độ đầu tiên là kết thúc request luôn, không thử đích khác.');
    rule.statuses.forEach(status=>{
      if(status>=500&&rule.action==='stop')warnings.push(status+' đang đặt là dừng lại: nhà cung cấp có sự cố là kết thúc request luôn, không thử đích khác.');
    });
  });
  if(policy.defaultAction==='stop')warnings.push('Hành động mặc định là dừng lại: mọi mã không có trong bảng trên sẽ kết thúc request mà không thử đích khác.');
  return Array.from(new Set(warnings));
}
function updatePolicyWarnings(){
  const warnings=policyWarnings();
  policyWarningsEl.replaceChildren();
  if(warnings.length===0){
    policyWarningsEl.hidden=true;
    return;
  }
  const title=document.createElement('strong');
  title.textContent='Trước khi lưu:';
  policyWarningsEl.appendChild(title);
  const list=document.createElement('ul');
  warnings.forEach(warning=>{
    const item=document.createElement('li');
    item.textContent=warning;
    list.appendChild(item);
  });
  policyWarningsEl.appendChild(list);
  policyWarningsEl.hidden=false;
}
function policyRuleIndexFrom(target){
  const row=target.closest('.policy-rule');
  return row?Number.parseInt(row.dataset.ruleIndex,10):-1;
}
function policyRowFromRule(rule,row){
  const statusText=row.querySelector('[data-policy-field="status"]').value;
  const parsed=parseStatusList(statusText);
  rule.statusText=statusText;
  rule.statuses=parsed.statuses;
  rule.invalidStatuses=parsed.invalid;
  rule.action=row.querySelector('[data-policy-field="action"]').value;
  rule.cooldown_seconds=row.querySelector('[data-policy-field="cooldown_seconds"]').value;
  rule.honor_retry_after=row.querySelector('[data-policy-field="honor_retry_after"]').checked;
  rule.backoff=row.querySelector('[data-policy-field="backoff"]').checked;
  return rule;
}
function updatePolicyFromInput(target){
  const policy=state.errorPolicy;
  if(!policy)return;
  if(target===policyDefaultActionEl){
    policy.defaultAction=target.value;
    updatePolicyDefaultSummary();
    updatePolicyWarnings();
    refreshDirty();
    return;
  }
  if(target===policyDefaultCooldownEl){
    policy.defaultCooldown=target.value;
    updatePolicyDefaultSummary();
    refreshDirty();
    return;
  }
  const index=policyRuleIndexFrom(target);
  if(index<0||!policy.rules[index])return;
  const row=target.closest('.policy-rule');
  const rule=policyRowFromRule(policy.rules[index],row);
  applyPolicyRowState(row,rule);
  updatePolicyWarnings();
  refreshDirty();
}
function serializeErrorPolicy(){
  const policy=state.errorPolicy;
  if(!policy)return null;
  const rules=policy.rules.map(rule=>{
    const entry={status:rule.statuses,action:rule.action};
    if(rule.action==='fallback'){
      const cooldown=integerDraftValue(rule.cooldown_seconds);
      if(Number.isInteger(cooldown))entry.cooldown_seconds=cooldown;
      if(rule.honor_retry_after)entry.honor_retry_after=true;
      if(rule.backoff)entry.backoff='exponential';
      if(Number.isInteger(rule.max_cooldown_seconds)&&rule.max_cooldown_seconds>0)entry.max_cooldown_seconds=rule.max_cooldown_seconds;
    }
    return entry;
  });
  const fallbackDefault={action:policy.defaultAction};
  if(policy.defaultAction==='fallback'){
    const cooldown=integerDraftValue(policy.defaultCooldown);
    if(Number.isInteger(cooldown))fallbackDefault.cooldown_seconds=cooldown;
  }
  return{default:fallbackDefault,rules};
}
// validatePolicy mirrors the server's own checks so an obvious mistake is named
// before a round trip.
function validatePolicy(){
  const policy=state.errorPolicy;
  const errors=[];
  if(!policy)return errors;
  const claimed=new Map();
  policy.rules.forEach((rule,index)=>{
    const label='Quy tắc '+(index+1);
    if((rule.invalidStatuses||[]).length)errors.push(label+': không đọc được '+(rule.invalidStatuses||[]).join(', ')+'. Dùng mã như 429 hoặc khoảng như 500-504.');
    if(!rule.statuses.length)errors.push(label+': cần ít nhất một mã trạng thái.');
    rule.statuses.forEach(status=>{
      if(claimed.has(status))errors.push(label+': mã '+status+' đã dùng ở quy tắc '+claimed.get(status)+'.');
      else claimed.set(status,index+1);
    });
    if(policyCooldownEnabled(rule)&&!Number.isInteger(integerDraftValue(rule.cooldown_seconds)))errors.push(label+': cooldown phải là số nguyên không âm.');
    if(!policyCooldownEnabled(rule)&&rule.honor_retry_after)errors.push(label+': quy tắc dừng lại không thể dùng Retry-After.');
  });
  if(policy.defaultAction==='fallback'&&!Number.isInteger(integerDraftValue(policy.defaultCooldown)))errors.push('Cooldown mặc định phải là số nguyên không âm.');
  if(!Number.isInteger(integerDraftValue(state.attemptTimeout))||integerDraftValue(state.attemptTimeout)<0)errors.push('Thời gian chờ mỗi lần thử phải là số nguyên không âm.');
  return errors;
}
function addPolicyRule(){
  const policy=state.errorPolicy;
  if(!policy)return;
  policy.rules.push({statuses:[],statusText:'',action:'fallback',cooldown_seconds:'30',honor_retry_after:false,backoff:false,max_cooldown_seconds:0});
  renderPolicy();
  refreshDirty();
  const rows=policyRulesEl.querySelectorAll('.policy-rule');
  const last=rows[rows.length-1];
  if(last)last.querySelector('[data-policy-field="status"]').focus();
}
function initializePolicyEvents(){
  if(!policyRulesEl)return;
  policyRulesEl.addEventListener('input',event=>updatePolicyFromInput(event.target));
  policyRulesEl.addEventListener('change',event=>updatePolicyFromInput(event.target));
  policyRulesEl.addEventListener('click',event=>{
    const button=event.target.closest('button[data-action="remove-rule"]');
    if(!button)return;
    const index=policyRuleIndexFrom(button);
    if(index<0||!state.errorPolicy)return;
    state.errorPolicy.rules.splice(index,1);
    renderPolicy();
    refreshDirty();
  });
  policyAddRuleEl.addEventListener('click',addPolicyRule);
  policyDefaultActionEl.addEventListener('change',()=>updatePolicyFromInput(policyDefaultActionEl));
  policyDefaultCooldownEl.addEventListener('input',()=>updatePolicyFromInput(policyDefaultCooldownEl));
  attemptTimeoutEl.addEventListener('input',()=>{state.attemptTimeout=attemptTimeoutEl.value;refreshDirty()});
}
