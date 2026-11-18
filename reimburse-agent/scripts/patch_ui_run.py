import io

p = "internal/server/web/index.html"
s = io.open(p, encoding="utf-8").read()

OLD_HEAD = '''async function run(){
  if(!S.sel) return;
  const faults = [];'''
NEW_HEAD = '''// startAudit 启动一次审核。opts.task_id 非空表示这是续审（在已有任务上再跑一轮）。
// 首次审核与续审走同一条事件流、同一个报告渲染，区别只在服务端：续审带重查计划。
async function startAudit(opts){
  opts = opts || {};
  if(!opts.task_id && !S.sel) return;
  const faults = [];'''
assert OLD_HEAD in s
s = s.replace(OLD_HEAD, NEW_HEAD)

OLD = '''  $("#runBtn").disabled = true;
  $("#runState").innerHTML = '<span class="spin"></span>运行中';
  $("#result").innerHTML = '<div class="empty">审查进行中…</div>';
  S.evCount = 0; S.report = null;
  renderSteps("plan");
  if(S.es){ S.es.close(); S.es = null; }

  let sid;
  try{
    const r = await jpost("/api/audit", {case_id:S.sel, explain:$("#explainSel").value, disabled, faults});
    sid = r.session_id;
  }catch(e){
    $("#runState").textContent = "启动失败";
    $("#result").innerHTML = `<div class="warnbar">启动失败：${esc(e.message)}</div>`;
    $("#runBtn").disabled = false;
    return;
  }
'''
NEW = '''  $("#runBtn").disabled = true;
  $("#runState").innerHTML = '<span class="spin"></span>' + (opts.task_id ? "续审中" : "运行中");
  // 任务视图里点"续审"时，右栏还是任务详情，直接覆盖会丢上下文；
  // 所以这里只把运行状态放到左栏日志区顶部。
  if(!opts.task_id) $("#result").innerHTML = '<div class="empty">审查进行中…</div>';
  S.evCount = 0; S.report = null;
  renderSteps("plan");
  if(S.es){ S.es.close(); S.es = null; }

  let sid;
  try{
    const url = opts.task_id ? `/api/tasks/${opts.task_id}/reaudit` : "/api/audit";
    const body = opts.task_id
      ? {explain:$("#explainSel").value, disabled, faults}
      : {case_id:S.sel, explain:$("#explainSel").value, disabled, faults};
    const r = await jpost(url, body);
    sid = r.session_id;
    S.lastTaskID = opts.task_id || r.task_id || null;
    if(r.recheck){
      // 续审时先把"这轮要重查什么"写进日志，读者一眼能看出不是从头再跑。
      addEvent({seq:0, stage:"plan", kind:"stage", title:"续审计划",
        detail:`复用：${(r.recheck.keep_verified||[]).join("、")||"无"}；重查判定 ${(r.recheck.check_ids||[]).length} 项`});
    }
  }catch(e){
    $("#runState").textContent = "启动失败";
    const box = opts.task_id ? $("#taskMsg") : $("#result");
    if(box) box.innerHTML = `<div class="warnbar">启动失败：${esc(e.message)}</div>`;
    $("#runBtn").disabled = false;
    return;
  }
'''
assert OLD in s
s = s.replace(OLD, NEW)

OLD2 = '''    const d = JSON.parse(e.data);
    $("#runState").textContent = d.ok ? "完成" : "异常结束";
    const st = jget("/api/stats").then(x=>{ $("#m-cost").textContent = `本次运行 ${x.sessions||0} 单 · 成本约 $${(x.cost_usd||0).toFixed(5)}`; });
  });'''
NEW2 = '''    const d = JSON.parse(e.data);
    $("#runState").textContent = d.ok ? "完成" : "异常结束";
    jget("/api/stats").then(x=>{ $("#m-cost").textContent = `本次运行 ${x.sessions||0} 单 · 成本约 $${(x.cost_usd||0).toFixed(5)}`; }).catch(()=>{});
    // 任务状态变了（退回补材料 / 转人工 / 通过），列表与详情都要刷新。
    if(S.lastTaskID){ loadTasks(); if(S.taskID===S.lastTaskID) openTask(S.lastTaskID); S.lastTaskID=null; }
  });'''
assert OLD2 in s
s = s.replace(OLD2, NEW2)

s = s.replace('$("#runBtn").onclick = run;', '$("#runBtn").onclick = ()=>startAudit({});')
io.open(p, "w", encoding="utf-8").write(s)
print("ok")
