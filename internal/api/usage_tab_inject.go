package api

import "bytes"

const localUsageTabScript = `<script id="cpa-usage-tab">
(function(){
  if (window.__cpaUsageTabMounted) return;
  window.__cpaUsageTabMounted = true;
  var SS_PERIOD = 'cpa.usageTab.period';
  function el(tag, attrs, children){
    var e = document.createElement(tag);
    if (attrs){ for (var k in attrs){ if (k === 'style') Object.assign(e.style, attrs[k]); else e.setAttribute(k, attrs[k]); } }
    (Array.isArray(children) ? children : (children == null ? [] : [children])).forEach(function(c){ e.appendChild(typeof c === 'string' ? document.createTextNode(c) : c); });
    return e;
  }
  function fmtNum(n){ return (Number(n)||0).toLocaleString(); }
  function fmtCost(n){ n=Number(n)||0; return (n>0&&n<0.01?'~$'+n.toFixed(4):'~$'+n.toFixed(2)); }
  function fmtTime(s){ if(!s)return '-'; try{return new Date(s).toLocaleString()}catch(_){return s} }
  function row(cells, head){ return el('tr', null, cells.map(function(c){ return el(head?'th':'td', {style:{textAlign:c.align||'left',padding:'5px 8px',font:head?'600 10px sans-serif':'12px sans-serif',color:head?'#475569':'#0f172a',borderBottom:'1px solid #e2e8f0',whiteSpace:c.nowrap?'nowrap':'normal'}}, c.value); })); }
  function table(title, headers, rows, map){
    var t=el('table',{style:{width:'100%',borderCollapse:'collapse'}}); t.appendChild(row(headers,true));
    if(!rows||!rows.length)t.appendChild(el('tr',null,el('td',{colspan:String(headers.length),style:{padding:'12px',textAlign:'center',color:'#94a3b8',font:'12px sans-serif',borderBottom:'1px solid #e2e8f0'}},'No data yet.')));
    else rows.forEach(function(r){t.appendChild(row(map(r)))});
    return el('div',{style:{marginTop:'12px'}},[el('div',{style:{font:'600 12px sans-serif',color:'#0f172a',margin:'4px 0 6px'}},title),el('div',{style:{overflowX:'auto',border:'1px solid #e2e8f0',borderRadius:'8px'}},t)]);
  }
  function card(label,value,color){ return el('div',{style:{flex:'1 1 150px',minWidth:'140px',background:'#fff',border:'1px solid #e2e8f0',borderRadius:'10px',padding:'10px 12px',boxSizing:'border-box'}},[el('div',{style:{font:'600 10px sans-serif',color:'#64748b',textTransform:'uppercase',letterSpacing:'.05em'}},label),el('div',{style:{font:'700 20px sans-serif',color:color||'#0f172a',marginTop:'4px'}},value)]); }
  function mount(){
    if(document.getElementById('cpa-usage-panel'))return;
    var handle=el('button',{id:'cpa-usage-handle',title:'Toggle usage panel',style:{position:'fixed',right:'0',top:'50%',transform:'translateY(-50%)',zIndex:'2147483645',padding:'14px 8px',background:'#2563eb',color:'#fff',border:'none',borderRadius:'8px 0 0 8px',cursor:'pointer',boxShadow:'-2px 0 8px rgba(37,99,235,.25)',font:'600 12px sans-serif',writingMode:'vertical-rl',textOrientation:'mixed'}},'Usage');
    var period=el('select',{style:{padding:'5px 7px',borderRadius:'6px',border:'1px solid #ccc',font:'12px sans-serif'}});
    [['today','Today'],['24h','24h'],['7d','7D'],['30d','30D'],['60d','60D'],['all','All']].forEach(function(o){var x=el('option',{value:o[0]},o[1]); if(o[0]===(sessionStorage.getItem(SS_PERIOD)||'7d'))x.selected=true; period.appendChild(x);});
    var refresh=el('button',{style:{padding:'5px 10px',background:'#2563eb',color:'#fff',border:'none',borderRadius:'6px',cursor:'pointer',font:'12px sans-serif'}},'Refresh');
    var close=el('button',{style:{padding:'5px 8px',background:'transparent',color:'#64748b',border:'none',cursor:'pointer',font:'600 16px sans-serif',marginLeft:'auto'}},'×');
    var status=el('div',{style:{whiteSpace:'pre-wrap',font:'11px/1.4 ui-monospace,Menlo,Consolas,monospace',color:'#dc2626',padding:'8px 12px',display:'none',background:'#fef2f2',borderBottom:'1px solid #fecaca'}});
    var overview=el('div',{style:{display:'flex',flexWrap:'wrap',gap:'8px',padding:'12px'}}), tables=el('div',{style:{padding:'0 12px 16px'}});
    var panel=el('div',{id:'cpa-usage-panel',style:{position:'fixed',right:'0',top:'0',bottom:'0',width:'520px',maxWidth:'100vw',background:'#fff',borderLeft:'1px solid #e2e8f0',boxShadow:'-8px 0 24px rgba(15,23,42,.08)',zIndex:'2147483644',display:'none',flexDirection:'column',transform:'translateX(100%)',transition:'transform .2s ease'}},[el('div',{style:{display:'flex',alignItems:'center',gap:'8px',padding:'10px 12px',borderBottom:'1px solid #e2e8f0',background:'#f8fafc'}},[el('div',{style:{font:'600 14px sans-serif',color:'#0f172a'}},'Usage'),el('div',{style:{flex:'1'}},''),period,refresh,close]),status,el('div',{style:{flex:'1',overflowY:'auto',overflowX:'hidden'}},[overview,tables])]);
    function render(j){ overview.innerHTML=''; tables.innerHTML=''; overview.appendChild(card('Total Requests',fmtNum(j.totalRequests),'#0f172a')); overview.appendChild(card('Input Tokens',fmtNum(j.totalPromptTokens),'#2563eb')); overview.appendChild(card('Output Tokens',fmtNum(j.totalCompletionTokens),'#16a34a')); overview.appendChild(card('Est. Cost',fmtCost(j.totalCost),'#d97706'));
      tables.appendChild(table('By Model',[{value:'Provider'},{value:'Model'},{value:'Req',align:'right'},{value:'In',align:'right'},{value:'Out',align:'right'},{value:'Cost',align:'right'}],j.byModel||[],function(r){return[{value:r.provider||'-'},{value:r.model||'-'},{value:fmtNum(r.requests),align:'right'},{value:fmtNum(r.promptTokens),align:'right'},{value:fmtNum(r.completionTokens),align:'right'},{value:fmtCost(r.cost),align:'right'}]}));
      tables.appendChild(table('By Provider',[{value:'Provider'},{value:'Req',align:'right'},{value:'In',align:'right'},{value:'Out',align:'right'},{value:'Cost',align:'right'}],j.byProvider||[],function(r){return[{value:r.provider||'-'},{value:fmtNum(r.requests),align:'right'},{value:fmtNum(r.promptTokens),align:'right'},{value:fmtNum(r.completionTokens),align:'right'},{value:fmtCost(r.cost),align:'right'}]}));
      tables.appendChild(table('Recent',[{value:'When',nowrap:true},{value:'Model'},{value:'In',align:'right'},{value:'Out',align:'right'},{value:'Cost',align:'right'},{value:'Status'}],(j.recentRequests||[]).slice(0,25),function(r){return[{value:fmtTime(r.timestamp),nowrap:true},{value:r.model||'-'},{value:fmtNum(r.promptTokens),align:'right'},{value:fmtNum(r.completionTokens),align:'right'},{value:fmtCost(r.cost),align:'right'},{value:r.status||'-'}]})); }
    async function load(){ sessionStorage.setItem(SS_PERIOD,period.value); status.style.display='none'; refresh.disabled=true; refresh.textContent='...'; try{ var res=await fetch('/v0/usage-stats?period='+encodeURIComponent(period.value)); var text=await res.text(); if(!res.ok){status.style.display='block'; status.textContent='HTTP '+res.status+' '+res.statusText+'\n'+text.slice(0,400); return;} render(JSON.parse(text)); }catch(e){status.style.display='block'; status.textContent='Request failed: '+(e&&e.message?e.message:String(e));}finally{refresh.disabled=false; refresh.textContent='Refresh';} }
    function open(v){ if(v){panel.style.display='flex'; requestAnimationFrame(function(){panel.style.transform='translateX(0)'}); handle.style.right='520px'; load();} else {panel.style.transform='translateX(100%)'; setTimeout(function(){panel.style.display='none'},200); handle.style.right='0';} }
    handle.addEventListener('click',function(){open(panel.style.display!=='flex')}); close.addEventListener('click',function(){open(false)}); refresh.addEventListener('click',load); period.addEventListener('change',load); document.body.appendChild(handle); document.body.appendChild(panel);
  }
  if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',mount); else mount();
})();
</script>`

func injectLocalUsageTab(html []byte) []byte {
	if bytes.Contains(html, []byte(`id="cpa-usage-tab"`)) || bytes.Contains(html, []byte(`id='cpa-usage-tab'`)) {
		return html
	}
	idx := bytes.LastIndex(bytes.ToLower(html), []byte("</body>"))
	if idx < 0 {
		out := make([]byte, 0, len(html)+len(localUsageTabScript))
		out = append(out, html...)
		out = append(out, localUsageTabScript...)
		return out
	}
	out := make([]byte, 0, len(html)+len(localUsageTabScript))
	out = append(out, html[:idx]...)
	out = append(out, localUsageTabScript...)
	out = append(out, html[idx:]...)
	return out
}
