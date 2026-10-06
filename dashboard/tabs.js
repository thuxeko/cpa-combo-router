// Tab switching is shared by both panels. Each tab owns a panel element and the
// configuration footer must be re-measured whenever the visible panel changes.
function tabPanelPairs(){
  return [
    {tab:document.getElementById('configuration-tab'),panel:configurationPanelEl},
    {tab:document.getElementById('policy-tab'),panel:policyPanelEl}
  ];
}
function activateTab(name,moveFocus=false){
  tabPanelPairs().forEach(pair=>{
    if(!pair.tab||!pair.panel)return;
    const selected=pair.tab.id===name+'-tab';
    pair.tab.setAttribute('aria-selected',String(selected));
    pair.tab.tabIndex=selected?0:-1;
    pair.panel.hidden=!selected;
    if(selected&&moveFocus)pair.tab.focus();
  });
  updateConfigurationActionsClearance();
  refreshDirty();
}
function handleTabKey(event){
  const tabs=tabPanelPairs().map(pair=>pair.tab).filter(Boolean);
  const current=tabs.indexOf(event.currentTarget);
  let next=current;
  if(event.key==='ArrowRight')next=(current+1)%tabs.length;
  else if(event.key==='ArrowLeft')next=(current-1+tabs.length)%tabs.length;
  else if(event.key==='Home')next=0;
  else if(event.key==='End')next=tabs.length-1;
  else return;
  event.preventDefault();
  activateTab(tabs[next].id.replace(/-tab$/,''),true);
}
function activeTabName(){
  const selected=tabPanelPairs().find(pair=>pair.tab&&pair.tab.getAttribute('aria-selected')==='true');
  return selected?selected.tab.id.replace(/-tab$/,''):'configuration';
}
