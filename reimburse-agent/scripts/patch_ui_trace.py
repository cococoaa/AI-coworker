import io

p = "internal/server/web/index.html"
s = io.open(p, encoding="utf-8").read()

OLD = '''    <h3 style="font-size:12px;color:var(--faint);margin:14px 0 4px">抽取字段与置信度</h3>
    <table class="fieldtable"><thead><tr><th>字段</th><th>合并值</th><th>置信度</th></tr></thead><tbody>${fields}</tbody></table>'''
NEW = '''    <h3 style="font-size:12px;color:var(--faint);margin:14px 0 4px">抽取字段与置信度</h3>
    <table class="fieldtable"><thead><tr><th>字段</th><th>合并值</th><th>置信度</th></tr></thead><tbody>${fields}</tbody></table>
    ${traceBox}'''
assert OLD in s
s = s.replace(OLD, NEW)

OLD2 = '''  const tools = Object.entries(rep.tools||{}).map(([k,v])=>{'''
NEW2 = '''  // 字段溯源：每个值回原件里指出处。找不到的单独标黄——那可能不是错，但必须让人看一眼。
  const traceBox = (rep.field_trace||[]).length ? (()=>{
    const srcName = {invoice:"发票原文", support:"仅佐证凭证", none:"原件未找到", empty:"空值"};
    const rows = rep.field_trace.map(t=>{
      const cls = t.source==="none" ? "nf" : t.source==="support" ? "sup" : "";
      return `<tr class="${cls}">
        <td class="f">${esc(t.label)}</td>
        <td class="v">${esc(t.value||"—")}
          ${t.snippet?`<div class="q">${esc(t.snippet)}</div>`:""}
          ${t.note?`<div class="q" style="color:var(--warn)">${esc(t.note)}</div>`:""}</td>
        <td><span class="srcbadge ${t.source}">${srcName[t.source]||t.source}</span></td>
      </tr>`;
    }).join("");
    const nf = rep.field_trace.filter(t=>t.source==="none").length;
    return `<h3 style="font-size:12px;color:var(--faint);margin:14px 0 4px">字段溯源
      ${nf?`<span style="color:var(--warn)">（${nf} 个在原件中未找到原文）</span>`:""}</h3>
      <div class="muted" style="margin-bottom:4px">每个值都回到送进模型的那份原文里找过，并摘出上下文。
        「仅佐证凭证」表示发票上没有、只见于辅助材料；「原件未找到」可能是格式改写，也可能是模型补全，建议人工核对。</div>
      <table class="tracetable"><tbody>${rows}</tbody></table>`;
  })() : "";

  const tools = Object.entries(rep.tools||{}).map(([k,v])=>{'''
assert OLD2 in s
s = s.replace(OLD2, NEW2)

io.open(p, "w", encoding="utf-8").write(s)
print("ok")
