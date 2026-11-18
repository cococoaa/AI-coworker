const toolNames = {verify_invoice:"发票查验",check_company:"销售方核验",check_history:"历史查重"};

function readToolOutcome(value){
  const verdict = value?.status ?? value?.Status;
  const code = typeof verdict==="object" && verdict!==null ? (verdict.status ?? verdict.Status) : verdict;
  const status = ({1:"found",2:"not_found",3:"failed",4:"timeout"})[code] || code;
  const invoices = value?.invoices ?? value?.Invoices ?? [];
  const companies = value?.companies ?? value?.Companies ?? [];
  const history = value?.history ?? value?.History ?? [];
  const count = verdict?.records ?? verdict?.Records ?? (invoices.length + companies.length + history.length);
  return {status,count,invoices,companies,history,reason:verdict?.reason ?? verdict?.Reason ?? value?.detail ?? ""};
}

function toolStatusText(tool){
  if(tool.status==="found" || tool.status==="ok") return tool.count ? `已核验 · 查到 ${tool.count} 条记录` : "已核验 · 未查到记录";
  if(tool.status==="not_found") return "已核验 · 未查到记录";
  if(tool.status==="timeout") return "核验超时";
  if(tool.status==="failed" && !/^发票号不可信，无从查[验重]$|^销售方税号不可信，无从核验$/.test(tool.reason)) return "核验失败";
  return "未核验";
}

const reviewDate = new Intl.DateTimeFormat("zh-CN", {dateStyle:"short",timeStyle:"short",timeZone:"Asia/Shanghai"});
const reviewPageSize = 8;
const reviewCategories = {transport:"交通",lodging:"住宿",meal:"餐饮"};

function reviewCard(r, canRevoke){
  const when = new Date(r.at);
  const date = r.at && !Number.isNaN(when.getTime()) ? reviewDate.format(when) : "";
  const details = [reviewCategories[r.category], r.task_id ? "任务 "+r.task_id : "", r.case_id ? "单据 "+r.case_id : ""].filter(Boolean);
  const revoked = r.revoked_at
    ? `<div class="review-revoked">已撤销${r.revoked_by_name||r.revoked_by?" · "+esc(r.revoked_by_name||r.revoked_by):""}${r.revoke_reason?" · "+esc(r.revoke_reason):""}</div>`
    : "";
  return `<article class="rvitem ${r.revoked_at?"rev":""}">
    <div class="review-top"><strong>${esc(r.claim_no || r.case_id || "未关联报销单")}</strong><span class="what">${esc(DEC[r.decision]||r.decision)}</span></div>
    ${details.length?`<div class="muted">${esc(details.join(" · "))}</div>`:""}
    ${r.case_title?`<div class="muted">${esc(r.case_title)}</div>`:""}
    <div class="review-top"><span class="muted">${esc([r.operator_name||r.operator,date,r.id].filter(Boolean).join(" · "))}</span>
      ${canRevoke&&!r.revoked_at?`<button data-rv="${esc(r.id)}">撤销</button>`:""}</div>
    ${r.note?`<div class="review-note">${esc(r.note)}</div>`:""}${revoked}
  </article>`;
}

async function loadReviews(box = $("#reviewList")){
  if(!box) return;
  const previousPage = Number(box.dataset.page || 1);
  const previousQuery = box.querySelector(".review-search")?.value || "";
  const onlyPrevious = box.querySelector(".review-only")?.checked || false;
  const caseID = box.dataset.case || "";
  const canRevoke = box.id==="reviewList";
  try{
    const data = await jget("/api/reviews");
    if(!box.isConnected) return;
    const records = data.reviews || [];
    box.innerHTML = `<div class="review-controls">
      <input class="review-search" type="search" aria-label="搜索复核记录" placeholder="搜索报销单号、任务号或复核人" value="${esc(previousQuery)}">
      ${caseID?`<label><input class="review-only" type="checkbox" ${onlyPrevious?"checked":""}> 只看当前单据</label>`:""}
      </div><div class="review-rows"></div>
      <div class="review-pager"><button type="button" class="ghost review-prev">上一页</button><span class="review-count" aria-live="polite"></span><button type="button" class="ghost review-next">下一页</button></div>`;
    if(canRevoke) box.insertAdjacentHTML("afterbegin",'<h3 class="review-heading">复核记录</h3>');
    const search=box.querySelector(".review-search");
    const only=box.querySelector(".review-only");
    const prev=box.querySelector(".review-prev");
    const next=box.querySelector(".review-next");
    const rows=box.querySelector(".review-rows");
    let page = previousPage;
    function renderPage(){
      const term = search.value.trim().toLowerCase();
      const matches = records.filter(r => (!only?.checked || r.case_id===caseID) &&
        (!term || [r.claim_no,r.task_id,r.case_id,r.case_title,r.operator_name,r.operator,r.id]
          .some(field=>String(field??"").toLowerCase().includes(term))));
      const pageCount = Math.max(1, Math.ceil(matches.length/reviewPageSize));
      page = Math.min(pageCount,Math.max(page,1));
      box.dataset.page = page;
      rows.innerHTML = matches.length ? matches.slice((page-1)*reviewPageSize,page*reviewPageSize)
        .map(r=>reviewCard(r,canRevoke)).join("") : '<div class="gap">没有符合条件的复核记录</div>';
      prev.disabled = page===1;
      next.disabled = page===pageCount;
      box.querySelector(".review-count").textContent = `共 ${matches.length} 条 · ${page}/${pageCount} 页`;
    }
    search.oninput=()=>{page=1;renderPage();};
    if(only) only.onchange=()=>{page=1;renderPage();};
    prev.onclick=()=>{page--;renderPage();};
    next.onclick=()=>{page++;renderPage();};
    rows.onclick=async event=>{
      const button = event.target.closest("button[data-rv]");
      if(!button) return;
      const who = $("#whoSel").value;
      if(!who){ alert("请先选择操作人"); return; }
      const reason = prompt("撤销原因（会记入审计）");
      if(reason===null) return;
      button.disabled=true;
      try{
        const r = await jpost("/api/review/revoke",{id:button.dataset.rv,reason,employee_id:who});
        if(box.isConnected) $("#reviewResult").innerHTML = `<div class="warnbar">${esc(r.impact.message)}</div>`;
        await Promise.all([loadReviews(box),loadTasks()]);
      }catch(e){ button.disabled=false; alert("撤销失败：" + e.message); }
    };
    renderPage();
  }catch(e){
    if(box.isConnected) box.innerHTML = '<div class="warnbar">复核记录加载失败，请重试。</div>';
  }
}
