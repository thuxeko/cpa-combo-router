// The live health panel reads the /health endpoint the Go side exposes. It is
// strictly read-only: it shows the runtime as it is, even while the
// configuration tab holds unsaved edits, and never writes back.
const HEALTH_URL='/v0/resource/plugins/combo-router/health';
const HEALTH_POLL_MS=10000;
const healthPanelEl=document.getElementById('health-panel');
const healthStatusEl=document.getElementById('health-status');
const healthRoutesEl=document.getElementById('health-routes');
const healthEmptyEl=document.getElementById('health-empty');
const healthDotEl=document.getElementById('health-dot');
const healthFootTextEl=document.getElementById('health-foot-text');
const healthRefreshEl=document.getElementById('health-refresh');
let healthPollTimer=null;

function healthFormatMillis(value){
  if(!Number.isFinite(value)||value<=0)return '—';
  if(value<1000)return `${Math.round(value)} ms`;
  return `${(value/1000).toFixed(1)} s`;
}
function healthFormatClock(value){
  if(!value)return '—';
  const date=new Date(value);
  if(Number.isNaN(date.getTime()))return '—';
  return date.toLocaleTimeString('vi-VN',{hour:'2-digit',minute:'2-digit',second:'2-digit'});
}
function healthBadge(metric){
  const badge=document.createElement('span');
  badge.className='badge';
  if(metric.degraded){
    badge.classList.add('badge-degraded');
    badge.textContent='đang kém';
  }else if(metric.attempts>0){
    badge.classList.add('badge-ok');
    badge.textContent='ổn';
  }else{
    badge.classList.add('badge-idle');
    badge.textContent='chưa có dữ liệu';
  }
  return badge;
}
function healthTone(metric,key){
  if(metric[key]>0)return key==='timeouts'?'is-warn':'is-bad';
  return '';
}
function renderHealth(snapshot){
  const routes=snapshot&&snapshot.routes?snapshot.routes:{};
  const routeKeys=Object.keys(routes);
  healthEmptyEl.hidden=routeKeys.length>0;
  healthRoutesEl.textContent='';
  const fragment=document.createDocumentFragment();
  routeKeys.sort().forEach(alias=>{
    const route=routes[alias];
    const targets=route&&route.targets?route.targets:{};
    const targetKeys=Object.keys(targets);
    const routeEl=document.createElement('section');
    routeEl.className='health-route';
    const head=document.createElement('div');
    head.className='health-route-head';
    const title=document.createElement('h3');
    title.textContent=alias;
    const strategy=document.createElement('span');
    strategy.className='health-route-strategy';
    strategy.textContent=`${targetKeys.length} đích`;
    head.append(title,strategy);
    routeEl.appendChild(head);
    if(targetKeys.length){
      const grid=document.createElement('div');
      grid.className='health-targets';
      targetKeys.sort().forEach(model=>{
        const metric=targets[model]||{};
        const card=document.createElement('div');
        card.className='health-target'+(metric.degraded?' is-degraded':'');
        const name=document.createElement('div');
        name.className='health-target-name';
        name.textContent=model;
        name.appendChild(healthBadge(metric));
        card.appendChild(name);
        const rows=[
          ['TTFB trung bình',healthFormatMillis(metric.ttfb_avg_ms),''],
          ['Byte đầu gần nhất',healthFormatMillis(metric.last_ttfb_ms),''],
          ['Thành công',String(metric.successes||0),''],
          ['Hết giờ chờ',String(metric.timeouts||0),healthTone(metric,'timeouts')],
          ['Lỗi khác',String(metric.failures||0),healthTone(metric,'failures')],
          ['Lần cuối',healthFormatClock(metric.last_seen),'']
        ];
        rows.forEach(pair=>{
          const row=document.createElement('div');
          row.className='health-target-metric'+(pair[2]?' '+pair[2]:'');
          row.textContent=pair[0]+': ';
          const bold=document.createElement('b');
          bold.textContent=pair[1];
          row.appendChild(bold);
          card.appendChild(row);
        });
        grid.appendChild(card);
      });
      routeEl.appendChild(grid);
    }else{
      const empty=document.createElement('p');
      empty.className='health-empty';
      empty.textContent='Tuyến chưa có chỉ số.';
      routeEl.appendChild(empty);
    }
    fragment.appendChild(routeEl);
  });
  healthRoutesEl.appendChild(fragment);
}
function healthSetStatus(tone,text){
  healthStatusEl.dataset.tone=tone;
  healthStatusEl.textContent=text;
  healthDotEl.dataset.tone=tone==='error'?'error':'ok';
}
async function refreshHealth(){
  try{
    const snapshot=await requestManagementJSON(HEALTH_URL);
    renderHealth(snapshot);
    healthSetStatus('ok','Cập nhật '+new Date().toLocaleTimeString('vi-VN',{hour:'2-digit',minute:'2-digit',second:'2-digit'}));
  }catch(error){
    healthSetStatus('error','Không tải được: '+(error&&error.message?error.message:String(error)));
  }
}
function initializeHealthEvents(){
  healthRefreshEl.addEventListener('click',refreshHealth);
  healthPollTimer=setInterval(()=>{
    // Poll only while the tab is visible; a hidden panel has no reader.
    if(!healthPanelEl.hidden)refreshHealth();
  },HEALTH_POLL_MS);
  if(managementKey())refreshHealth();
}