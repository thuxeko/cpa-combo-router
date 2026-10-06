const tabs=Array.from(document.querySelectorAll('.page-tab'));

applyHostTheme();
observeHostTheme();
initializeConfigurationActions();
initializeConfigurationEvents();
initializePolicyEvents();
tabs.forEach(tab=>{
  tab.addEventListener('click',()=>activateTab(tab.id.replace(/-tab$/,''),false));
  tab.addEventListener('keydown',handleTabKey);
});
window.addEventListener('resize',updateConfigurationActionsClearance);
document.addEventListener('keydown',event=>{
  if((event.ctrlKey||event.metaKey)&&event.key.toLowerCase()==='s'&&!workspaceEl.hidden){event.preventDefault();if(state.dirty)saveConfiguration()}
});
if(managementKey())loadConfiguration();
else showFallbackKeyInput('CPAMC chưa có khoá quản trị nào được lưu. Nhập khoá cho phiên trình duyệt này.');
