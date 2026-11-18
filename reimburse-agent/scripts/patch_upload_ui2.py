import io

p = "internal/server/web/index.html"
s = io.open(p, encoding="utf-8").read()

OLD = '''function switchTab(t){
  S.tab = t;
  $("#tabTasks").classList.toggle("on", t==="tasks");
  $("#tabCases").classList.toggle("on", t==="cases");
  $("#taskPane").style.display = t==="tasks" ? "" : "none";
  $("#casePane").style.display = t==="cases" ? "" : "none";
  if(t==="tasks") loadTasks(); else loadCases();
}'''

NEW = '''function switchTab(t){
  S.tab = t;
  $("#tabTasks").classList.toggle("on", t==="tasks");
  $("#tabCases").classList.toggle("on", t==="cases");
  $("#tabUpload").classList.toggle("on", t==="upload");
  $("#taskPane").style.display = t==="tasks" ? "" : "none";
  $("#casePane").style.display = t==="cases" ? "" : "none";
  $("#uploadPane").style.display = t==="upload" ? "" : "none";
  if(t==="tasks") loadTasks();
  else if(t==="cases") loadCases();
}

// ---- 真实票据上传 ----
// 上传成功后单据进入「全部单据」，也能直接在任务列表里被审。
// 界面上要说清楚：提取失败就是失败，不会拿半份文本蒙混过关。

async function uploadInvoice(){
  const btn = $("#upBtn");
  const inv = $("#up_invoice").files[0];
  if(!inv){ $("#upResult").innerHTML = '<div class="upbad">请选择发票文件</div>'; return; }
  const cat = $("#up_category").value;
  const emp = $("#up_applicant_id").value.trim();
  if(!emp){ $("#upResult").innerHTML = '<div class="upbad">请填写申请人工号</div>'; return; }

  const fd = new FormData();
  const put = (k, v) => { if(v !== undefined && v !== null && String(v) !== "") fd.append(k, v); };
  put("claim_no", $("#up_claim_no").value.trim());
  put("request_no", $("#up_request_no").value.trim());
  put("category", cat);
  put("applicant_id", emp);
  put("applicant_name", $("#up_applicant_name").value.trim());
  put("from_city", $("#up_from_city").value.trim());
  put("city", $("#up_city").value.trim());
  put("start_date", $("#up_start_date").value);
  put("end_date", $("#up_end_date").value);
  put("purpose", $("#up_purpose").value.trim());
  const amt = $("#up_amount").value.trim();
  if(amt){ put("amount_fen", String(Math.round(parseFloat(amt)*100))); }
  fd.append("invoice", inv);
  const sup = $("#up_support").files[0];
  if(sup) fd.append("support", sup);

  btn.disabled = true;
  btn.textContent = "上传中…";
  $("#upResult").innerHTML = "";
  try{
    const r = await fetch("/api/upload", {method:"POST", body: fd});
    const d = await r.json();
    if(!r.ok || !d.ok){
      // 失败时把"该怎么办"一起显示：用户改不了的问题（扫描件/加密）要明说。
      $("#upResult").innerHTML = `<div class="upbad">上传失败：${esc(d.error||"未知错误")}
        ${d.action?`<div style="margin-top:4px;color:var(--dim)">${esc(d.action)}</div>`:""}</div>`;
      return;
    }
    $("#upResult").innerHTML = `<div class="upok">
      已建单 <b>${esc(d.case_id)}</b><br>
      发票号码 ${esc(d.invoice_no||"未能识别")} · 提取 ${d.invoice_chars} 字
      ${d.has_support?"· 佐证凭证已提取":"· 无佐证凭证"}<br>
      ${(d.warnings||[]).map(w=>`<div style="color:#e3c07b">⚠ ${esc(w)}</div>`).join("")}
      <div style="margin-top:6px;color:var(--dim)">${esc(d.next||"")}</div></div>`;
    // 让新单据出现在列表里，并直接选中，省一次点击。
    await loadCases();
    await select(d.case_id);
    switchTab("cases");
  }catch(e){
    $("#upResult").innerHTML = `<div class="upbad">上传请求失败：${esc(e.message)}</div>`;
  }finally{
    btn.disabled = false;
    btn.textContent = "上传并建单";
  }
}

$("#upBtn").onclick = uploadInvoice;'''

assert OLD in s
s = s.replace(OLD, NEW)

s = s.replace('$("#tabCases").onclick = ()=>switchTab("cases");',
              '$("#tabCases").onclick = ()=>switchTab("cases");\n$("#tabUpload").onclick = ()=>switchTab("upload");')

io.open(p, "w", encoding="utf-8").write(s)
print("ok")
