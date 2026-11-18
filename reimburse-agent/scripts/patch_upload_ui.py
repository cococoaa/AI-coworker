import io

p = "internal/server/web/index.html"
s = io.open(p, encoding="utf-8").read()

# 1) 页签：加"上传票据"
OLD_TABS = '''      <button id="tabCases">全部单据 <span class="n" id="caseCount">0</span></button>
    </div>'''
NEW_TABS = '''      <button id="tabCases">全部单据 <span class="n" id="caseCount">0</span></button>
      <button id="tabUpload">上传票据</button>
    </div>'''
assert OLD_TABS in s
s = s.replace(OLD_TABS, NEW_TABS)

# 2) 上传面板
OLD_PANE = '''    <div id="casePane" style="display:none">
      <div class="filters" id="filters"></div>
      <div id="cases"></div>'''
NEW_PANE = '''    <div id="uploadPane" style="display:none">
      <div class="upform">
        <div class="uphint">上传电子发票（PDF 或文本）与佐证凭证，系统提取文字后建成一张可审核的单据。
          图片与扫描件需要 OCR，当前不支持。</div>
        <label class="f">报销单号<input id="up_claim_no" placeholder="BX26110999"></label>
        <label class="f">申请单号<input id="up_request_no" placeholder="SQ20261118001"></label>
        <label class="f">类别
          <select id="up_category">
            <option value="transport">交通</option>
            <option value="lodging">住宿</option>
            <option value="meal">餐饮</option>
          </select></label>
        <label class="f">申请人工号<input id="up_applicant_id" placeholder="E1001"></label>
        <label class="f">申请人姓名<input id="up_applicant_name" placeholder="张伟"></label>
        <label class="f">出发城市<input id="up_from_city" placeholder="上海"></label>
        <label class="f">到达城市<input id="up_city" placeholder="杭州"></label>
        <label class="f">开始日期<input id="up_start_date" type="date"></label>
        <label class="f">结束日期<input id="up_end_date" type="date"></label>
        <label class="f">事由<input id="up_purpose" placeholder="客户现场支持"></label>
        <label class="f">申请金额（元）<input id="up_amount" type="number" step="0.01" placeholder="73.00"></label>
        <label class="f">发票文件（必填）<input id="up_invoice" type="file" accept=".pdf,.txt,application/pdf,text/plain"></label>
        <label class="f">佐证凭证（可选）<input id="up_support" type="file" accept=".pdf,.txt,application/pdf,text/plain"></label>
        <button id="upBtn" class="primary" style="margin-top:8px">上传并建单</button>
        <div id="upResult" style="margin-top:10px"></div>
      </div>
    </div>
    <div id="casePane" style="display:none">
      <div class="filters" id="filters"></div>
      <div id="cases"></div>'''
assert OLD_PANE in s
s = s.replace(OLD_PANE, NEW_PANE)

# 3) 样式
OLD_CSS = '''.tabs button.on .n{background:#1f3346;color:#a8c7fa}'''
NEW_CSS = '''.tabs button.on .n{background:#1f3346;color:#a8c7fa}
.upform{padding:12px 14px;display:flex;flex-direction:column;gap:8px}
.upform .f{display:flex;flex-direction:column;gap:3px;font-size:11.5px;color:var(--faint)}
.upform .f input,.upform .f select{background:var(--panel2);border:1px solid var(--line);
  color:var(--fg);border-radius:5px;padding:5px 7px;font-size:12px;font-family:inherit}
.upform .f input[type=file]{padding:3px;font-size:11px}
.uphint{font-size:11.5px;color:var(--dim);line-height:1.6;padding:8px 10px;
  background:var(--panel2);border:1px solid var(--line);border-radius:6px}
.upok{background:#0d1f12;border:1px solid #1d4025;color:#7ee787;font-size:12px;
  padding:8px 10px;border-radius:6px;line-height:1.6}
.upbad{background:#22100f;border:1px solid #5a2321;color:#ff9b94;font-size:12px;
  padding:8px 10px;border-radius:6px;line-height:1.6}'''
assert OLD_CSS in s
s = s.replace(OLD_CSS, NEW_CSS)

io.open(p, "w", encoding="utf-8").write(s)
print("ok")
