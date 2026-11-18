const chatLog = $("#chatLog");
const chatForm = $("#chatForm");
const chatInput = $("#chatInput");
const chatSend = $("#chatSend");
const chatAttachment = $("#chatAttachment");
const chatCasePicker = $("#chatCasePicker");
const chatMessages = [];

function setWorkbench(open){
  $("#businessPane").hidden = !open;
  $("#workbenchMeta").hidden = !open;
  const toggle = $("#workbenchToggle");
  toggle.setAttribute("aria-expanded", String(open));
  toggle.textContent = open ? "收起工作台" : "审核工作台";
}

$("#workbenchToggle").onclick = () => {
  if(!$("#contestPane").hidden) $("#contestToggle").click();
  const open = $("#businessPane").hidden;
  setWorkbench(open);
  if(open) $("#businessPane").scrollIntoView({block:"start"});
};

function addChatTurn(role, text){
  const row = document.createElement("article");
  row.className = "chat-turn " + role;
  const speaker = document.createElement("span");
  speaker.className = "chat-speaker";
  speaker.textContent = role === "user" ? "你" : "报销同事";
  const message = document.createElement("div");
  message.className = "chat-message";
  message.textContent = text;
  row.append(speaker, message);
  chatLog.append(row);
  if(!$("#conversation").hidden && $("#businessPane").hidden){
    chatForm.scrollIntoView({block:"end"});
  }
  return {row, message, finished:false};
}

chatForm.addEventListener("submit", async event => {
  event.preventDefault();
  const text = chatInput.value.trim();
  if(!text || chatSend.disabled) return;
  addChatTurn("user", text);
  chatInput.value = "";
  chatSend.disabled = true;
  const turn = addChatTurn("assistant", "…");
  const outgoing = [...chatMessages.slice(-8), {role:"user", content:text}];
  try {
    const data = await jpost("/api/chat", {messages:outgoing});
    const reply = data.reply || "暂时没有回复，请再试一次。";
    turn.message.textContent = reply;
    if(data.mode === "local") turn.row.querySelector(".chat-speaker").textContent = "报销同事 · 离线回复";
    if(data.mode === "model" || data.mode === "business"){
      chatMessages.push({role:"user", content:text}, {role:"assistant", content:reply.slice(0,1600)});
      if(chatMessages.length > 8) chatMessages.splice(0, chatMessages.length - 8);
    }
  } catch(e) {
    turn.message.textContent = "消息未发送成功，请重试。";
    if(!chatInput.value) chatInput.value = text;
  } finally {
    chatSend.disabled = false;
    if(!$("#conversation").hidden && !chatInput.value) chatInput.focus({preventScroll:true});
  }
});

function openChatAttachment(){
  if(!$("#contestPane").hidden) $("#contestToggle").click();
  chatCasePicker.hidden = true;
  chatAttachment.hidden = false;
  chatAttachment.open = true;
  chatAttachment.scrollIntoView({block:"nearest"});
}

chatAttachment.append(document.querySelector("#uploadPane .upform"));
$("#upBtn").textContent = "上传并送审";
$("#chatAttach").onclick = () => {
  if(chatAttachment.open){
    chatAttachment.open = false;
    chatAttachment.hidden = true;
  }else{
    openChatAttachment();
  }
};

$("#chatChoose").onclick = async () => {
  chatCasePicker.hidden = !chatCasePicker.hidden;
  if(chatCasePicker.hidden) return;
  chatAttachment.hidden = true;
  chatAttachment.open = false;
  try {
    await loadCases();
    const select = $("#chatCase");
    select.replaceChildren(new Option("选择单据", ""));
    for(const c of S.cases) select.add(new Option(c.id + " · " + c.title, c.id));
    select.focus();
  } catch(e) {
    addChatTurn("assistant", "暂时读不到单据列表，请重试。");
    chatCasePicker.hidden = true;
  }
};

$("#chatAuditExisting").onclick = async () => {
  const id = $("#chatCase").value;
  if(!id || S.running || $("#upBtn").disabled) return;
  const button = $("#chatAuditExisting");
  button.disabled = true;
  try {
    await select(id);
    switchTab("cases");
    setWorkbench(false);
    chatCasePicker.hidden = true;
    await startAudit({label:id});
  } catch(e) {
    addChatTurn("assistant", "暂时读不到这张单据，请重试。");
  } finally {
    button.disabled = false;
  }
};

