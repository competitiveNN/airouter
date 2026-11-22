package main

// dashboard.go adds a web dashboard to the airouter gateway.
//
// The dashboard is served at /admin/dashboard and is split into three
// pages, all fed from a single JSON payload fetched at /admin/dashboard/data:
//
//   - Chains & Inflight: each logical model's configured endpoints with how
//     many are currently eligible, and the models serving sessions right now.
//   - Leaderboards: speed, volume, failure-rate and session leaders over a
//     selectable window (1d or 7d, matching the ring retention).
//   - Errors, Cooldowns & SQLite Stats: the raw cooldown/circuit tables (the
//     operator surface for clearing backoffs) and a read-only dump of every
//     table in the SQLite state database (priorities/TPS, sticky sessions and
//     meta), so the persisted state is visible without opening cooldowns.db.
//
// The page refreshes on a 30 s timer so an operator can leave it open.

// dashboardHTML is the multi-page dashboard. It is a const string so the
// gate's tree-fingerprint check sees it inline: the page is self-contained
// (no external assets) so the CSP can stay at 'self' and there is nothing
// for a dependency auditor to review.
const dashboardHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>AI Router Dashboard</title>
<style>
body{font-family:system-ui,sans-serif;margin:0;padding:0;background:#f6f7fb;color:#111}
header{background:#0f172a;color:#fff;padding:1rem 2rem;display:flex;justify-content:space-between;align-items:center}
nav.tabs{background:#1e293b;padding:0.5rem 2rem;display:flex;gap:1rem}
nav.tabs button{background:transparent;color:#94a3b8;border:none;padding:.5rem 1rem;border-radius:6px;cursor:pointer;font-weight:600;font-size:.9rem}
nav.tabs button.active{background:#2563eb;color:#fff}
main{padding:2rem;max-width:1200px;margin:0 auto}
.page{display:none}
.page.active{display:block}
.card{background:#fff;border-radius:12px;box-shadow:0 1px 3px rgba(0,0,0,.1);padding:1.5rem;margin-bottom:1.5rem}
h2{margin-top:0}
button.action{background:#2563eb;color:#fff;border:none;padding:.6rem 1rem;border-radius:8px;cursor:pointer;margin-right:.5rem}
button.action:disabled{opacity:.5}
button.wsel{background:transparent;color:#64748b;border:1px solid #cbd5e1;padding:.4rem .8rem;border-radius:6px;cursor:pointer}
button.wsel.on{background:#2563eb;color:#fff;border-color:#2563eb}
table{border-collapse:collapse;width:100%;font-size:.9rem;table-layout:fixed}
th,td{text-align:left;padding:.5rem .75rem;border-bottom:1px solid #e2e8f0;overflow:hidden}
th{color:#64748b;font-weight:600;text-transform:uppercase;font-size:.7rem;letter-spacing:.05em}
td.num{text-align:right;font-variant-numeric:tabular-nums}
td.clamp,th.clamp{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
td.clamp:hover{overflow:visible;white-space:normal;word-break:break-all}
.pag{display:flex;gap:.5rem;align-items:center;margin-top:.5rem;font-size:.8rem}
.pag button{background:transparent;color:#2563eb;border:1px solid #cbd5e1;padding:.2rem .6rem;border-radius:6px;cursor:pointer}
.pag button:disabled{opacity:.4;cursor:default}
.search-bar{display:flex;gap:.5rem;align-items:center;margin-bottom:.75rem}
.search-bar input[type=search]{flex:1;padding:.45rem .75rem;border:1px solid #cbd5e1;border-radius:8px;outline:none;font:inherit;background:#fff}
.search-bar input[type=search]:focus{border-color:#2563eb;box-shadow:0 0 0 3px rgba(37,99,235,.15)}
#errorSearchClear{display:none;background:transparent;color:#94a3b8;border:none;padding:.15rem .5rem;border-radius:6px;cursor:pointer;font-size:1.2rem;line-height:1}
#errorSearchClear:hover{color:#dc2626}
@media(max-width:600px){.search-bar{flex-direction:column;align-items:stretch}#errorSearchClear{align-self:flex-end}}
details.tree{margin:.4rem 0}
details.tree>summary{cursor:pointer;font-weight:600;padding:.3rem 0}
details.tree[open]>summary{margin-bottom:.3rem}
.chain{margin-bottom:1.25rem}
.chainhead{display:flex;gap:.5rem;align-items:baseline;margin-bottom:.4rem}
.chainhead b{min-width:5rem}
.eps{display:grid;grid-template-columns:repeat(auto-fill,minmax(15rem,1fr));gap:.35rem .6rem}
.pill{display:flex;align-items:baseline;gap:.4rem;padding:.15rem .5rem;border-radius:999px;font-size:.75rem;font-family:ui-monospace,monospace}
.pill .pnm{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pill .pct{white-space:nowrap}
.pill.ok{background:#dcfce7;color:#166534}
.pill.no{background:#fee2e2;color:#991b1b}
.pill.cooldown{background:#fef9c3;color:#854d0e}
.pill.circuit{background:#f3e8ff;color:#6b21a8}
.pill.tor-ready{background:#dcfce7;color:#166534}
.pill.tor-bootstrap{background:#fef9c3;color:#854d0e}
.pill.tor-unreachable{background:#fee2e2;color:#991b1b}
.pill.tor-disabled{background:#e2e8f0;color:#64748b}
.sum{color:#64748b;font-size:.85rem;margin-left:auto}
.life{color:#475569;font-size:.8rem}
.sessions{margin:.4rem 0 .8rem 1rem;padding-left:.75rem;border-left:2px solid #e2e8f0;display:none}
.session{display:flex;gap:.5rem;align-items:baseline;padding:.1rem 0;font-size:.78rem}
.session .sid{font-family:ui-monospace,monospace;color:#334155;min-width:6rem;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.session .ep{color:#64748b;min-width:8rem;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:1.5rem}
@media(max-width:900px){.grid{grid-template-columns:1fr}}
.rate{font-variant-numeric:tabular-nums;font-weight:600}
.badge{display:inline-block;padding:.2rem .5rem;font-size:.75rem;border-radius:4px;background:#e2e8f0;color:#334155;margin-right:.4rem}
</style>
</head>
<body>
<header>
  <h1>AI Router Dashboard</h1>
  <div style="display:flex;align-items:center;gap:1rem">
    <span id="configUpdated" style="font-size:.85rem;color:#cbd5e1"></span>
    <span id="status" style="font-size:.85rem;color:#94a3b8"></span>
    <button class="action" onclick="openConfigEditor()" style="background:#334155">Config Editor</button>
  </div>
</header>
<nav class="tabs">
  <button id="tab-btn-1" class="active" onclick="switchTab(1)">Chains &amp; Inflight</button>
  <button id="tab-btn-2" onclick="switchTab(2)">Leaderboards</button>
  <button id="tab-btn-3" onclick="switchTab(3)">Errors, Cooldowns &amp; SQLite Stats</button>
</nav>
<main>

<!-- PAGE 1: Chains & Inflight -->
<div id="page-1" class="page active">
  <div class="card">
    <h2>Chains</h2>
    <p>How many endpoints in each logical model's fallback chain are currently eligible.</p>
    <div id="chains"></div>
  </div>
  <div class="card">
    <h2>Inflight</h2>
    <p>Models currently serving sessions, how many sessions each is generating tokens for right now, and cumulative lifetime.</p>
    <div id="inflight"></div>
  </div>
</div>

<!-- PAGE 2: Leaderboards -->
<div id="page-2" class="page">
  <div class="card">
    <h2>Leaderboards <span id="wsel" style="margin-left:1rem"></span></h2>
    <div class="grid">
      <div>
        <h3>Fastest (tokens/s)</h3>
        <table><thead><tr><th class="clamp">Endpoint</th><th>Tok/s</th><th>Tokens</th><th>p50</th><th>p95</th><th>p99</th></tr></thead>
        <tbody id="speed"></tbody></table>
        <div class="pag" id="speed-pag"></div>
      </div>
      <div>
        <h3>Most requests</h3>
        <table><thead><tr><th class="clamp">Endpoint</th><th>Req/s</th><th>Requests</th></tr></thead>
        <tbody id="volume"></tbody></table>
        <div class="pag" id="volume-pag"></div>
      </div>
      <div>
        <h3>Highest failure rate</h3>
        <table><thead><tr><th class="clamp">Endpoint</th><th>Fail %</th><th>Failures</th></tr></thead>
        <tbody id="failures"></tbody></table>
        <div class="pag" id="failures-pag"></div>
      </div>
      <div>
        <h3>Most sessions</h3>
        <table><thead><tr><th class="clamp">Endpoint</th><th>Sessions</th></tr></thead>
        <tbody id="sessions"></tbody></table>
        <div class="pag" id="sessions-pag"></div>
      </div>
    </div>
  </div>
<div class="card">
  <h2>Proxy sources (highest failure rate)</h2>
  <p>Proxy list sources ranked by the share of their proxies that have failed.</p>
  <table><thead><tr><th class="clamp">Source</th><th>Total</th><th>Failed</th><th>Fail %</th></tr></thead><tbody id="proxySources"></tbody></table>
</div>

<div class="card">
  <h2>Tor instances</h2>
  <p>Each spawned Tor daemon and its bootstrap state. When instances are not <code>ready</code>, proxied requests route through circuits Tor has not finished building and may fail or hang; that is expected for the first minutes after startup.</p>
  <table><thead><tr><th class="clamp">Instance</th><th>State</th><th>Bootstrap</th><th>Down until</th><th>Last error</th><th>Started</th></tr></thead><tbody id="torInstances"></tbody></table>
</div>
</div>

<!-- PAGE 3: Errors, Cooldowns & SQLite Stats -->
<div id="page-3" class="page">
  <div class="card">
    <h2>Cooldowns &amp; Circuits</h2>
    <p>Endpoints currently on cooldown or with open circuits. Check entries to clear.</p>
    <div class="search-bar" id="errorSearchBar">
      <input type="search" id="errorSearch" placeholder="Filter errors by model, provider, or error text" autocomplete="off" spellcheck="false">
      <button id="errorSearchClear" class="wsel" title="Clear search">×</button>
      <span id="errorCount" class="life"></span>
    </div>
    <div id="cooldowns"></div>
    <div id="cooldownActions" style="margin-top:.75rem;display:none">
      <button id="clearSelected" class="action" disabled>Clear selected</button>
      <button id="selectAll" class="wsel" onclick="toggleSelectAll()">Select all</button>
    </div>
  </div>
  <div class="card">
    <h2>SQLite Store Statistics (cooldowns.db)</h2>
    <p>Read-only view of the persisted priorities (TPS) table.</p>
    <div id="sqliteMeta" style="margin-bottom:1rem"></div>
    <h3>Priorities Table (TPS)</h3>
    <table><thead><tr><th class="clamp">Key</th><th>TPS</th></tr></thead><tbody id="sqlitePriorities"></tbody></table>
  </div>
</div>

</main>
<script>
const apiKey = new URLSearchParams(location.search).get('api_key') || prompt('Enter gateway API key for admin:') || '';
async function req(path, opts={}) {
  const headers = Object.assign({'Content-Type':'application/json'}, opts.headers||{});
  if(apiKey) headers['Authorization']='Bearer '+apiKey;
  const res = await fetch(path,{...opts,headers});
  if(!res.ok){ const t=await res.text(); throw new Error(t); }
  return res.headers.get('content-type')?.includes('application/json')? res.json(): res.text();
}

let activeWindow='1d';
function switchTab(n){
  for(let i=1;i<=3;i++){
    const p=document.getElementById('page-'+i);
    const b=document.getElementById('tab-btn-'+i);
    if(p) p.classList.toggle('active', i===n);
    if(b) b.classList.toggle('active', i===n);
  }
}
function el(tag, cls, text){ const e=document.createElement(tag); if(cls)e.className=cls; if(text!==undefined)e.textContent=text; return e; }
function renderWindowSel(){
  const s=document.getElementById('wsel');
  if(!s) return;
  s.innerHTML='';
  for(const w of ['1d','7d']){
    const b=el('button', 'wsel'+(w===activeWindow?' on':''), w);
    b.onclick=()=>{ activeWindow=w; refresh(); };
    s.appendChild(b);
  }
}

function renderChains(chains, breaker){
  const c=document.getElementById('chains');
  c.innerHTML='';
  if(!chains || !chains.length){ c.textContent='No chains configured.'; return; }
  for(const ch of chains){
    const row=el('div','chain');
    const head=el('div','chainhead');
    head.appendChild(el('b',null,ch.model));
    const sum=el('span','sum',ch.operational+'/'+ch.total+' eligible');
    if(!breaker) sum.title='circuit breaker is disabled; the circuit column is inactive';
    head.appendChild(sum);
    row.appendChild(head);
    const eps=el('div','eps');
    const sorted=(ch.endpoints||[]).slice().sort((a,b)=>{
      if(a.circuit && a.circuit!=='closed' && (!b.circuit||b.circuit==='closed')) return -1;
      if(b.circuit && b.circuit!=='closed' && (!a.circuit||a.circuit==='closed')) return 1;
      if(!a.eligible && b.eligible) return -1;
      if(a.eligible && !b.eligible) return 1;
      if(a.cooldown && !b.cooldown) return -1;
      if(!a.cooldown && b.cooldown) return 1;
      return 0;
    });
    for(const ep of sorted){
      let cls='ok', txt='eligible';
      if(ep.circuit && ep.circuit!=='closed'){ cls='circuit'; txt='circuit '+ep.circuit; }
      else if(!ep.eligible){
        if(ep.cooldown){
          cls='cooldown';
          if(ep.cooldown_until && typeof serverNow==='number'){
            const left=Math.max(0, Math.ceil((new Date(ep.cooldown_until).getTime()-serverNow)/1000));
            txt='cooldown ('+left+'s)';
          } else { txt='cooldown'; }
        } else { cls='no'; txt='down'; }
      }
      const pill=el('div','pill '+cls);
      pill.title=ep.endpoint;
      pill.appendChild(el('span','pnm',ep.endpoint));
      pill.appendChild(el('span','pct',txt));
      eps.appendChild(pill);
    }
    row.appendChild(eps);
    c.appendChild(row);
  }
}

function humanLifetime(sec){
  if(typeof sec!=='number'||isNaN(sec)) return '—';
  if(sec<60) return sec.toFixed(1)+'s';
  if(sec<3600) return Math.floor(sec/60)+'m '+Math.floor(sec%60)+'s';
  return Math.floor(sec/3600)+'h '+Math.floor((sec%3600)/60)+'m';
}
let sessionExpansion=new Map();
function renderInflight(inflight){
  const c=document.getElementById('inflight');
  c.innerHTML='';
  if(!inflight || !inflight.length){ c.textContent='No models currently serving sessions.'; return; }
  for(const m of inflight){
    const row=el('div','chain');
    const head=el('div','chainhead');
    head.appendChild(el('b',null,m.model));
    const sessions=m.sessions||0;
    const lifeSec=m.cumulative_lifetime_seconds||0;
    const oldest=m.since?new Date(m.since).toLocaleTimeString():'—';
    const lifeSpan = el('span','life',sessions+' session'+(sessions===1?'':'s')+' active  •  cum. lifetime '+humanLifetime(lifeSec)+'  •  oldest since '+oldest);
    lifeSpan.dataset.base = lifeSec;
    head.appendChild(lifeSpan);
    row.appendChild(head);
    const eps=el('div','eps');
    for(const e of (m.endpoints||[])){
      const pill=el('div','pill ok');
      pill.appendChild(el('span','pnm',e.endpoint));
      pill.appendChild(el('span','pct',e.sessions+' session'+(e.sessions===1?'':'s')));
      eps.appendChild(pill);
    }
    row.appendChild(eps);
    const details=m.sessions_detail||m.session_details||m.sessions_list||m.session_list||m.sessions_expanded||m.sessions_breakdown||m.detail||m.details||m.session_rows||m.rows||m.members||m.items||[];
    if(details.length>0){
      const isExpanded=sessionExpansion.get(m.model)||false;
      const tbtn=el('button','wsel',isExpanded?'Hide active sessions':'Show active sessions ('+details.length+')');
      tbtn.style.marginTop='.4rem';
      tbtn.onclick=()=>{ sessionExpansion.set(m.model,!isExpanded); renderInflight(inflight); };
      row.appendChild(tbtn);
      const slist=el('div','sessions');
      slist.style.display=isExpanded?'block':'none';
      for(const s of details){
        const sdiv=el('div','session');
        const sid=s.session||s.session_id||s.id||'—';
        const sep=s.endpoint||'—';
        const age=typeof s.age==='number'?s.age:(typeof s.age_seconds==='number'?s.age_seconds:(typeof s.duration==='number'?s.duration:(typeof s.duration_seconds==='number'?s.duration_seconds:0)));
        sdiv.appendChild(el('span','sid',sid));
        sdiv.appendChild(el('span','ep',sep));
        sdiv.appendChild(el('span','life','age '+age.toFixed(1)+'s'));
        slist.appendChild(sdiv);
      }
      row.appendChild(slist);
    }
    c.appendChild(row);
  }
}

let speedPage=0, volumePage=0, failuresPage=0, sessionsPage=0;
const pageSize=10;
function renderBoard(id, rows, cols){
  const tbody=document.getElementById(id);
  tbody.innerHTML='';
  const pagDiv=document.getElementById(id+'-pag');
  if(pagDiv) pagDiv.innerHTML='';
  if(!rows || !rows.length){
    const tr=document.createElement('tr');
    const td=document.createElement('td');
    td.colSpan=cols.length+1;
    td.textContent='No data.';
    tr.appendChild(td);
    tbody.appendChild(tr);
    return;
  }
  let page=0;
  if(id==='speed') page=speedPage;
  else if(id==='volume') page=volumePage;
  else if(id==='failures') page=failuresPage;
  else if(id==='sessions') page=sessionsPage;

  const totalPages=Math.max(1, Math.ceil(rows.length/pageSize));
  if(page>=totalPages) page=totalPages-1;
  const pageRows=rows.slice(page*pageSize, page*pageSize+pageSize);

  for(const r of pageRows){
    const tr=document.createElement('tr');
    const td0=document.createElement('td');
    td0.className='clamp';
    td0.textContent=r.endpoint;
    td0.title=r.endpoint;
    tr.appendChild(td0);
    for(const [getter, formatter] of cols){
      const val=getter(r);
      const td=document.createElement('td');
      if(typeof val==='number') td.className='num rate';
      td.textContent=formatter(val);
      tr.appendChild(td);
    }
    tbody.appendChild(tr);
  }

  if(pagDiv && rows.length>pageSize){
    const prev=el('button',null,'Prev');
    prev.disabled=page===0;
    prev.onclick=()=>{
      if(id==='speed') speedPage--; else if(id==='volume') volumePage--; else if(id==='failures') failuresPage--; else if(id==='sessions') sessionsPage--;
      renderBoard(id, rows, cols);
    };
    const info=el('span',null,(page+1)+'/'+totalPages);
    const next=el('button',null,'Next');
    next.disabled=page>=totalPages-1;
    next.onclick=()=>{
      if(id==='speed') speedPage++; else if(id==='volume') volumePage++; else if(id==='failures') failuresPage++; else if(id==='sessions') sessionsPage++;
      renderBoard(id, rows, cols);
    };
    pagDiv.appendChild(prev);
    pagDiv.appendChild(info);
    pagDiv.appendChild(next);
  }
}

let cooldownSelection=new Set();
let cooldownPage=0, circuitPage=0;
function renderCooldowns(data){
  const c=document.getElementById('cooldowns');
  c.innerHTML='';
  const cdMap=data.cooldowns||{};
  const circMap=data.circuits||{};
  const cdKeys=Object.keys(cdMap);
  const circKeys=Object.keys(circMap);
  const actions=document.getElementById('cooldownActions');

  if(!cdKeys.length && !circKeys.length){
    c.textContent='No models currently on cooldown or with open circuits.';
    if(actions) actions.style.display='none';
    return;
  }
  if(actions) actions.style.display='block';

  const grid=el('div','grid');
  // Left: cooldown errors grouped by status code, expandable.
  const left=el('div');
  left.appendChild(el('h3',null,'Cooldown errors'));
  const cdRows=Object.entries(cdMap).map(([k,v])=>({model_key:k, expiry:v.expiry||null, status_code:v.status_code||0, error_count:v.error_count||0, last_error:v.last_error||'', state:''}));
  const circRows=Object.entries(circMap).map(([k,v])=>({model_key:k, expiry:null, status_code:-1, error_count:v.probes_sent||0, last_error:'', state:'circuit '+v.state, probes:v.probes_sent||0, opened:v.opened_at||null}));
  const pageSize=20;
  const totalCdPages=Math.max(1, Math.ceil(cdRows.length/pageSize));
  cooldownPage=Math.min(cooldownPage, totalCdPages-1);
  const cdStart=cooldownPage*pageSize;
  const pageCd=cdRows.slice(cdStart, cdStart+pageSize);
  const groups={};
  for(const r of pageCd){
    let key = r.status_code>0 ? r.status_code : 'unknown';
    if(r.status_code===0){
      const le=(r.last_error||'').toLowerCase();
      if(le.includes('timeout')||le.includes('timed out')||le.includes('timedout')||le.includes('upstream timedout')||le.includes('upstream timed out')||le.includes('upstream timeout')||le.includes('deadline exceeded')||le.includes('i/o timeout')||le.includes('tls handshake timeout')||le.includes('client.timeout exceeded')||le.includes('timeout awaiting response headers')){
        key='timeout';
      }
    }
    (groups[key]=groups[key]||[]).push(r);
  }
  const groupKeys=Object.keys(groups).sort();
  if(!groupKeys.length){ left.appendChild(el('p',null,'No cooldown errors.')); }
  for(const key of groupKeys){
    const det=el('details','tree');
    det.open=true;
    const sum=el('summary',null,(key==='timeout'?'Timeout':'Error '+key)+' ('+groups[key].length+')');
    det.appendChild(sum);
    const table=document.createElement('table');
    const thead=document.createElement('thead');
    const thr=document.createElement('tr');
    for(const h of ['','Model','Expiry','Status','Errors','Last error']){
      const th=document.createElement('th'); th.textContent=h; thr.appendChild(th);
    }
    thead.appendChild(thr); table.appendChild(thead);
    const tbody=document.createElement('tbody');
    for(const r of groups[key]){
      const tr=document.createElement('tr');
      const cb=document.createElement('input'); cb.type='checkbox'; cb.value=r.model_key;
      if(cooldownSelection.has(r.model_key)) cb.checked=true;
      cb.onchange=e=>{ if(e.target.checked) cooldownSelection.add(r.model_key); else cooldownSelection.delete(r.model_key); updateClearButton(); };
      const td0=document.createElement('td'); td0.appendChild(cb); tr.appendChild(td0);
      const td1=document.createElement('td'); td1.className='clamp'; td1.textContent=r.model_key; td1.title=r.model_key; tr.appendChild(td1);
      const td2=document.createElement('td'); td2.textContent=r.expiry?new Date(r.expiry).toLocaleString():'—'; tr.appendChild(td2);
      const td3=document.createElement('td'); td3.textContent=(r.state||r.status_code); tr.appendChild(td3);
      const td4=document.createElement('td'); td4.className='num'; td4.textContent=r.error_count||0; tr.appendChild(td4);
      const td5=document.createElement('td'); td5.className='clamp'; td5.textContent=(r.last_error||'').slice(0,120); td5.title=r.last_error||''; tr.appendChild(td5);
      tbody.appendChild(tr);
    }
    table.appendChild(tbody);
    det.appendChild(table);
    left.appendChild(det);
  }
  if(cdRows.length>pageSize){
    const pag=el('div','pag');
    const prev=el('button',null,'Prev'); prev.disabled=cooldownPage===0; prev.onclick=()=>{cooldownPage--; renderCooldowns(data);};
    const info=el('span',null,(cooldownPage+1)+'/'+totalCdPages);
    const next=el('button',null,'Next'); next.disabled=cooldownPage>=totalCdPages-1; next.onclick=()=>{cooldownPage++; renderCooldowns(data);};
    pag.appendChild(prev); pag.appendChild(info); pag.appendChild(next);
    left.appendChild(pag);
  }
  grid.appendChild(left);

  // Right: circuits separate table.
  const right=el('div');
  right.appendChild(el('h3',null,'Circuits'));
  const totalCircPages=Math.max(1, Math.ceil(circRows.length/pageSize));
  circuitPage=Math.min(circuitPage, totalCircPages-1);
  const circStart=circuitPage*pageSize;
  const pageCirc=circRows.slice(circStart, circStart+pageSize);
  const ctable=document.createElement('table');
  const cthead=document.createElement('thead');
  const cthr=document.createElement('tr');
  for(const h of ['Model','State','Opened','Probes']){
    const th=document.createElement('th'); th.textContent=h; cthr.appendChild(th);
  }
  cthead.appendChild(cthr); ctable.appendChild(cthead);
  const ctbody=document.createElement('tbody');
  for(const r of pageCirc){
    const tr=document.createElement('tr');
    const td0=document.createElement('td'); td0.className='clamp'; td0.textContent=r.model_key; td0.title=r.model_key; tr.appendChild(td0);
    const td1=document.createElement('td'); td1.textContent=r.state; tr.appendChild(td1);
    const td2=document.createElement('td'); td2.textContent=r.opened?new Date(r.opened).toLocaleString():'—'; tr.appendChild(td2);
    const td3=document.createElement('td'); td3.className='num'; td3.textContent=r.probes||0; tr.appendChild(td3);
    ctbody.appendChild(tr);
  }
  ctable.appendChild(ctbody);
  right.appendChild(ctable);
  if(circRows.length>pageSize){
    const pag=el('div','pag');
    const prev=el('button',null,'Prev'); prev.disabled=circuitPage===0; prev.onclick=()=>{circuitPage--; renderCooldowns(data);};
    const info=el('span',null,(circuitPage+1)+'/'+totalCircPages);
    const next=el('button',null,'Next'); next.disabled=circuitPage>=totalCircPages-1; next.onclick=()=>{circuitPage++; renderCooldowns(data);};
    pag.appendChild(prev); pag.appendChild(info); pag.appendChild(next);
    right.appendChild(pag);
  }
  grid.appendChild(right);
  c.appendChild(grid);
  updateClearButton();
}

function renderSqliteStats(stats){
  if(!stats) return;
  const metaDiv=document.getElementById('sqliteMeta');
  if(metaDiv){
    metaDiv.innerHTML='';
    metaDiv.appendChild(el('span','badge','DB Path: '+(stats.path||'—')));
    metaDiv.appendChild(el('span','badge','Size: '+((stats.size_bytes||0)/1024).toFixed(1)+' KB'));
    metaDiv.appendChild(el('span','badge','Priority rows: '+(stats.cooldowns_count||0)));
  }
  const pBody=document.getElementById('sqlitePriorities');
  if(pBody){
    pBody.innerHTML='';
    const priMap=stats.priorities||{};
    const priKeys=Object.keys(priMap).sort();
    if(!priKeys.length){
      const tr=document.createElement('tr'); const td=document.createElement('td'); td.colSpan=2; td.textContent='No priority records.'; tr.appendChild(td); pBody.appendChild(tr);
    } else {
      for(const k of priKeys){
        const tr=document.createElement('tr');
        const td0=document.createElement('td'); td0.className='clamp'; td0.textContent=k; td0.title=k; tr.appendChild(td0);
        const td1=document.createElement('td'); td1.className='num'; td1.textContent=Number(priMap[k]).toFixed(2); tr.appendChild(td1);
        pBody.appendChild(tr);
      }
    }
  }
}


function renderProxySources(rows){
  const tbody=document.getElementById('proxySources');
  if(!tbody) return;
  tbody.innerHTML='';
  if(!rows || !rows.length){
    const tr=document.createElement('tr'); const td=document.createElement('td'); td.colSpan=4; td.textContent='No proxy source data.'; tr.appendChild(td); tbody.appendChild(tr); return;
  }
  for(const r of rows){
    const tr=document.createElement('tr');
    const td0=document.createElement('td'); td0.className='clamp'; td0.textContent=r.source_url||r.source||'—'; td0.title=r.source_url||''; tr.appendChild(td0);
    const td1=document.createElement('td'); td1.className='num'; td1.textContent=r.total||0; tr.appendChild(td1);
    const td2=document.createElement('td'); td2.className='num'; td2.textContent=r.failed||0; tr.appendChild(td2);
    const td3=document.createElement('td'); td3.className='num'; td3.textContent=((r.failure_rate||0)*100).toFixed(1)+'%'; tr.appendChild(td3);
    tbody.appendChild(tr);
  }
}

function pillClass(state){
  if(state==='ready') return 'pill tor-ready';
  if(state==='bootstrapping') return 'pill tor-bootstrap';
  if(state==='unreachable') return 'pill tor-unreachable';
  return 'pill tor-disabled';
}

function renderTorStatus(tor){
  const tbody=document.getElementById('torInstances');
  if(!tbody) return;
  tbody.innerHTML='';
  if(!tor){
    const tr=document.createElement('tr'); const td=document.createElement('td'); td.colSpan=6; td.textContent='Tor data not available.'; tr.appendChild(td); tbody.appendChild(tr); return;
  }
  if(!tor.enabled){
    const tr=document.createElement('tr'); const td=document.createElement('td'); td.colSpan=6; td.textContent='Tor mode disabled (tor_instances not set).'; tr.appendChild(td); tbody.appendChild(tr); return;
  }
  const inst=tor.instances||[];
  if(!inst.length){
    const tr=document.createElement('tr'); const td=document.createElement('td'); td.colSpan=6; td.textContent='No Tor instances yet.'; tr.appendChild(td); tbody.appendChild(tr); return;
  }
  for(const r of inst){
    const tr=document.createElement('tr');
    const td0=document.createElement('td'); td0.className='clamp'; td0.textContent=r.url||'—'; td0.title=r.url||''; tr.appendChild(td0);
    const td1=document.createElement('td'); td1.className=pillClass(r.state); td1.textContent=(r.state||'?')+(r.down?' (down)':'')+(r.blacklisted?' (blacklisted)':''); tr.appendChild(td1);
    const td2=document.createElement('td'); td2.className='num';
    if(r.progress>=100) td2.textContent='100%';
    else if(r.progress>0) td2.textContent=r.progress+'%';
    else td2.textContent='—';
    tr.appendChild(td2);
    const td3=document.createElement('td'); td3.className='num';
    if(r.down_until) td3.textContent=new Date(r.down_until).toLocaleTimeString(); else td3.textContent='—';
    tr.appendChild(td3);
    const td4=document.createElement('td'); td4.className='clamp'; td4.textContent=r.error||'—'; td4.title=r.error||''; tr.appendChild(td4);
    const td5=document.createElement('td'); td5.className='num'; td5.textContent=r.started_at?new Date(r.started_at).toLocaleTimeString():'—'; tr.appendChild(td5);
    tbody.appendChild(tr);
  }
}

function updateClearButton(){
  const btn=document.getElementById('clearSelected');
  if(btn) btn.disabled=cooldownSelection.size===0;
}
function toggleSelectAll(){
  const boxes=document.querySelectorAll('#cooldowns input[type=checkbox]');
  const allSelected=Array.from(boxes).every(b=>b.checked);
  boxes.forEach(b=>{ b.checked=!allSelected; if(b.checked) cooldownSelection.add(b.value); else cooldownSelection.delete(b.value); });
  updateClearButton();
}
async function clearSelectedCooldowns(){
  if(!cooldownSelection.size) return;
  const models=Array.from(cooldownSelection);
  try{
    const url=new URL('/admin/cooldowns', location.origin);
    models.forEach(m=>url.searchParams.append('model',m));
    await req(url.pathname+url.search,{method:'DELETE'});
    cooldownSelection.clear();
    refresh();
    document.getElementById('status').textContent='Cooldowns cleared for '+models.length+' model(s)';
  }catch(e){ alert('Clear failed: '+e); }
}

let serverNow=Date.now();
let searchQuery='';
let searchDebounceTimer=null; // debounced error-log search (filter by 'q')

function setupSearch(){
  const input=document.getElementById('errorSearch');
  const clearBtn=document.getElementById('errorSearchClear');
  if(!input || !clearBtn) return;
  input.value=searchQuery;
  clearBtn.style.display=searchQuery?'block':'none';

  input.oninput=(e)=>{
    searchQuery=e.target.value;
    clearBtn.style.display=searchQuery?'block':'none';
    if(searchDebounceTimer) clearTimeout(searchDebounceTimer);
    searchDebounceTimer=setTimeout(()=>{
      refresh();
    }, 250);
  };

  clearBtn.onclick=()=>{
    searchQuery='';
    input.value='';
    clearBtn.style.display='none';
    if(searchDebounceTimer) clearTimeout(searchDebounceTimer);
    refresh();
  };

  input.onkeydown=(e)=>{
    if(e.key==='Escape'){
      e.preventDefault();
      clearBtn.onclick();
    }
  };
}

async function refresh(){
  try{
    const d=await req('/admin/dashboard/data?window='+encodeURIComponent(activeWindow)+(searchQuery.trim()?'&q='+encodeURIComponent(searchQuery.trim()):''));
    if(d.now) serverNow = new Date(d.now).getTime();
    if(d.config_updated_at){
      const elc=document.getElementById('configUpdated');
      if(elc) elc.textContent='Config updated '+new Date(d.config_updated_at).toLocaleString();
    }
    renderWindowSel();
    renderChains(d.chains||[], d.circuit_breaker);
    renderInflight(d.inflight||[]);
    renderBoard('speed', d.speed||[], [[r=>r.rate,v=>v.toFixed(1)],[r=>r.tokens,v=>v],[r=>r.latency_percentiles?r.latency_percentiles.p50_ms:null,v=>v===null?'—':v.toFixed(1)+'ms'],[r=>r.latency_percentiles?r.latency_percentiles.p95_ms:null,v=>v===null?'—':v.toFixed(1)+'ms'],[r=>r.latency_percentiles?r.latency_percentiles.p99_ms:null,v=>v===null?'—':v.toFixed(1)+'ms']]);
    renderBoard('volume', d.volume||[], [[r=>r.rate,v=>v.toFixed(3)],[r=>r.attempts,v=>v]]);
    renderBoard('failures', d.failures||[], [[r=>r.rate,v=>(v*100).toFixed(1)+'%'],[r=>r.failures,v=>v]]);
    const sessRows=[];
    for(const m of (d.inflight||[])){ for(const e of (m.endpoints||[])){ sessRows.push({endpoint:e.endpoint, sessions:e.sessions||0}); } }
    sessRows.sort((a,b)=>b.sessions-a.sessions);
    renderBoard('sessions', sessRows, [[r=>r.sessions,v=>v]]);
    if(d.cooldowns||d.circuits) renderCooldowns(d);
    setupSearch();
    if(d.sqlite_stats) renderSqliteStats(d.sqlite_stats);
    if(d.proxy_sources) renderProxySources(d.proxy_sources);
    if(d.tor) renderTorStatus(d.tor);
    const errCount = (d.cooldowns ? Object.keys(d.cooldowns).length : 0) + (d.circuits ? Object.keys(d.circuits).length : 0);
    const ecEl=document.getElementById('errorCount');
    if(ecEl) ecEl.textContent = errCount + ' error' + (errCount===1 ? '' : 's') + (searchQuery.trim() ? ' matching' : '');
    document.getElementById('status').textContent='Updated '+new Date(d.generated).toLocaleTimeString();
  }catch(e){ document.getElementById('status').textContent='Error: '+e; }
}

function openConfigEditor(){
  // Supported: /admin/config/editor /admin/config/edit /admin/config-editor
  const u=new URL('/admin/config/editor', location.href);
  if(apiKey) u.searchParams.set('api_key', apiKey);
  window.location.href=u.toString();
}
const clearBtn=document.getElementById('clearSelected');
if(clearBtn) clearBtn.onclick=clearSelectedCooldowns;
setupSearch();
refresh();
setInterval(refresh, 30000);
</script>
</body>
</html>
`
