package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUsageHoursFrontendRender 「积分与 token 按小时」表的渲染回归。
//
// 为什么要钉：这张表此前只有 index.html 里的静态表头（tbody id=usHourBody），
// app.js 里没有任何渲染代码，用户永远看到一张空表。测试钉住五件事：
//   - series 点被渲染成行，且按时间**降序**（最新时段在最上面）——后端 series 是
//     「日桶升序 + 小时桶升序」两段拼接，渲染必须与输入顺序解耦；
//   - 小时桶标签带日期（MM-DD HH:00，跨天窗口下只有 HH:00 分不清是哪天），日桶
//     是 MM-DD；时间解析不出来的点丢弃而不是渲染出 NaN；
//   - 没有积分样本的时段「积分」列显示 —（没观测到 ≠ 0），有样本时 token/积分
//     = 合计 Token ÷ 实测积分；
//   - 账号下拉：选项来自 by_account ∪ series_by_account（昵称优先、uid 前 8 位兜底），
//     选中某个号只保留该号的行；选中的号在本窗口里不存在时回落「全部账号」，
//     不留一张空表；
//   - 两个下拉（时序 / 按小时表）共用同一份选择：usSetAcct 改一个，另一个跟着变。
func TestUsageHoursFrontendRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf('/* renderUsageHours'),end=src.indexOf('/* fmtTokTimeLabel');
assert(start>=0&&end>start,'renderUsageHours 区段定位失败');
const hosts={usHourBody:{innerHTML:''},usChartNote:{textContent:''},
 usHourAcct:{innerHTML:'',value:'',dataset:{}},usChartAcct:{innerHTML:'',value:'',dataset:{}}};
let chartSeries=null;
const ctx={$:id=>hosts[id],
 esc:s=>String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'),
 fmtTok:n=>String(Math.round(Number(n||0))),
 fmtCredit:n=>Number(n).toFixed(2),
 renderUsageChart:s=>{chartSeries=s;},
 usageData:null};
vm.createContext(ctx);vm.runInContext(src.slice(start,end),ctx);
const data={
 // 整体 series：输入故意乱序（日桶在中间），渲染必须自己按时间降序排。
 series:[
  {t:'2026-09-15',scope:'day',requests:99,prompt_tokens:900,completion_tokens:100,total_tokens:1000,credits:20},
  {t:'2026-09-16T13',scope:'hour',requests:10,prompt_tokens:100,completion_tokens:50,total_tokens:150,credits:3},
  {t:'garbage',scope:'hour',requests:7},
  {t:'2026-09-16T12',scope:'hour',requests:5,prompt_tokens:40,completion_tokens:10,total_tokens:50,credits:0}],
 by_account:[{key:'aaaaaaaa1111',realm:'cn',extra:'小号A'}],
 series_by_account:[
  {t:'2026-09-16T13',scope:'hour',uid:'aaaaaaaa1111',nickname:'小号A',realm:'cn',requests:2,prompt_tokens:20,completion_tokens:10,total_tokens:30,credits:1},
  {t:'2026-09-16T13',scope:'hour',uid:'bbbbbbbb2222',realm:'global',requests:8,prompt_tokens:200,completion_tokens:100,total_tokens:300,credits:4},
  {t:'2026-09-16T12',scope:'hour',uid:'aaaaaaaa1111',nickname:'小号A',realm:'cn',requests:1,total_tokens:10},
  {t:'nope',scope:'hour',uid:'cccccccc3333'}]};
ctx.usageData=data;
// ① 全部账号：坏时间点丢弃，3 行按时间降序。
ctx.renderUsageHours(data);
let html=hosts.usHourBody.innerHTML;
assert.equal(html.split('<tr>').length-1,3,'坏时间点应丢弃，3 行：'+html);
assert(html.indexOf('9-16 13:00')<html.indexOf('9-16 12:00'),'最新小时桶应在最前：'+html);
assert(html.indexOf('9-16 12:00')<html.indexOf('<td>9-15</td>'),'小时桶应排在更早的日桶之前');
assert(html.includes('<td>9-16 13:00</td>'),'小时桶标签带日期');
assert(html.includes('<td>9-15</td>'),'日桶标签为 MM-DD');
assert(html.includes('<td class="num">—</td>'),'无积分样本显示 —');
assert(html.includes('<td class="num">50</td>'),'token/积分 = 150÷3 = 50');
assert(html.includes('<td class="num">3.00</td>'),'积分列走 fmtCredit');
assert(!html.includes('小号A'),'账号不再占一列（由下拉筛选）：'+html);
// ② 下拉选项：全部账号 + 两个号（小号A 用昵称，另一个用 uid 前 8 位兜底）。
const opts=hosts.usHourAcct.innerHTML;
assert(opts.includes('<option value="">全部账号</option>'),'下拉首项应是全部账号：'+opts);
assert(opts.includes('小号A'),'选项要显示昵称：'+opts);
assert(opts.includes('>bbbbbbbb<'),'没昵称时用 uid 前 8 位兜底：'+opts);
assert.equal(hosts.usHourAcct.value,'','默认选中全部账号');
// ③ 选某个号：只剩该号的行（12:00 与 13:00 两条），仍按时间降序。
vm.runInContext('usAcctUID="aaaaaaaa1111"',ctx);
ctx.renderUsageHours(data);
html=hosts.usHourBody.innerHTML;
assert.equal(html.split('<tr>').length-1,2,'按账号筛应剩 2 行：'+html);
assert(!html.includes('<td>9-15</td>'),'日桶是别人/整体的点，不该出现：'+html);
assert(html.indexOf('9-16 13:00')<html.indexOf('9-16 12:00'),'筛后仍按时间降序');
// ④ 选中的号不在本窗口：回落全部账号，不留空表。
vm.runInContext('usAcctUID="zzzzzzzz9999"',ctx);
ctx.renderUsageHours(data);
assert.equal(hosts.usHourBody.innerHTML.split('<tr>').length-1,3,'号不在窗口内应回落全部账号：'+hosts.usHourBody.innerHTML);
assert.equal(hosts.usHourAcct.value,'','回落时下拉也回到全部账号');
// ⑤ 两个下拉共用一份选择：改一个，另一个跟着变，时序图拿到的是筛后的点。
ctx.usSetAcct('aaaaaaaa1111');
assert.equal(hosts.usChartAcct.value,'aaaaaaaa1111','时序下拉应同步');
assert.equal(hosts.usHourAcct.value,'aaaaaaaa1111','按小时下拉应同步');
assert.equal(chartSeries.length,2,'时序图应只拿该号的点：'+chartSeries.length);
assert(chartSeries.every(p=>p.uid==='aaaaaaaa1111'),'时序图不该混入其他号');
assert(hosts.usChartNote.textContent.includes('小号A'),'说明行应标出当前筛选的账号：'+hosts.usChartNote.textContent);
// ⑥ 空数据：整行占位，colspan 必须等于表头列数（8），否则空表会错位。
ctx.usSetAcct('');
ctx.renderUsageHours({});
assert.equal(hosts.usHourBody.innerHTML,'<tr><td colspan="8" class="empty">暂无数据</td></tr>');
console.log('usage hours frontend regression passed');`
	path := filepath.Join(t.TempDir(), "usage-hours.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
