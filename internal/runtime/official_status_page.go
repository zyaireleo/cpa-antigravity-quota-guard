package runtime

// officialGuardStatusHTML is the self-contained resource page exposed through
// the official management API resource route.  It deliberately talks only to
// the current cpa-antigravity-quota-guard ABI and keeps the management key in
// page memory for the lifetime of the document.
const officialGuardStatusHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<title>Antigravity Quota Guard</title>
<style>
:root{color-scheme:light;--bg:#f4f7fb;--panel:#ffffff;--panel-2:#f8fafc;--line:#d9e1eb;--text:#172033;--muted:#667085;--accent:#1479ff;--accent-2:#6558d8;--good:#138a5b;--warn:#a86600;--bad:#c9354d;--unknown:#7b8798;--shadow:0 12px 32px rgba(31,41,55,.08)}
*{box-sizing:border-box}
body{margin:0;min-width:320px;background:var(--bg);color:var(--text);font:14px/1.5 Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{width:min(1500px,calc(100% - 32px));margin:0 auto;padding:28px 0 48px}
h1,h2,h3,p{margin:0}
button,input{font:inherit}
button{border:1px solid #c7d2df;border-radius:10px;background:#ffffff;color:var(--text);cursor:pointer;padding:10px 15px;transition:background .16s ease,border-color .16s ease,transform .16s ease}
button:hover:not(:disabled){background:#f1f5f9;border-color:#8e9db0;transform:translateY(-1px)}
button:focus-visible,input:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
button:disabled{cursor:not-allowed;opacity:.5}
button.primary{background:linear-gradient(135deg,#0d6efd,#6558d8);border-color:#0d6efd;color:#ffffff}
button.subtle{background:transparent}
input{width:100%;border:1px solid #c7d2df;border-radius:10px;background:#ffffff;color:var(--text);padding:10px 12px}
.eyebrow{color:var(--accent);font-size:11px;font-weight:700;letter-spacing:.16em;text-transform:uppercase}
.hero{display:flex;align-items:flex-start;justify-content:space-between;gap:24px;margin-bottom:22px}
.hero h1{font-size:clamp(26px,4vw,42px);letter-spacing:-.035em;line-height:1.1;margin-top:7px}
.hero-copy{max-width:760px;color:var(--muted);margin-top:10px}
.mode-badge,.status-badge,.state-chip{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--line);border-radius:999px;padding:5px 10px;color:var(--muted);font-size:12px;white-space:nowrap}
.mode-badge{font-weight:700;text-transform:uppercase;letter-spacing:.08em}
.mode-badge.ready,.status-badge.closed,.state-chip.closed{color:var(--good);border-color:rgba(72,213,151,.45);background:rgba(72,213,151,.1)}
.mode-badge.warn,.status-badge.half_open,.state-chip.half_open{color:var(--warn);border-color:rgba(255,200,87,.45);background:rgba(255,200,87,.1)}
.mode-badge.bad,.status-badge.open,.state-chip.open{color:var(--bad);border-color:rgba(255,113,138,.45);background:rgba(255,113,138,.1)}
.status-badge.uninitialized,.state-chip.uninitialized{color:var(--unknown);border-color:rgba(168,182,199,.4);background:rgba(168,182,199,.08)}
.toolbar,.panel{border:1px solid var(--line);background:var(--panel);border-radius:16px;box-shadow:var(--shadow)}
.toolbar{display:flex;align-items:flex-end;gap:10px;padding:14px;margin-bottom:18px;flex-wrap:wrap}
.key-field{display:block;flex:1 1 300px;max-width:480px;color:var(--muted);font-size:12px;font-weight:600}
.key-field input{display:block;margin-top:5px}
.toolbar-meta{color:var(--muted);font-size:12px;margin-left:auto;padding:0 3px 9px;min-width:140px;text-align:right}
#message{min-height:22px;margin:0 3px 12px;color:var(--muted)}
#message.good{color:var(--good)}
#message.bad{color:var(--bad)}
#message.warn{color:var(--warn)}
.summary-grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin-bottom:18px}
.summary-card{border:1px solid var(--line);border-radius:14px;background:var(--panel);padding:16px;min-height:122px}
.summary-card-top{display:flex;align-items:center;justify-content:space-between;gap:10px;color:var(--muted);font-size:12px}
.summary-number{font-size:29px;font-weight:750;letter-spacing:-.04em;margin:13px 0 8px}
.summary-number small{font-size:13px;font-weight:500;color:var(--muted);letter-spacing:0}
.summary-detail{color:var(--muted);font-size:12px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.summary-detail strong{color:var(--text);font-weight:600}
.panel{padding:18px;margin-bottom:18px}
.panel-heading{display:flex;align-items:flex-start;justify-content:space-between;gap:18px;margin-bottom:14px}
.panel-heading h2{font-size:17px;letter-spacing:-.01em}
.panel-heading p{color:var(--muted);font-size:12px;margin-top:4px}
.panel-note{color:var(--muted);font-size:12px;text-align:right}
.group-overview{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;margin-bottom:18px}
.group-card{border:1px solid var(--line);border-radius:12px;padding:14px;background:var(--panel-2)}
.group-title{display:flex;align-items:center;justify-content:space-between;gap:10px;margin-bottom:12px}
.group-title strong{font-size:15px}
.group-title span{color:var(--muted);font-size:12px}
.state-list{display:flex;flex-wrap:wrap;gap:8px}
.table-wrap{overflow:auto;border:1px solid var(--line);border-radius:12px}
table{width:100%;min-width:1080px;border-collapse:collapse}
th,td{padding:11px 12px;border-bottom:1px solid rgba(36,56,82,.72);text-align:left;vertical-align:middle}
th{background:#f7f9fc;color:var(--muted);font-size:11px;font-weight:650;letter-spacing:.04em;text-transform:uppercase;position:sticky;top:0;z-index:1}
tbody tr:last-child td{border-bottom:0}
tbody tr:hover{background:#f5faff}
.account-cell strong{display:block;font-size:13px;color:var(--text);font-weight:650}
.account-cell span,.subtle-text{display:block;color:var(--muted);font-size:11px;margin-top:2px}
.quota-cell{min-width:130px}
.quota-value{display:flex;align-items:center;justify-content:space-between;gap:8px;margin-bottom:5px;font-weight:700}
.quota-value small{color:var(--muted);font-size:11px;font-weight:500}
.quota-bar{height:5px;border-radius:999px;background:#e5eaf0;overflow:hidden}
.quota-bar span{display:block;height:100%;border-radius:inherit;background:linear-gradient(90deg,#46d89c,#70d8ff)}
.quota-bar.unknown span{width:0!important;background:var(--unknown)}
.reason{max-width:220px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;color:var(--muted)}
.action-cell{white-space:nowrap}
.action-cell button{padding:7px 10px;font-size:12px}
.empty{padding:32px;text-align:center;color:var(--muted)}
.diagnostics-grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:10px;margin-bottom:14px}
.metric{border:1px solid var(--line);border-radius:11px;padding:12px;background:var(--panel-2)}
.metric span{display:block;color:var(--muted);font-size:11px}
.metric strong{display:block;font-size:20px;margin-top:6px}
.info-list{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px 18px;margin-bottom:14px}
.info-row{display:flex;align-items:baseline;justify-content:space-between;gap:16px;padding-bottom:8px;border-bottom:1px dashed rgba(36,56,82,.8)}
.info-row span{color:var(--muted);font-size:12px}
.info-row strong{font-size:12px;text-align:right;word-break:break-word}
.selection{border-top:1px solid var(--line);padding-top:14px;margin-top:8px}
.selection h3{font-size:13px;margin-bottom:8px}
.selection-grid{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:8px;color:var(--muted);font-size:12px}
.selection-grid div{border:1px solid var(--line);border-radius:9px;padding:9px;background:var(--panel-2)}
.selection-grid strong{display:block;color:var(--text);font-size:12px;margin-top:3px;word-break:break-word}
details{border-top:1px solid var(--line);padding-top:12px;margin-top:14px}
summary{cursor:pointer;color:var(--muted);font-size:12px}
pre{max-height:330px;overflow:auto;margin:10px 0 0;border:1px solid var(--line);border-radius:10px;padding:12px;background:#f7f9fc;color:#334155;font:11px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;word-break:break-word}
.footer{color:#7b8798;font-size:11px;text-align:center;margin-top:20px}
@media (max-width:1050px){.summary-grid,.diagnostics-grid{grid-template-columns:repeat(2,minmax(0,1fr))}.toolbar-meta{margin-left:0;text-align:left;padding-bottom:0}.hero{flex-direction:column}.panel-note{text-align:left}}
@media (max-width:680px){main{width:min(100% - 20px,1500px);padding-top:18px}.summary-grid,.group-overview,.diagnostics-grid,.info-list{grid-template-columns:1fr}.panel{padding:13px;border-radius:13px}.toolbar{padding:12px}.toolbar button{flex:1 1 130px}.selection-grid{grid-template-columns:1fr}.hero h1{font-size:30px}}
</style>
</head>
<body>
<main>
  <header class="hero">
    <div>
      <div class="eyebrow">CPA official ABI</div>
      <h1>反重力额度守卫</h1>
      <p class="hero-copy">按账号 × 模型组查看 Gemini 与 Claude/GPT 的额度证据、熔断状态和恢复时间。此页面只读展示当前官方接口，并提供受控的 probe 与 half-open 操作。</p>
    </div>
    <div id="modeBadge" class="mode-badge">未加载</div>
  </header>

  <section class="toolbar" aria-label="管理操作">
    <label class="key-field">Management Key
      <input id="key" type="password" autocomplete="off" spellcheck="false" placeholder="输入 CPA Management Key">
    </label>
    <button id="refresh" class="primary" type="button">刷新状态</button>
    <button id="probe" type="button">手动 Probe（Gemini）</button>
    <div id="updated" class="toolbar-meta">尚未加载</div>
  </section>
  <div id="message" role="status" aria-live="polite"></div>

  <section class="summary-grid" aria-label="状态摘要">
    <article class="summary-card"><div class="summary-card-top"><span>账号总数</span><span>roster</span></div><div id="accountCount" class="summary-number">—</div><div id="accountDetail" class="summary-detail">等待状态数据</div></article>
    <article class="summary-card"><div class="summary-card-top"><span>账号 × 模型组</span><span>entries</span></div><div id="entryCount" class="summary-number">—</div><div id="entryDetail" class="summary-detail">Gemini / Claude/GPT</div></article>
    <article class="summary-card"><div class="summary-card-top"><span>可执行状态</span><span>enforcement</span></div><div id="readyState" class="summary-number">—</div><div id="readyDetail" class="summary-detail">等待状态数据</div></article>
    <article class="summary-card"><div class="summary-card-top"><span>打开中的冷却</span><span>cooldown</span></div><div id="cooldownCount" class="summary-number">—</div><div id="cooldownDetail" class="summary-detail">由当前 snapshot.metrics 计算</div></article>
  </section>

  <section class="panel">
    <div class="panel-heading"><div><h2>模型组分布</h2><p>状态按 snapshot.entries 聚合；额度未知时不会伪造为 0%。</p></div><div id="entryUpdated" class="panel-note">—</div></div>
    <div id="groupOverview" class="group-overview"></div>
  </section>

  <section class="panel">
    <div class="panel-heading"><div><h2>账号额度明细</h2><p>Half-open 只改变指定账号 × 模型组的熔断状态，不修改 auth JSON。</p></div><div class="panel-note">状态证据由官方 quota guard 提供</div></div>
    <div class="table-wrap">
      <table>
        <thead><tr><th>账号</th><th>模型组</th><th>状态</th><th>剩余额度</th><th>恢复 / 重置</th><th>证据</th><th>原因</th><th>操作</th></tr></thead>
        <tbody id="entries"></tbody>
      </table>
      <div id="empty" class="empty" hidden>暂无账号 × 模型组状态</div>
    </div>
  </section>

  <section class="panel">
    <div class="panel-heading"><div><h2>运行诊断</h2><p>仅展示状态接口返回的调度、probe、fail-closed 和持久化指标。</p></div></div>
    <div id="metrics" class="diagnostics-grid"></div>
    <div id="info" class="info-list"></div>
    <div id="selection" class="selection"></div>
    <details><summary>查看当前状态 JSON（仅当前页面内存）</summary><pre id="rawStatus">尚未加载</pre></details>
  </section>
  <div class="footer">Management Key 仅在当前页面内存中使用，关闭页面后自动清除。</div>
</main>
<script>
(function(){
  "use strict";
  var STATUS_PATH="/v0/management/cpa-antigravity-quota-guard/status";
  var PROBE_PATH="/v0/management/cpa-antigravity-quota-guard/actions/probe";
  var HALF_OPEN_PATH="/v0/management/cpa-antigravity-quota-guard/actions/half-open";
  var managementKey="";
  var latestStatus=null;
  var busy=false;
  var statusStale=false;

  var byId=function(id){return document.getElementById(id);};
  var escapeHTML=function(value){
    return String(value===undefined||value===null?"":value).replace(/[&<>"']/g,function(ch){return {"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[ch];});
  };
  var textOrDash=function(value){return value===undefined||value===null||value===""?"—":escapeHTML(value);};
  var finiteNumber=function(value){return typeof value==="number"&&isFinite(value);};
  var meaningfulTime=function(value){
    if(!value){return false;}
    var date=new Date(value);
    return !isNaN(date.getTime())&&date.getUTCFullYear()>1;
  };
  var formatTime=function(value){
    if(!meaningfulTime(value)){return "—";}
    var date=new Date(value);
    try{return escapeHTML(date.toLocaleString("zh-CN",{hour12:false}));}catch(_){return escapeHTML(date.toISOString());}
  };
  var modelLabel=function(group){return group==="gemini"?"Gemini":group==="claude_gpt"?"Claude / GPT":textOrDash(group);};
  var stateLabel=function(state){return {open:"冷却中",closed:"可用",half_open:"试探中",uninitialized:"未初始化"}[state]||textOrDash(state);};
  var stateClass=function(state){return {open:"open",closed:"closed",half_open:"half_open",uninitialized:"uninitialized"}[state]||"uninitialized";};
  var statusLabel=function(ready){return ready?"已就绪":"未就绪";};
  var statusClass=function(ready){return ready?"ready":"warn";};
  var readKey=function(){
    managementKey=String(byId("key").value||"").trim();
    return managementKey;
  };
  var authHeaders=function(){
    var key=readKey();
    var headers={"Accept":"application/json"};
    if(key){headers.Authorization="Bearer "+key;headers["X-Management-Key"]=key;}
    return headers;
  };
  var setMessage=function(message,kind){
    var element=byId("message");
    element.textContent=message||"";
    element.className=kind||"";
  };
  var setBusy=function(value){
    busy=value;
    [byId("refresh"),byId("probe")].forEach(function(button){button.disabled=value;});
    document.querySelectorAll("[data-half-open]").forEach(function(button){button.disabled=value||statusStale;});
  };
  var apiError=function(payload,raw,status){
    if(payload&&typeof payload.error==="string"){return payload.error;}
    if(payload&&payload.error&&typeof payload.error.message==="string"){return payload.error.message;}
    if(payload&&typeof payload.message==="string"){return payload.message;}
    return raw||("HTTP "+status);
  };
  var fetchJSON=function(path,method,body){
    var headers=authHeaders();
    var options={method:method||"GET",headers:headers};
    if(body!==undefined){headers["Content-Type"]="application/json";options.body=JSON.stringify(body);}
    return fetch(path,options).then(function(response){
      return response.text().then(function(raw){
        var payload=null;
        if(raw){try{payload=JSON.parse(raw);}catch(_){payload=null;}}
        if(!response.ok){throw new Error(apiError(payload,raw,response.status));}
        return payload;
      });
    });
  };
  var groupCounts=function(entries,group){
    var counts={open:0,closed:0,half_open:0,uninitialized:0,total:0};
    entries.forEach(function(entry){if(entry.model_group===group){counts.total++;if(Object.prototype.hasOwnProperty.call(counts,entry.state)){counts[entry.state]++;}}});
    return counts;
  };
  var uniqueAccounts=function(entries){
    var seen={};
    entries.forEach(function(entry){var key=String(entry.auth_index||entry.auth_id||"");if(key){seen[key]=true;}});
    return Object.keys(seen).length;
  };
  var remainingText=function(entry){
    if(!finiteNumber(entry.remaining_percent)){return "未知";}
    return String(Math.round(entry.remaining_percent*100)/100)+"%";
  };
  var remainingWidth=function(entry){
    if(!finiteNumber(entry.remaining_percent)){return 0;}
    return Math.max(0,Math.min(100,entry.remaining_percent));
  };
  var recoveryText=function(entry){
    if(entry.state==="half_open"){
      return meaningfulTime(entry.half_open_lease_until)?"租约至 "+formatTime(entry.half_open_lease_until):"租约时间未知";
    }
    if(entry.state==="open"){
      return meaningfulTime(entry.recover_at)?"恢复 "+formatTime(entry.recover_at):"恢复时间未知";
    }
    return meaningfulTime(entry.reset_at)?"重置 "+formatTime(entry.reset_at):"—";
  };
  var renderMode=function(status){
    var badge=byId("modeBadge");
    var ready=status&&status.enforcement_ready===true;
    var configured=status&&status.configured===true;
    var mode=configured?String(status.mode||"observe"):"未配置";
    badge.className="mode-badge "+(configured?(ready?"ready":"warn"):"bad");
    badge.textContent=configured?mode+(ready?" · 已就绪":" · 未就绪"):mode;
  };
  var renderSummary=function(status,entries){
    var accounts=uniqueAccounts(entries);
    var metrics=status.snapshot&&status.snapshot.metrics?status.snapshot.metrics:{};
    var ready=status.enforcement_ready===true;
    byId("accountCount").textContent=String(accounts);
    byId("accountDetail").innerHTML="识别到 <strong>"+escapeHTML(accounts)+"</strong> 个账号";
    byId("entryCount").innerHTML=String(entries.length)+" <small>条</small>";
    byId("entryDetail").textContent="Gemini "+groupCounts(entries,"gemini").total+" · Claude/GPT "+groupCounts(entries,"claude_gpt").total;
    byId("readyState").textContent=statusLabel(ready);
    byId("readyDetail").innerHTML="mode: <strong>"+textOrDash(status.mode)+"</strong> · scheduler: <strong>"+(status.required_scheduler_abi===true?"ready":"未满足")+"</strong>";
    byId("cooldownCount").textContent=String(finiteNumber(metrics.cooldown_open)?metrics.cooldown_open:entries.filter(function(entry){return entry.state==="open"||entry.state==="half_open";}).length);
    byId("cooldownDetail").textContent="open / half-open 账号 × 模型组";
    byId("updated").textContent=status.snapshot&&status.snapshot.generated_at?"更新于 "+formatTime(status.snapshot.generated_at):"已加载";
    byId("entryUpdated").textContent=entries.length?"共 "+entries.length+" 条状态":"没有状态条目";
  };
  var renderGroupOverview=function(entries){
    var groups=["gemini","claude_gpt"];
    byId("groupOverview").innerHTML=groups.map(function(group){
      var counts=groupCounts(entries,group);
      return "<article class=\"group-card\"><div class=\"group-title\"><strong>"+modelLabel(group)+"</strong><span>"+counts.total+" 条</span></div><div class=\"state-list\">"+
        "<span class=\"state-chip closed\">可用 "+counts.closed+"</span>"+
        "<span class=\"state-chip open\">冷却中 "+counts.open+"</span>"+
        "<span class=\"state-chip half_open\">试探中 "+counts.half_open+"</span>"+
        "<span class=\"state-chip uninitialized\">未初始化 "+counts.uninitialized+"</span>"+
        "</div></article>";
    }).join("");
  };
  var renderEntries=function(entries){
    var target=byId("entries");
    byId("empty").hidden=entries.length!==0;
    target.innerHTML=entries.map(function(entry){
      var state=String(entry.state||"");
      var width=remainingWidth(entry);
      var unknown=!finiteNumber(entry.remaining_percent);
      var authIndex=String(entry.auth_index||"");
      var authID=String(entry.auth_id||"");
      var button=authIndex?"<button type=\"button\" data-half-open=\"1\" data-auth-index=\""+escapeHTML(authIndex)+"\" data-model-group=\""+escapeHTML(entry.model_group||"")+"\">Half-open</button>":"<button type=\"button\" disabled>无 auth_index</button>";
      return "<tr>"+
        "<td><div class=\"account-cell\"><strong>"+textOrDash(authIndex||authID)+"</strong>"+(authID&&authID!==authIndex?"<span>id: "+escapeHTML(authID)+"</span>":"")+"</div></td>"+
        "<td>"+modelLabel(entry.model_group)+"</td>"+
        "<td><span class=\"status-badge "+stateClass(state)+"\">"+stateLabel(state)+"</span></td>"+
        "<td><div class=\"quota-cell\"><div class=\"quota-value\"><span>"+escapeHTML(remainingText(entry))+"</span><small>"+(unknown?"无证据":"quota")+"</small></div><div class=\"quota-bar "+(unknown?"unknown":"")+"\"><span style=\"width:"+width+"%\"></span></div></div></td>"+
        "<td class=\"subtle-text\">"+recoveryText(entry)+"</td>"+
        "<td class=\"subtle-text\">"+textOrDash(meaningfulTime(entry.last_evidence_at)?entry.last_evidence_at:entry.last_quota_evidence_at)+"<br>"+textOrDash(entry.last_evidence_source)+"</td>"+
        "<td class=\"reason\" title=\""+escapeHTML(entry.reason||"")+"\">"+textOrDash(entry.reason)+"</td>"+
        "<td class=\"action-cell\">"+button+"</td>"+
      "</tr>";
    }).join("");
  };
  var metricValue=function(value){return finiteNumber(value)?String(value):value===undefined||value===null?"—":escapeHTML(value);};
  var renderDiagnostics=function(status){
    var snapshot=status.snapshot||{};
    var metrics=snapshot.metrics||{};
    var readiness=status.readiness||{};
    var features=Array.isArray(status.host_features)?status.host_features.join(", "):"—";
    var metricRows=[
      ["scheduler picks",metrics.scheduler_pick_total],
      ["scheduler excluded",metrics.scheduler_excluded_total],
      ["probe success",metrics.quota_probe_success_total],
      ["probe errors",metrics.quota_probe_error_total],
      ["fail-closed",metrics.fail_closed_total],
      ["half-open attempts",metrics.half_open_attempt_total],
      ["half-open success",metrics.half_open_success_total],
      ["half-open failure",metrics.half_open_failure_total]
    ];
    byId("metrics").innerHTML=metricRows.map(function(row){return "<div class=\"metric\"><span>"+row[0]+"</span><strong>"+metricValue(row[1])+"</strong></div>";}).join("");
    var reasons=Array.isArray(readiness.reasons)&&readiness.reasons.length?readiness.reasons.join(", "):"无";
    var required=Array.isArray(status.required_scheduler_for)&&status.required_scheduler_for.length?status.required_scheduler_for.join(", "):"—";
    byId("info").innerHTML=[
      ["configured",status.configured===true?"true":"false"],
      ["mode",status.mode],
      ["enforcement_ready",status.enforcement_ready===true?"true":"false"],
      ["required_scheduler_abi",status.required_scheduler_abi===true?"true":"false"],
      ["required_scheduler_for",required],
      ["host_features",features],
      ["readiness",readiness.ready===true?"ready":"not ready"],
      ["readiness reasons",reasons],
      ["stale evidence",metrics.quota_evidence_stale],
      ["persist errors",metrics.state_persist_error_total],
      ["last persist error",status.last_persist_error],
      ["last probe error",status.last_probe_error]
    ].map(function(row){return "<div class=\"info-row\"><span>"+escapeHTML(row[0])+"</span><strong>"+textOrDash(row[1])+"</strong></div>";}).join("");
    var selection=snapshot.last_selection;
    if(!selection){byId("selection").innerHTML="<h3>最近一次选择</h3><div class=\"subtle-text\">暂无调度选择记录</div>";return;}
    byId("selection").innerHTML="<h3>最近一次选择</h3><div class=\"selection-grid\">"+
      [["时间",formatTime(selection.at)],["请求 / 模型",textOrDash(selection.request_id)+" · "+textOrDash(selection.model)],["模型组",modelLabel(selection.model_group)],["选中账号",textOrDash(selection.selected_auth_index||selection.selected_auth_id)],["结果",selection.handled===true?(selection.observe_only?"observe-only":"handled"):"delegated"],["错误码",textOrDash(selection.error_code)]]
      .map(function(row){return "<div><span>"+escapeHTML(row[0])+"</span><strong>"+row[1]+"</strong></div>";}).join("")+"</div>";
  };
  var renderStatus=function(status){
    statusStale=false;
    latestStatus=status||{};
    var snapshot=latestStatus.snapshot||{};
    var entries=Array.isArray(snapshot.entries)?snapshot.entries:[];
    renderMode(latestStatus);renderSummary(latestStatus,entries);renderGroupOverview(entries);renderEntries(entries);renderDiagnostics(latestStatus);
    byId("rawStatus").textContent=JSON.stringify(latestStatus,null,2);
  };
  var clearStatus=function(){
    renderStatus({configured:false,snapshot:{entries:[],metrics:{}},readiness:{ready:false},host_features:[]});
    byId("updated").textContent="状态不可用";
    byId("entryUpdated").textContent="状态不可用";
  };
  var markStatusStale=function(){
    statusStale=true;
    document.querySelectorAll("[data-half-open]").forEach(function(button){button.disabled=true;});
    byId("updated").textContent="状态可能过期";
    byId("entryUpdated").textContent="状态可能过期";
  };
  var loadStatus=function(){
    if(busy){return;}
    setBusy(true);setMessage("正在读取官方状态…","warn");
    fetchJSON(STATUS_PATH,"GET").then(function(status){renderStatus(status);setMessage("状态已刷新。","good");}).catch(function(error){clearStatus();setMessage("读取状态失败："+error.message,"bad");}).then(function(){setBusy(false);});
  };
  var runProbe=function(){
    if(busy){return;}
    setBusy(true);setMessage("正在执行官方配额 probe…","warn");
    fetchJSON(PROBE_PATH,"POST",{}).then(function(status){renderStatus(status);setMessage("Probe 完成，状态已刷新。","good");}).catch(function(error){setMessage("Probe 失败："+error.message,"bad");}).then(function(){setBusy(false);});
  };
  var runHalfOpen=function(button){
    if(busy){return;}
    var authIndex=button.getAttribute("data-auth-index")||"";
    var modelGroup=button.getAttribute("data-model-group")||"";
    if(!authIndex||!modelGroup){setMessage("Half-open 缺少账号或模型组。","bad");return;}
    setBusy(true);setMessage("正在为 "+authIndex+" / "+modelLabel(modelGroup)+" 申请 half-open…","warn");
    fetchJSON(HALF_OPEN_PATH,"POST",{auth_index:authIndex,model_group:modelGroup}).then(function(){
      setMessage("Half-open 已提交，正在刷新状态…","warn");
      return fetchJSON(STATUS_PATH,"GET").then(function(status){
        renderStatus(status);
        setMessage("Half-open 已提交，状态已刷新。","good");
      }).catch(function(error){
        markStatusStale();
        setMessage("Half-open 已提交，但状态刷新失败："+error.message+"（当前数据可能过期）","warn");
      });
    }).catch(function(error){setMessage("Half-open 失败："+error.message,"bad");}).then(function(){setBusy(false);});
  };
  byId("refresh").addEventListener("click",loadStatus);
  byId("probe").addEventListener("click",runProbe);
  byId("entries").addEventListener("click",function(event){
    var button=event.target&&event.target.closest?event.target.closest("[data-half-open]"):null;
    if(button){runHalfOpen(button);}
  });
  window.addEventListener("pagehide",function(){managementKey="";byId("key").value="";});
})();
</script>
</body>
</html>`