function beginAuditTurn(label, recheck){
  addChatTurn("user", (recheck ? "补件续审 · " : "送审 · ") + label);
  return addChatTurn("assistant", "我在审核，稍等一下。");
}

function finishAuditTurn(turn, message){
  turn.message.textContent = message;
  turn.finished = true;
}

function auditErrorText(err){
  try{
    const parsed = JSON.parse(err.message);
    if(parsed.error) return "这次未能送审。" + parsed.error;
  }catch{}
  return "这次未能送审，请重试。";
}

const supportGuides = {
  transport: {
    title:"铁路或航空行程单",
    source:"从购票平台、铁路 12306 或航空公司订单里找原始行程凭证。",
    fields:[["乘车人或旅客姓名","按凭证填写姓名"],["乘车或乘机日期","按凭证选择日期","date"],
      ["车次或航班","如 G7305 或 MU5101"],["席别或舱位","如 二等座 或 经济舱"],["合计金额（元）","如 553.00","money"]]
  },
  lodging: {
    title:"酒店水单",
    source:"从酒店前台或住宿订单索取结账水单。",
    fields:[["入住人","按水单填写姓名"],["入住日期","","date"],["离店日期","","date"],
      ["住宿晚数","如 2","count"],["合计金额（元）","如 760.00","money"]]
  },
  meal: {
    title:"餐饮消费清单",
    source:"从餐厅索取带菜品和价格的结账明细。",
    fields:[["消费日期","","date"],["消费明细","每行一项，写清品名和金额，如 工作餐 ¥88.00","lines"],
      ["合计金额（元）","如 88.00","money"]]
  }
};

function supplementField(key, label, help, type="text", required=true){
  const attrs = `class="supField" data-field="${esc(key)}" data-label="${esc(label)}" ${required?"required":""}`;
  const hint = help ? `<small>${esc(help)}</small>` : "";
  return `<label><span>${esc(label)}</span>${type==="lines"
    ? `<textarea ${attrs} rows="3"></textarea>`
    : `<input ${attrs} type="${type==="date"?"date":type==="count"?"number":"text"}"
        ${type==="count"?'min="0" step="1"':type==="money"?'inputmode="decimal"':''}>`}${hint}</label>`;
}

function supplementEditor(item, category, claimFields){
  if(item.key==="claim_info" && claimFields?.length){
    const fields = claimFields.map(f=>supplementField(f.key,f.label,f.help,f.key.endsWith("_count")?"count":"text",f.required));
    return `<div class="sup-editor" data-mode="claim">${fields.join("")}</div>`;
  }
  if(item.key==="support" && supportGuides[category]){
    const guide = supportGuides[category];
    const fields = guide.fields.map(([label,help,type],i)=>supplementField(i,label,help,type)).join("");
    return `<div class="sup-editor" data-mode="document" data-category="${esc(category)}">
      <p class="muted">${esc(guide.source)} 请按原件填写下列信息。</p>
      ${category==="transport"?'<label><span>交通方式</span><select class="supMode"><option value="train">火车</option><option value="flight">飞机</option></select></label>':""}
      ${fields}
      <details class="sup-paste"><summary>已有凭证文本？直接粘贴</summary>
        <label><span>${esc(guide.title)}正文</span><textarea class="supPasted" rows="5" placeholder="粘贴原文，保留日期、姓名或消费明细及合计金额。"></textarea></label>
      </details>
    </div>`;
  }
  const hint = item.key==="claim_info"
    ? "用「申请单号：审批单上的编号」这样的格式填写，每项一行。"
    : item.key==="support" ? "粘贴佐证凭证正文，包含实际日期、明细及合计金额。" : "请填写原件上能确认的信息。";
  return `<div class="sup-editor" data-mode="text">${item.hint && item.kind==="explanation"?`<p class="muted">${esc(item.hint)}</p>`:""}<label><span>${esc(item.label)}</span>
    <textarea class="supPasted" rows="4" placeholder="${esc(hint)}" required></textarea></label></div>`;
}

