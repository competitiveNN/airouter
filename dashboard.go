package main

// dashboard.go adds a web dashboard to the airouter gateway.
//
// The dashboard is served at /admin/dashboard and renders five sections from
// a single JSON payload fetched at /admin/dashboard/data:
//
//   - chains: each logical model's configured endpoints, with how many
//     are currently eligible (not on cooldown, circuit not open).
//   - inflight: the models with sessions being served right now, and
//     how many sessions each is generating tokens for.
//   - speed: leaderboard of fastest endpoints by tokens/s.
//   - volume: leaderboard of endpoints serving the most requests.
//   - failures: leaderboard of endpoints failing the most requests.
//
// The leaderboards are selectable by time window (1d or 7d) and the page
// refreshes on a 30 s timer so an operator can leave it open.

// dashboardHTML is the single-page dashboard. It is a const string so the
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
main{padding:2rem;max-width:1200px;margin:0 auto}
.card{background:#fff;border-radius:12px;box-shadow:0 1px 3px rgba(0,0,0,.1);padding:1.5rem;margin-bottom:1.5rem}
h2{margin-top:0}
textarea{background:#0f172a;color:#e2e8f0;padding:1rem;border-radius:8px;overflow:auto;font-family:ui-monospace,monospace;width:100%;box-sizing:border-box;min-height:16rem;resize:vertical}
button{background:#2563eb;color:#fff;border:none;padding:.6rem 1rem;border-radius:8px;cursor:pointer;margin-right:.5rem}
button:disabled{opacity:.5}
button.wsel{background:transparent;color:#64748b;border:1px solid #cbd5e1;padding:.4rem .8rem}
button.wsel.on{background:#2563eb;color:#fff;border-color:#2563eb}
table{border-collapse:collapse;width:100%;font-size:.9rem}
th,td{text-align:left;padding:.5rem .75rem;border-bottom:1px solid #e2e8f0}
th{color:#64748b;font-weight:600;text-transform:uppercase;font-size:.7rem;letter-spacing:.05em}
td.num{text-align:right;font-variant-numeric:tabular-nums}
/* One chain is a header line (model plus the counts that answer the
   operator's question at a glance) over a grid of endpoint pills. A long
   fallback chain is not a paragraph: the grid keeps the pills in aligned
   columns however many there are, and renderChains sorts the endpoints
   that need an operator's eye first. */
.chain{margin-bottom:1.25rem}
.chainhead{display:flex;gap:.5rem;align-items:baseline;margin-bottom:.4rem}
.chainhead b{min-width:5rem}
.eps{display:grid;grid-template-columns:repeat(auto-fill,minmax(15rem,1fr));gap:.35rem .6rem}
.pill{display:flex;align-items:baseline;gap:.4rem;padding:.15rem .5rem;border-radius:999px;font-size:.75rem;font-family:ui-monospace,monospace}
/* The slug is the only part that may clip: the count or status
   beside it is the reason the pill exists, so it lives in its
   own span that the ellipsis can never reach. min-width:0 lets
   the slug shrink below its content size, which is what makes
   the ellipsis engage inside a flex container. */
.pill .pnm{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pill .pct{white-space:nowrap}
.pill.ok{background:#dcfce7;color:#166534}
.pill.no{background:#fee2e2;color:#991b1b}
/* A down endpoint can be down for two different reasons, and they need
   different colours: a cooldown is a planned wait (amber, often transient),
   while an open circuit is a breaker that needs an operator's eye. */
.pill.cooldown{background:#fef9c3;color:#854d0e}
.pill.circuit{background:#f3e8ff;color:#6b21a8}
.sum{color:#64748b;font-size:.85rem;margin-left:auto}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:1.5rem}
@media(max-width:900px){.grid{grid-template-columns:1fr}}
.rate{font-variant-numeric:tabular-nums;font-weight:600}
</style>
</head>
<body>
<header><h1>AI Router Dashboard</h1><span id="status"></span></header>
<main>
<div class="card">
<h2>Chains</h2>
<p>How many endpoints in each logical model's fallback chain are currently eligible.</p>
<div id="chains"></div>
</div>
<div class="card">
<h2>Inflight</h2>
<p>Models currently serving sessions, and how many sessions each is generating tokens for right now.</p>
<div id="inflight"></div>
</div>
<div class="card">
<h2>Leaderboards <span id="wsel"></span></h2>
<div class="grid">
<div>
<h3>Fastest (tokens/s)</h3>
<table><thead><tr><th>Endpoint</th><th>Tok/s</th><th>Tokens</th><th>p50</th><th>p95</th><th>p99</th></tr></thead>
<tbody id="speed"></tbody></table>
</div>
<div>
<h3>Most requests</h3>
<table><thead><tr><th>Endpoint</th><th>Req/s</th><th>Requests</th></tr></thead>
<tbody id="volume"></tbody></table>
</div>
<div>
<h3>Highest failure rate</h3>
<table><thead><tr><th>Endpoint</th><th>Fail %</th><th>Failures</th></tr></thead>
<tbody id="failures"></tbody></table>
</div>
</div>
</div>
<div class="card">
<h2>Cooldowns</h2>
<p>Endpoints currently on cooldown or with open circuits. Check entries to clear.</p>
<div id="cooldowns"></div>
<div id="cooldownActions" style="margin-top:.75rem;display:none">
<button id="clearSelected" class="wsel" disabled>Clear selected</button>
<button id="selectAll" class="wsel" onclick="toggleSelectAll()">Select all</button>
</div>
</div>
<div class="card">
<h2>Config Editor</h2>
<p>Load and edit config (JSON). Save writes back to config.yaml and reloads.</p>
<button id="load">Load Config</button>
<button id="save" disabled>Save Config</button>
<textarea id="editor" rows="16"></textarea>
</div>
</div>
</div>
</main>
<script>
// The key comes from ?api_key= in the page URL when present, which is what
// makes the dashboard usable from a browser at all — a browser cannot set an
// Authorization header on a typed URL or a bookmark. The prompt stays as the
// fallback so a URL without the key still works, and so the query form is not
// the only way in.
const apiKey = new URLSearchParams(location.search).get('api_key')
               || prompt('Enter gateway API key for admin:') || '';
async function req(path, opts={}) {
  const headers = Object.assign({'Content-Type':'application/json'}, opts.headers||{});
  if(apiKey) headers['Authorization']='Bearer '+apiKey;
  const res = await fetch(path,{...opts,headers});
  if(!res.ok){ const t=await res.text(); throw new Error(t); }
  return res.headers.get('content-type')?.includes('application/json')? res.json(): res.text();
}
let activeWindow='1d';
function el(tag, cls, text){ const e=document.createElement(tag); if(cls)e.className=cls; if(text!==undefined)e.textContent=text; return e; }
function renderWindowSel(){
  const s=document.getElementById('wsel');
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
  // The breaker flag is rendered in the card's heading, not just shipped in
  // the JSON: an operator reading the page needs to know whether the
  // circuit column means anything, and a field they cannot see is a field
  // that gets forgotten. When the breaker is off every endpoint renders ""
  // or "closed", and without this label the column looks like decoration.
  // The flag arrives as an argument: reading it off the payload's
  // top-level object here would be a ReferenceError, because that
  // object lives in refresh()'s scope, and an exception here
  // would blank the whole page.
  for(const ch of chains){
    const row=el('div','chain');
    const head=el('div','chainhead');
    head.appendChild(el('b',null,ch.model));
    const sum=el('span','sum',ch.operational+'/'+ch.total+' eligible');
    if(!breaker) sum.title='circuit breaker is disabled; the circuit column is inactive';
    head.appendChild(sum);
    row.appendChild(head);
    const eps=el('div','eps');
    // The fallback order matters only for endpoints that
    // are actually serving. Anything that needs an
    // operator's eye jumps the queue: an open circuit
    // (deliberate, not incidental) outranks a plain down,
    // which outranks a cooldown (a planned, often
    // transient wait); eligible endpoints keep their
    // configured order at the end. The rank mirrors the
    // if-chain below so a pill's position always matches
    // the class it renders with.
    const rank=ep=>{
      if(ep.eligible) return 3;
      if(ep.cooldown) return 2;
      if(ep.circuit&&ep.circuit!=='closed') return 0;
      return 1;
    };
    const ordered=[...ch.endpoints].sort((a,b)=>rank(a)-rank(b));
    for(const ep of ordered){
      let why, cls;
      if(ep.eligible){
        // An eligible endpoint with no circuit record is still "closed" —
        // the breaker has never been created for it, which is the normal
        // state, not a missing value. Rendering "" here would leave the
        // circuit column blank for every healthy endpoint.
        why='up'+(ep.circuit?' ('+ep.circuit+')':'');
        cls='ok';
      }
      else if(ep.cooldown){
        cls='cooldown';
        // The expiry is a wall-clock instant. Render it as a countdown so the
        // operator can see how long to wait without parsing an ISO string,
        // and keep it live so a page left open shows the remaining time
        // ticking down rather than a stale timestamp.
        const t=new Date(ep.cooldown_until).getTime();
        why='cooldown '+cooldownLabel(t, serverNow);
      } else if(ep.circuit&&ep.circuit!=='closed'){ why=ep.circuit; cls='circuit'; }
      else { why='down'; cls='no'; }
      // Slug and status are separate spans: the pill ellipsizes
      // only the slug, so a long endpoint name can never hide
      // the up/down/cooldown verdict it annotates.
      const p=el('span','pill '+cls);
      const slug=el('span','pnm',ep.endpoint); slug.title=ep.endpoint;
      p.appendChild(slug);
      p.appendChild(el('span','pct',why));
      if(ep.cooldown) p.dataset.until=ep.cooldown_until;
      eps.appendChild(p);
    }
    row.appendChild(eps);
    c.appendChild(row);
  }
}
// The countdown is anchored to the gateway's clock, not the browser's: the
// gateway is the authority on whether a cooldown is still active, and a
// laptop that thinks it is five minutes ahead would render every cooldown
// as already expired. "now" is the server's notion of the moment the payload
// was generated, and the countdown advances from it.
let serverNow = Date.now();
function cooldownLabel(untilMs, atMs){
  const diff=Math.max(0, untilMs-atMs);
  const m=Math.floor(diff/60000), s=Math.floor((diff%60000)/1000);
  if(m>0) return 'until '+new Date(untilMs).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'});
  return m+':'+String(s).padStart(2,'0');
}
setInterval(()=>{
  // Rewrite only the cooldown pills' countdowns; everything else on the page
  // is untouched by this timer, which is why a refresh() does not need to
  // re-render the whole chain card to keep the label live. The anchor is the
  // server clock plus elapsed time, so skew cannot accumulate across ticks.
  serverNow += 1000;
  document.querySelectorAll('.pill.cooldown[data-until]').forEach(p=>{
    const t=Number(p.dataset.until);
    // The countdown lives in the pill's .pct span, so the rewrite
    // targets it directly: setting the pill's textContent would
    // replace the slug span with a bare text node and un-do the
    // two-part layout on every tick.
    const ct=p.querySelector('.pct');
    if(Number.isFinite(t)&&ct) ct.textContent='cooldown '+cooldownLabel(t, serverNow);
  });
}, 1000);
// renderInflight draws the live panel: one row per model that
// has sessions being served right now, with the endpoints
// generating tokens for them. It reuses the chains card's
// header-over-pills layout, because the question it answers is
// the chains card's question at a single instant: not "how many
// endpoints are eligible" but "what is being served NOW". An
// empty panel says so in words, the same way the chains card
// does, so an operator can tell "nothing in flight" from "the
// panel failed to render".
function renderInflight(rows){
  const c=document.getElementById('inflight');
  c.innerHTML='';
  if(!rows || !rows.length){ c.textContent='No sessions in flight.'; return; }
  for(const m of rows){
    const row=el('div','chain');
    const head=el('div','chainhead');
    head.appendChild(el('b',null,m.model));
    const n=m.sessions;
    const sum=el('span','sum', n+(n===1?' session':' sessions')+' in flight');
    if(m.since) sum.title='oldest session since '+m.since;
    head.appendChild(sum);
    row.appendChild(head);
    const eps=el('div','eps');
    for(const e of m.endpoints||[]){
      const s=e.sessions;
      // Slug and count are separate spans: the pill ellipsizes
      // only the slug, so a long endpoint name can never hide
      // the session count it annotates.
      const p=el('span','pill ok');
      const slug=el('span','pnm',e.endpoint); slug.title=e.endpoint;
      p.appendChild(slug);
      p.appendChild(el('span','pct', s===1?'1 session':s+' sessions'));
      eps.appendChild(p);
    }
    row.appendChild(eps);
    c.appendChild(row);
  }
}
function renderBoard(id, rows, cols){
  const tb=document.getElementById(id);
  tb.innerHTML='';
  const top=rows.slice(0,10);
  for(const r of top){
    const tr=document.createElement('tr');
    tr.appendChild(el('td',null,r.endpoint));
    for(const [acc,fmt] of cols){
      tr.appendChild(el('td','num',fmt(acc(r))));
    }
    tb.appendChild(tr);
  }
}
let cooldownSelection = new Set();
function renderCooldowns(data){
  const c=document.getElementById('cooldowns');
  c.innerHTML='';
  const cdRows=Object.entries(data.cooldowns||{}).map(([k,v])=>({model_key:k, expiry:v.expiry||null, status_code:v.status_code||0, error_count:v.error_count||0, last_error:v.last_error||'', state:''}));
  const circRows=Object.entries(data.circuits||{}).map(([k,v])=>({model_key:k, expiry:null, status_code:-1, error_count:v.probes_sent||0, last_error:'', state:'circuit '+v.state}));
  const rows=[...cdRows, ...circRows];
  if(!rows.length){
    c.textContent='No cooldowns or circuits active.';
    document.getElementById('cooldownActions').style.display='none';
    return;
  }
  document.getElementById('cooldownActions').style.display='block';
  const table=document.createElement('table');
  const thead=document.createElement('thead');
  const thr=document.createElement('tr');
  for(const h of ['','Model','Expiry','Status','Errors','Last error']){
    const th=document.createElement('th'); th.textContent=h; thr.appendChild(th);
  }
  thead.appendChild(thr);
  table.appendChild(thead);
  const tbody=document.createElement('tbody');
  for(const r of rows){
    const tr=document.createElement('tr');
    const cb=document.createElement('input'); cb.type='checkbox'; cb.value=r.model_key;
    if(cooldownSelection.has(r.model_key)) cb.checked=true;
    cb.onchange=e=>{ if(e.target.checked) cooldownSelection.add(r.model_key); else cooldownSelection.delete(r.model_key); updateClearButton(); };
    const td0=document.createElement('td'); td0.appendChild(cb); tr.appendChild(td0);
    const td1=document.createElement('td'); td1.textContent=r.model_key; tr.appendChild(td1);
    const td2=document.createElement('td'); td2.textContent=r.expiry?new Date(r.expiry).toLocaleString():'—'; tr.appendChild(td2);
    const td3=document.createElement('td'); td3.textContent=(r.state||r.status_code); tr.appendChild(td3);
    const td4=document.createElement('td'); td4.className='num'; td4.textContent=r.error_count||0; tr.appendChild(td4);
    const td5=document.createElement('td'); td5.textContent=(r.last_error||'').slice(0,120); tr.appendChild(td5);
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  c.appendChild(table);
  updateClearButton();
}
function updateClearButton(){
  const btn=document.getElementById('clearSelected');
  btn.disabled=cooldownSelection.size===0;
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
async function refresh(){
  try{
    const d=await req('/admin/dashboard/data?window='+activeWindow);
    // Anchor the countdown to the gateway's clock. The browser's clock is
    // not trustworthy here: a laptop five minutes ahead renders every
    // cooldown as already expired, and one behind makes a cooldown that has
    // not actually started look like it has hours left. The gateway is the
    // authority on whether an endpoint is cooling down, so its "now" is the
    // reference the countdown advances from.
    if(d.now) serverNow = new Date(d.now).getTime();
    renderWindowSel();
    renderChains(d.chains||[], d.circuit_breaker);
    renderInflight(d.inflight||[]);
    renderBoard('speed', d.speed||[], [[r=>r.rate,v=>v.toFixed(1)],[r=>r.tokens,v=>v],[r=>r.latency_percentiles?r.latency_percentiles.p50_ms:null,v=>v===null?'—':v.toFixed(1)+'ms'],[r=>r.latency_percentiles?r.latency_percentiles.p95_ms:null,v=>v===null?'—':v.toFixed(1)+'ms'],[r=>r.latency_percentiles?r.latency_percentiles.p99_ms:null,v=>v===null?'—':v.toFixed(1)+'ms']]);
    renderBoard('volume', d.volume||[], [[r=>r.rate,v=>v.toFixed(3)],[r=>r.attempts,v=>v]]);
    renderBoard('failures', d.failures||[], [[r=>r.rate,v=>(v*100).toFixed(1)+'%'],[r=>r.failures,v=>v]]);
    if(d.cooldowns||d.circuits) renderCooldowns(d);
    document.getElementById('status').textContent='Updated '+new Date(d.generated).toLocaleTimeString();
  }catch(e){ document.getElementById('status').textContent='Error: '+e; }
}
const editor=document.getElementById('editor');
document.getElementById('load').onclick=async()=>{
  try{
    cfg=await req('/admin/config');
    editor.value=JSON.stringify(cfg,null,2);
    document.getElementById('save').disabled=false;
    document.getElementById('status').textContent='Loaded';
  }catch(e){ alert(e); }
};
document.getElementById('save').onclick=async()=>{
  try{
    const parsed=JSON.parse(editor.value);
    await req('/admin/config',{method:'POST',body:JSON.stringify(parsed)});
    alert('Saved and reloaded');
    document.getElementById('status').textContent='Saved';
  }catch(e){ alert('Save failed: '+e); }
};
document.getElementById('clearSelected').onclick=clearSelectedCooldowns;
refresh();
setInterval(refresh, 30000);
</script>
</body>
</html>`
