package main

import "net/http"

// configEditorHTML is a dedicated full-page config editor. It is self-contained
// (no external assets) so the CSP can stay at 'self'.
const configEditorHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1">
<title>AI Router Config Editor</title>
<style>
body{font-family:system-ui,sans-serif;margin:0;padding:0;background:#f6f7fb;color:#111}
header{background:#0f172a;color:#fff;padding:1rem 2rem;display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:.5rem}
header h1{margin:0;font-size:1.25rem}
main{padding:1rem;max-width:1400px;margin:0 auto}
.card{background:#fff;border-radius:12px;box-shadow:0 1px 3px rgba(0,0,0,.1);padding:1rem;margin-bottom:1rem}
.toolbar{display:flex;gap:.5rem;flex-wrap:wrap;align-items:center;margin-bottom:.75rem}
button{background:#2563eb;color:#fff;border:none;padding:.7rem 1.1rem;border-radius:8px;cursor:pointer;min-height:44px;font-size:1rem}
button:disabled{opacity:.5}
button.secondary{background:#64748b}
button.ghost{background:transparent;color:#2563eb;border:1px solid #cbd5e1}
.tabs{display:flex;gap:.5rem;margin-bottom:.75rem}
.tab{padding:.6rem 1rem;border-radius:8px;cursor:pointer;background:#e2e8f0;color:#111;border:none;min-height:44px}
.tab.on{background:#2563eb;color:#fff}
.pane{display:none}
.pane.on{display:block}
/* Raw JSON editor with syntax highlighting overlay */
.rawwrap{position:relative;min-height:60vh}
.rawwrap pre,.rawwrap textarea{font-family:ui-monospace,monospace;font-size:.9rem;line-height:1.45;padding:1rem;border-radius:8px;box-sizing:border-box;margin:0;width:100%;min-height:60vh;white-space:pre;overflow:auto}
.rawwrap textarea{position:relative;background:transparent;color:transparent;caret-color:#e2e8f0;z-index:2;resize:vertical;border:1px solid #334155}
.rawwrap pre{position:absolute;top:0;left:0;right:0;bottom:0;background:#0f172a;color:#e2e8f0;pointer-events:none;z-index:1;border:1px solid #334155;overflow:auto}
/* highlight classes */
.hl-key{color:#93c5fd}
.hl-str{color:#86efac}
.hl-num{color:#fca5a5}
.hl-bool{color:#fcd34d}
.hl-null{color:#c4b5fd}
.hl-punct{color:#94a3b8}
/* Tree editor */
.tree{font-family:ui-monospace,monospace;font-size:.9rem}
.tree details{margin:.25rem 0;padding-left:.5rem;border-left:2px solid #e2e8f0}
.tree summary{cursor:pointer;padding:.4rem .2rem;min-height:44px;display:flex;align-items:center;gap:.5rem;user-select:none}
.tree summary:hover{background:#f1f5f9}
.tree .row{display:flex;align-items:center;gap:.5rem;padding:.35rem .2rem;flex-wrap:wrap}
.tree input,.tree select{font-family:ui-monospace,monospace;font-size:.9rem;padding:.4rem .6rem;border:1px solid #cbd5e1;border-radius:6px;min-height:40px}
.tree input.str{min-width:12rem}
.tree .badge{background:#e2e8f0;color:#334155;padding:.15rem .5rem;border-radius:999px;font-size:.75rem}
.tree .actions{display:flex;gap:.35rem}
.tree .actions button{padding:.35rem .6rem;min-height:36px;font-size:.85rem}
.touchhint{font-size:.8rem;color:#64748b;margin-top:.5rem}
@media(max-width:600px){header{padding:.75rem 1rem} main{padding:.5rem} .rawwrap pre,.rawwrap textarea{font-size:.8rem}}
</style>
</head>
<body>
<header><h1>AI Router Config Editor</h1><span id="status"></span><a id="back" href="/admin/dashboard" style="color:#93c5fd;text-decoration:none;margin-left:1rem">Back to Dashboard</a></header>
<main>
<div class="card">
  <div class="toolbar">
    <button id="load">Load Config</button>
    <button id="save">Save Config</button>
    <button id="validate" class="secondary">Validate JSON</button>
    <button id="expandAll" class="ghost">Expand all</button>
    <button id="collapseAll" class="ghost">Collapse all</button>
  </div>
  <div class="tabs">
    <button class="tab on" data-pane="tree">Tree</button>
    <button class="tab" data-pane="raw">Raw JSON</button>
  </div>
  <div id="pane-tree" class="pane on">
    <div id="tree" class="tree"></div>
    <p class="touchhint">Tap a summary to collapse/expand. Use inputs to edit values. Touch-friendly: large tap targets.</p>
  </div>
  <div id="pane-raw" class="pane">
    <div class="rawwrap">
      <pre id="highlight"></pre>
      <textarea id="editor" spellcheck="false"></textarea>
    </div>
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
let cfg = null;
const editor = document.getElementById('editor');
const highlight = document.getElementById('highlight');
const treeEl = document.getElementById('tree');
const status = document.getElementById('status');

function esc(s){ return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

// JSON syntax highlighter
function highlightJson(text){
  let out='';
  // tokenize
  const regex = /("(?:\\.|[^"\\])*")\s*:|("(?:\\.|[^"\\])*")|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false|null)\b|([{}\[\],:])/g;
  let last=0; let m;
  while((m=regex.exec(text))!==null){
    if(m.index>last) out += esc(text.slice(last,m.index));
    if(m[1]){ out += '<span class="hl-key">'+esc(m[1])+'</span>'; }
    else if(m[2]){ out += '<span class="hl-str">'+esc(m[2])+'</span>'; }
    else if(m[3]){ out += '<span class="hl-num">'+esc(m[3])+'</span>'; }
    else if(m[4]){ 
      const v=m[4];
      if(v==='null') out += '<span class="hl-null">'+v+'</span>';
      else out += '<span class="hl-bool">'+v+'</span>';
    }
    else if(m[5]){ out += '<span class="hl-punct">'+esc(m[5])+'</span>'; }
    last = regex.lastIndex;
  }
  if(last<text.length) out += esc(text.slice(last));
  return out;
}
function syncHighlight(){
  highlight.innerHTML = highlightJson(editor.value);
  // sync scroll
  highlight.scrollTop = editor.scrollTop;
  highlight.scrollLeft = editor.scrollLeft;
}
editor.addEventListener('input', syncHighlight);
editor.addEventListener('scroll', ()=>{ highlight.scrollTop=editor.scrollTop; highlight.scrollLeft=editor.scrollLeft; });

// Tabs
document.querySelectorAll('.tab').forEach(t=>{
  t.onclick=()=>{
    document.querySelectorAll('.tab').forEach(x=>x.classList.remove('on'));
    t.classList.add('on');
    const pane=t.getAttribute('data-pane');
    document.querySelectorAll('.pane').forEach(p=>p.classList.remove('on'));
    document.getElementById('pane-'+pane).classList.add('on');
  };
});

// Tree editor
function isObj(v){ return v!==null && typeof v==='object' && !Array.isArray(v); }
function isArr(v){ return Array.isArray(v); }

function makeInput(path, value, onChange){
  const input = document.createElement('input');
  if(typeof value === 'string'){ input.className='str'; input.value=value; input.onchange=()=>onChange(path, input.value); }
  else if(typeof value === 'number'){ input.type='number'; input.value=value; input.onchange=()=>onChange(path, Number(input.value)); }
  else if(typeof value === 'boolean'){ 
    const sel=document.createElement('select');
    sel.innerHTML='<option value="true">true</option><option value="false">false</option>';
    sel.value=String(value);
    sel.onchange=()=>onChange(path, sel.value==='true');
    return sel;
  } else { input.value=String(value); input.disabled=true; }
  return input;
}

function renderTree(root, data){
  treeEl.innerHTML='';
  const frag = document.createDocumentFragment();
  function build(key, value, path, parent){
    if(isObj(value) || isArr(value)){
      const details = document.createElement('details');
      details.className='tree';
      details.open = true;
      const summary = document.createElement('summary');
      const label = isArr(value) ? (key!==null?key+' ['+value.length+']': 'root ['+value.length+']') : (key!==null?key+' {'+Object.keys(value).length+'}': 'root {'+Object.keys(value).length+'}');
      summary.innerHTML = '<span class="badge">'+(isArr(value)?'array':'object')+'</span> '+esc(label);
      details.appendChild(summary);
      const entries = isArr(value) ? value.map((v,i)=>[i,v]) : Object.entries(value);
      for(const [k,v] of entries){
        const childPath = path.concat(k);
        const row = document.createElement('div');
        row.className='row';
        const child = build(k, v, childPath, value);
        // wrap leaf inputs already handled in build; for containers append details
        if(isObj(v) || isArr(v)){
          row.appendChild(child);
        } else {
          const lbl = document.createElement('span');
          lbl.textContent = (isArr(value)? k : k+':');
          row.appendChild(lbl);
          row.appendChild(child);
          const del = document.createElement('button');
          del.className='ghost';
          del.textContent='×';
          del.onclick=()=>{
            if(isArr(value)) value.splice(k,1); else delete value[k];
            renderTree(root, rootValue);
          };
          row.appendChild(del);
        }
        details.appendChild(row);
      }
      // add property button
      const addRow = document.createElement('div');
      addRow.className='row';
      const addBtn = document.createElement('button');
      addBtn.className='ghost';
      addBtn.textContent='+ add';
      addBtn.onclick=()=>{
        const k = prompt('Key (leave empty for array index):');
        let nv = null;
        try { nv = JSON.parse(prompt('Value (JSON):','null')); } catch(e){ alert('Invalid JSON'); return; }
        if(isArr(value)){ value.push(nv); } else { if(!k){ alert('Key required'); return; } value[k]=nv; }
        renderTree(root, rootValue);
      };
      addRow.appendChild(addBtn);
      details.appendChild(addRow);
      return details;
    } else {
      const input = makeInput(path, value, (p, nv)=>{
        // set value in root
        let cur = rootValue;
        for(let i=0;i<p.length-1;i++){ cur = cur[p[i]]; }
        cur[p[p.length-1]] = nv;
        editor.value = JSON.stringify(rootValue,null,2);
        syncHighlight();
      });
      return input;
    }
  }
  rootValue = data;
  const rootEl = build(null, data, [], data);
  frag.appendChild(rootEl);
  treeEl.appendChild(frag);
}

let rootValue = null;

document.getElementById('load').onclick = async ()=>{
  try{
    cfg = await req('/admin/config');
    rootValue = cfg;
    editor.value = JSON.stringify(cfg,null,2);
    renderTree(null, cfg);
    syncHighlight();
    document.getElementById('save').disabled=false;
    status.textContent='Loaded';
  }catch(e){ alert(e); }
};
document.getElementById('save').onclick = async ()=>{
  try{
    let parsed;
    try { parsed = JSON.parse(editor.value); }
    catch(e){ // try tree value
      parsed = rootValue;
    }
    await req('/admin/config',{method:'POST',body:JSON.stringify(parsed)});
    alert('Saved and reloaded');
    status.textContent='Saved';
  }catch(e){ alert('Save failed: '+e); }
};
document.getElementById('validate').onclick = ()=>{
  try{ JSON.parse(editor.value); alert('Valid JSON'); }
  catch(e){ alert('Invalid: '+e); }
};
document.getElementById('expandAll').onclick = ()=>{
  document.querySelectorAll('#tree details').forEach(d=>d.open=true);
};
document.getElementById('collapseAll').onclick = ()=>{
  document.querySelectorAll('#tree details').forEach(d=>d.open=false);
};
// sync tree edits back to raw
const origRender = renderTree;
// keep rootValue updated when tree changes: renderTree uses rootValue global
function refreshRaw(){
  editor.value = JSON.stringify(rootValue,null,2);
  syncHighlight();
}
// patch: make renderTree store rootValue
// (already global)

// initial
editor.addEventListener('change', ()=>{
  try{ rootValue = JSON.parse(editor.value); renderTree(null, rootValue); }
  catch(e){}
});
</script>
</body>
</html>`

func (g *GatewayContext) HandleAdminConfigEditor(w http.ResponseWriter, r *http.Request) {
	serveConfigEditorPage(w, r, g)
}

func (g *GatewayContext) HandleAdminConfigPage(w http.ResponseWriter, r *http.Request) {
	serveConfigEditorPage(w, r, g)
}

func (g *GatewayContext) HandleAdminConfigEditorPage(w http.ResponseWriter, r *http.Request) {
	serveConfigEditorPage(w, r, g)
}

func (g *GatewayContext) HandleAdminConfigEdit(w http.ResponseWriter, r *http.Request) {
	serveConfigEditorPage(w, r, g)
}

func (g *GatewayContext) HandleConfigEditor(w http.ResponseWriter, r *http.Request) {
	serveConfigEditorPage(w, r, g)
}

func (g *GatewayContext) HandleConfigEditorPage(w http.ResponseWriter, r *http.Request) {
	serveConfigEditorPage(w, r, g)
}

func serveConfigEditorPage(w http.ResponseWriter, r *http.Request, g *GatewayContext) {
	if !g.checkAuth(r) {
		writeAPIError(w, 401, "Invalid API key", "authentication_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, 405, "Method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'")
	w.Write([]byte(configEditorHTML))
}
