// Package judge 是报销审核的判定插件：读制度数据（policy），对一张单据给出结论与依据。
//
// # 这个包的边界
//
// 只依赖标准库。不 import 宿主的任何 internal 包，也不关心模型是哪个、工具走什么协议。
// 宿主负责三件事，都以接口形式注入：
//
//	InvoiceVerifier  发票查验
//	CompanyVerifier  企业核验
//	HistoryFinder    历史查重
//
// # 加一条规则要动哪几处
//
//  1. policy/policy.json 的 rules[] 里加一条，checks[] 里写清 id / when / outcome / requires；
//  2. judge/checks_*.go 里对应区间加一段判定（按规则号分段：R01–R05、R06–R10、R11–R15）；
//  3. fixtures/cases 里加一条或几条用例，在 truth.json 的 hits 写明预期命中项；
//  4. 跑 tools/policy-check（结构合法性）与 tools/conformance（判定与标准答案一致性）。
//
// 第 2 步漏了，第 4 步会报"规则表里有、代码里没实现"；第 1 步漏了，第 4 步会报"代码里有、规则表里没有"。
// 两个方向都能被抓住。
//
// # 最容易搞错的地方
//
// Verdict 的四种状态语义不同，混用会出事故：
//
//	Found     查到          可以据此判定
//	NotFound  查无          可以据此判定（"查无此票"本身就是驳回依据）
//	Failed    查询失败      不可据此判定，转人工
//	Timeout   超时          不可据此判定，转人工；它既不是"查到"也不是"查无"
//
// 早年把超时当"查无"，系统就会拿"税务平台查无此票"去驳回一张其实正常的发票。
// 现在 Usable() 只对 Found/NotFound 返回 true，写错会在编译期或测试期暴露。
package judge
