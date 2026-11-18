import io

p = "internal/server/web/index.html"
s = io.open(p, encoding="utf-8").read()

OLD = '''  renderSteps(null);
  await loadPeople();
  await loadQuota();
  await loadCases();
}'''

NEW = r'''  renderSteps(null);
  await loadPeople();
  await loadQuota();
  await loadCases();
  await loadTasks();
}

// ---- 任务队列 ----
// 财务上班第一件事是问"哪些单卡住了、卡在谁那里"，所以默认视图是进行中的任务，
// 而不是单据清单。单据清单退到第二个页签（演示时需要它挑单）。

async function loadTasks(){
  let d;
  try{ d = await jget("/api/tasks"); }
  catch(e){ $("#tasks").innerHTML = `<div class="gap">任务库不可用：${esc(e.message)}</div>`; return; }
  if(!d.enabled){
    $("#tasks").innerHTML = '<div class="gap">未启用任务库（启动时 -task-file 为空）</div>';
    return;
  }
  S.tasks = d.tasks || [];
  const active = S.tasks.filter(t=>!["approved","rejected","withdrawn"].includes(t.state));
  $("#taskCount").textContent = active.length;
  const waitMore = active.filter(t=>t.state==="need_more").length;
  const waitManual = active.filter(t=>t.state==="need_manual").length;
  $("#taskFilters").innerHTML = [
    ["waiting", `卡住的 ${waitMore+waitManual}`],
    ["need_more", `待补材料 ${waitMore}`],
    ["need_manual", `待复核 ${waitManual}`],
    ["all", `全部 ${S.tasks.length}`],
  ].map(([k,n])=>`<button data-f="${k}" class="${S.taskFilter===k?"on":""}">${n}</button>`).join("");
  $("#taskFilters").querySelectorAll("button").forEach(b=>b.onclick=()=>{S.taskFilter=b.dataset.f;loadTasks();});

  let list = S.tasks;
  if(S.taskFilter==="waiting") list = S.tasks.filter(t=>t.state==="need_more"||t.state==="need_manual");
  else if(S.taskFilter!=="all") list = S.tasks.filter(t=>t.state===S.taskFilter);

  if(list.length===0){
    $("#tasks").innerHTML = `<div class="gap">${S.taskFilter==="waiting"?"没有卡住的单":"没有任务"}<br>
      <span class="muted">在「全部单据」里选一张开始审核，任务会自动建出来</span></div>`;
    return;
  }
  $("#tasks").innerHTML = list.map(t=>{
    const miss = (t.outstanding_labels||[]).join("、");
    return `<div class="taskitem ${S.taskID===t.id?"sel":""}" data-t="${esc(t.id)}">
      <div class="top">
        <span class="st ${t.state}">${esc(t.state_label)}</span>
        <span class="tid">${esc(t.id)}</span>
        <span style="flex:1"></span>
        <span class="tid">${esc(t.case_id)}</span>
      </div>
      <div class="sub">卡在 <span class="who">${esc(t.waiting_on)}</span> · ${t.waiting_days>0?`已 ${t.waiting_days} 天`:"今天"} · 第 ${t.rounds||0} 轮</div>
      ${miss?`<div class="miss">缺：${esc(miss)}</div>`:""}
    </div>`;
  }).join("");
  $("#tasks").querySelectorAll(".taskitem").forEach(el=>el.onclick=()=>openTask(el.dataset.t));
}

// openTask 展开一张任务：补件清单、材料版本、续审计划、事件轨迹。
async function openTask(id){
  S.taskID = id;
  await loadTasks();
  let d;
  try{ d = await jget("/api/tasks/"+id); }
  catch(e){ $("#result").innerHTML = `<div class="warnbar">读取任务失败：${esc(e.message)}</div>`; return; }
  const t = d.task; S.task = t;
  const c = S.cases.find(x=>x.id===t.case_id);
  if(c) S.sel = t.case_id;

  const stTxt = t.state==="need_more" ? "退回补材料" : t.state==="need_manual" ? "转人工复核"
    : t.state==="pending" ? "待审核" : t.state==="approved" ? "初审通过" : t.state==="rejected" ? "已驳回" : "已撤销";
  const kindName = {absent:"原件未填写", unreadable:"无法辨认", ambiguous:"两模型不一致", missing_attachment:"整份缺失"};
  const items = (t.missing||[]).map(m=>`<div class="finding-row" style="${m.resolved?"opacity:.5;border-left-color:var(--ok)":""}">
    <div class="h">${esc(m.label)} · ${kindName[m.kind]||m.kind}${m.resolved?' <span style="color:var(--ok)">已补交</span>':""}</div>
    <div class="r">${esc(m.reason)}${m.clause?`（依据 ${esc(m.clause)}）`:""}</div>
    ${m.resolved?"":`<div style="margin-top:6px"><input class="supInput" data-k="${esc(m.key)}"
       placeholder="粘贴补交材料正文；只填文件名不算补交" style="width:100%;background:var(--panel2);
       border:1px solid var(--line);color:var(--fg);border-radius:5px;padding:4px 7px;font-size:11.5px;font-family:inherit">
       <button class="ghost supBtn" data-k="${esc(m.key)}" data-l="${esc(m.label)}"
       style="margin-top:4px;font-size:11px">提交补交</button></div>`}
  </div>`).join("");

  const atts = (t.attachments||[]).map(a=>`<div class="rvitem">v${a.version} · ${esc(a.name)}
    · ${esc(a.by||"")} · ${esc((a.at||"").slice(0,16).replace("T"," "))}
    ${a.note?`· ${esc(a.note)}`:""}</div>`).join("");

  const rc = t.next_recheck;
  const rcBox = rc ? `<div class="muted" style="margin-top:4px">续审时重查 ${(rc.check_ids||[]).length} 项判定：
    ${esc((rc.check_ids||[]).join("、")||"无")}<br>复用已核验：${esc((rc.keep_verified||[]).join("、")||"无")}
    <br>必须重查：${esc((rc.reverify||[]).join("、")||"无")}</div>` : "";

  const runs = (t.runs||[]).map(r=>`<div class="rvitem">第 ${r.round} 轮 · ${DEC[r.decision]||r.decision}
    · 核准 ${yuan(r.approved_fen)} · ${r.elapsed_ms||0}ms · $${(r.cost_usd||0).toFixed(5)}
    ${r.trigger?`· ${r.trigger==="reaudit"?"续审":"首次"}`:""}</div>`).join("");

  const evs = (t.events||[]).map(e=>`<div class="rvitem">${esc((e.at||"").slice(0,16).replace("T"," "))}
    · ${esc(e.detail)}${e.by?` <span class="who">${esc(e.by)}</span>`:""}</div>`).join("");

  const canReaudit = (t.state==="pending"||t.state==="need_more") && !(t.missing||[]).some(m=>!m.resolved);

  $("#result").innerHTML = `
    <div class="verdict v-${t.state==="approved"?"approve":t.state==="rejected"?"reject":t.state==="need_more"?"need_more_info":"manual_review"}">
      <div class="big">${stTxt}</div>
      <div class="amt">${esc(t.claim_no)} · ${esc(t.id)} · 第 ${(t.runs||[]).length} 轮</div>
      <div class="muted" style="margin-top:6px">卡在：<b style="color:#a8c7fa">${esc(d.waiting_on)}</b></div>
    </div>
    <h3 style="font-size:12px;color:var(--faint);margin:14px 0 4px">补件清单（${(t.missing||[]).filter(m=>!m.resolved).length}/${(t.missing||[]).length} 项待补）</h3>
    ${items || '<div class="muted">没有缺件</div>'}
    ${canReaudit?`<button class="primary" id="reauditBtn" style="margin-top:10px;width:100%">
      ${(t.runs||[]).length>0?"续审（复用已核验结果）":"开始审核"}</button>`:
      t.state==="need_more"?`<div class="warnbar">还有 ${(t.missing||[]).filter(m=>!m.resolved).length} 项没补齐，不能续审</div>`:""}
    <div id="taskMsg" style="margin-top:8px"></div>
    ${rcBox}
    ${atts?`<h3 style="font-size:12px;color:var(--faint);margin:14px 0 4px">材料版本</h3>${atts}`:""}
    ${runs?`<h3 style="font-size:12px;color:var(--faint);margin:14px 0 4px">审核批次</h3>${runs}`:""}
    ${evs?`<h3 style="font-size:12px;color:var(--faint);margin:14px 0 4px">任务轨迹</h3>${evs}`:""}
    <div style="margin-top:10px"><button class="ghost" id="taskBackBtn">← 回到任务列表</button></div>`;

  $("#taskBackBtn").onclick = ()=>{ S.taskID=null; S.task=null; loadTasks(); $("#result").innerHTML='<div class="empty">还没有结果</div>'; };
  if($("#reauditBtn")) $("#reauditBtn").onclick = ()=>startAudit({task_id:t.id});
  $("#result").querySelectorAll(".supBtn").forEach(b=>{
    b.onclick = ()=>submitSupport(t.id, b.dataset.k, b.dataset.l, b);
  });
}

// submitSupport 提交一项补交材料，然后刷新任务。
async function submitSupport(taskID, key, label, btn){
  const input = document.querySelector(`.supInput[data-k="${key}"]`);
  const content = input ? input.value.trim() : "";
  if(!content){ alert("请粘贴补交材料的正文。只登记文件名，续审时判定不会变。"); return; }
  const who = $("#whoSel").value;
  btn.disabled = true;
  try{
    const r = await jpost(`/api/tasks/${taskID}/resubmit`, {
      kind:key, name:label, note:"界面补交", content, employee_id:who});
    // 幂等命中也算成功，但要说清楚没有新增版本。
    const msg = r.duplicate ? "这份材料之前已经提交过，本次没有重复登记。" : "已登记，材料版本 +1。";
    const next = (r.next||{}).message||"";
    await loadTasks();
    await openTask(taskID);
    $("#taskMsg").innerHTML = `<div class="warnbar">${esc(msg)} ${esc(next)}</div>`;
  }catch(e){
    btn.disabled = false;
    $("#taskMsg").innerHTML = `<div class="warnbar">提交失败：${esc(e.message)}</div>`;
  }
}'''

assert OLD in s
s = s.replace(OLD, NEW)
io.open(p, "w", encoding="utf-8").write(s)
print("ok")