function readSupplement(editor){
  const pasted = editor.querySelector(".supPasted");
  if(pasted && (editor.dataset.mode==="text" || pasted.closest("details")?.open)){
    const value = pasted.value.trim();
    if(!value) { pasted.focus(); throw new Error("请粘贴凭证正文或关闭粘贴区，按上面的字段填写。"); }
    return value;
  }
  const fields = Array.from(editor.querySelectorAll(".supField"));
  const values = Object.fromEntries(fields.map(f=>[f.dataset.field,f.value.trim()]));
  for(const field of fields){
    if((field.required && !field.value.trim()) || !field.validity.valid){
      field.focus();
      throw new Error("请正确填写" + field.dataset.label + "。");
    }
  }
  if(editor.dataset.mode==="claim"){
    if(values.internal_count!==undefined && Number(values.internal_count)+Number(values.external_count)===0){
      throw new Error("内部人数和外部人数至少有一项大于 0。");
    }
    if(Number(values.external_count)>0 && values.external_org===""){
      throw new Error("有外部人员参加时，请填写招待对象单位。");
    }
    return Object.entries(values).filter(([,value])=>value!=="").map(([key,value])=>key+"="+value).join("\n");
  }
  const category = editor.dataset.category;
  const [a,b,c,d,e] = Object.values(values);
  const rawAmount = category==="transport" || category==="lodging" ? e : c;
  if(!/^\d+(?:\.\d{1,2})?$/.test(rawAmount) || !Number.isFinite(Number(rawAmount))){
    throw new Error("请填写合计金额，单位为元，最多两位小数。");
  }
  const amount = Number(rawAmount).toFixed(2);
  if(category==="transport"){
    const flight = editor.querySelector(".supMode").value==="flight";
    return [flight?"航空客票行程单":"铁路客票行程单",
      (flight?"旅客姓名：":"乘车人：")+a,(flight?"乘机日期：":"乘车日期：")+b,
      (flight?"航班：":"车次：")+c,(flight?"舱位：":"席别：")+d,"合计：¥"+amount].join("\n");
  }
  if(category==="lodging"){
    if(b>=c || Number(d)<1) throw new Error("请检查入住、离店日期和实际住宿晚数。");
    return ["酒店水单","入住人："+a,"入住日期："+b,"离店日期："+c,"共"+d+"晚","合计：¥"+amount].join("\n");
  }
  return ["餐饮消费清单","消费日期："+a,b,"合计：¥"+amount].join("\n");
}

function auditSummary(rep){
  const r = rep.result;
  if(r.decision === "approve") return "通过。";
  if(r.decision === "approve_with_adjustment") return "调整后通过，核准 " + yuan(r.approved_fen) + "。";
  if(r.decision === "manual_review"){
    const hasMissingFields = (r.hits || []).includes("R15-b");
    return hasMissingFields
      ? "待人工复审。发票有关键字段未能可靠确认。"
      : "待人工复审。部分核验未完成或结果需要确认。";
  }
  const reasons = (r.findings || [])
    .filter(f => f.outcome === r.decision && f.reason)
    .slice(0,2).map(f => f.reason);
  const prefix = r.decision === "need_more_info" ? "暂未通过，需要补材料。" : "不通过。";
  return reasons.length ? prefix + "\n" + reasons.join("\n") : prefix;
}

function showAuditTurn(turn, rep, reviewContext){
  finishAuditTurn(turn, auditSummary(rep));
  const actions = document.createElement("div");
  actions.className = "chat-actions";
  const addAction = (label, onClick) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "ghost";
    button.textContent = label;
    button.onclick = onClick;
    actions.append(button);
  };
  const show = manual => {
    if(S.running) return;
    setWorkbench(true);
    S.evCount = 0;
    $("#log").innerHTML = "";
    for(const ev of rep.events || []) addEvent(ev);
    showReport(rep, reviewContext);
    requestAnimationFrame(() => {
      const target = manual ? $("#manualReviewTitle") : $("#businessPane");
      target?.scrollIntoView({block:"start"});
      if(manual) target?.focus({preventScroll:true});
    });
  };
  addAction("详情", () => show(false));
  addAction("人工复审", () => show(true));
  if(reviewContext.taskID && rep.result.decision === "need_more_info"){
    addAction("去补材料", () => {
      if(S.running) return;
      setWorkbench(true);
      switchTab("tasks");
      openTask(reviewContext.taskID);
      $("#businessPane").scrollIntoView({block:"start"});
    });
  }
  turn.row.append(actions);
}
