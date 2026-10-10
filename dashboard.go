package main

// dashboard.go adds a web dashboard to the airouter gateway.
//
// The dashboard is served at /admin/dashboard and renders four sections from
// a single JSON payload fetched at /admin/dashboard/data:
//
//   - chains: each logical model's configured endpoints, with how many are
//     currently eligible (not on cooldown, circuit not open).
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
textarea{background:#0f172a;color:#e2e8f0;padding:1rem;border-radius:8px;overflow:auto;font-family:ui-monospace,monospace}
button{background:#2563eb;color:#fff;border:none;padding:.6rem 1rem;border-radius:8px;cursor:pointer;margin-right:.5rem}
button:disabled{opacity:.5}
button.wsel{background:transparent;color:#64748b;border:1px solid #cbd5e1;padding:.4rem .8rem}
button.wsel.on{background:#2563eb;color:#fff;border-color:#2563eb}
table{border-collapse:collapse;width:100%;font-size:.9rem}
th,td{text-align:left;padding:.5rem .75rem;border-bottom:1px solid #e2e8f0}
th{color:#64748b;font-weight:600;text-transform:uppercase;font-size:.7rem;letter-spacing:.05em}
td.num{text-align:right;font-variant-numeric:tabular-nums}
.chain{display:flex;gap:.5rem;align-items:center;margin-bottom:.5rem;flex-wrap:wrap}
.chain b{min-width:5rem}
.pill{padding:.15rem .5rem;border-radius:999px;font-size:.75rem;font-family:ui-monospace,monospace}
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
<h2>Leaderboards <span id="wsel"></span></h2>
<div class="grid">
<div>
<h3>Fastest (tokens/s)</h3>
<table><thead><tr><th>Endpoint</th><th>Tok/s</th><th>Tokens</th></tr></thead>
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
<div>
<h2>Config Editor</h2>
<p>Load and edit config (JSON). Save writes back to config.yaml and reloads.</p>
<button id="load">Load Config</button>
<button id="save" disabled>Save Config</button>
<textarea id="editor" rows="20" style="width:100%;"></textarea>
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
let window='1d';
function el(tag, cls, text){ const e=document.createElement(tag); if(cls)e.className=cls; if(text!==undefined)e.textContent=text; return e; }
function renderWindowSel(){
  const s=document.getElementById('wsel');
  s.innerHTML='';
  for(const w of ['1d','7d']){
    const b=el('button', 'wsel'+(w===window?' on':''), w);
    b.onclick=()=>{ window=w; refresh(); };
    s.appendChild(b);
  }
}
function renderChains(chains){
  const c=document.getElementById('chains');
  c.innerHTML='';
  if(!chains || !chains.length){ c.textContent='No chains configured.'; return; }
  for(const ch of chains){
    const row=el('div','chain');
    row.appendChild(el('b',null,ch.model));
    row.appendChild(el('span','sum',ch.operational+'/'+ch.total+' eligible'));
    c.appendChild(row);
    const eps=el('div');
    eps.style.marginLeft='5rem';
    for(const ep of ch.endpoints){
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
        why='cooldown '+cooldownLabel(t);
      } else if(ep.circuit&&ep.circuit!=='closed'){ why=ep.circuit; cls='circuit'; }
      else { why='down'; cls='no'; }
      const p=el('span','pill '+cls, ep.endpoint+' '+why);
      if(ep.cooldown) p.dataset.until=ep.cooldown_until;
      p.style.marginRight='6px';
      eps.appendChild(p);
    }
    c.appendChild(eps);
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
    if(Number.isFinite(t)) p.textContent=p.textContent.replace(/cooldown \S+$/, 'cooldown '+cooldownLabel(t, serverNow));
  });
}, 1000);
function renderBoard(id, rows, cols){
  const tb=document.getElementById(id);
  tb.innerHTML='';
  const top=rows.slice(0,10);
  for(const r of top){
    const tr=document.createElement('tr');
    tr.appendChild(el('td',null,r.endpoint));
    for(const [key,fmt] of cols){
      tr.appendChild(el('td','num',fmt(r[key])));
    }
    tb.appendChild(tr);
  }
}
async function refresh(){
  try{
    const d=await req('/admin/dashboard/data?window='+window);
    // Anchor the countdown to the gateway's clock. The browser's clock is
    // not trustworthy here: a laptop five minutes ahead renders every
    // cooldown as already expired, and one behind makes a cooldown that has
    // not actually started look like it has hours left. The gateway is the
    // authority on whether an endpoint is cooling down, so its "now" is the
    // reference the countdown advances from.
    if(d.now) serverNow = new Date(d.now).getTime();
    renderWindowSel();
    renderChains(d.chains||[]);
    renderBoard('speed', d.speed||[], [['rate',v=>v.toFixed(1)],['tokens',v=>v]]);
    renderBoard('volume', d.volume||[], [['rate',v=>v.toFixed(3)],['attempts',v=>v]]);
    renderBoard('failures', d.failures||[], [['rate',v=>(v*100).toFixed(1)+'%'],['failures',v=>v]]);
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
refresh();
setInterval(refresh, 30000);
</script>
</body>
</html>`
