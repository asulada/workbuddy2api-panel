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
// app.js 里没有任何渲染代码，用户永远看到一张空表。测试钉住四件事：
//   - series 点被渲染成行，且按时间**降序**（最新时段在最上面）——后端 series 是
//     「日桶升序 + 小时桶升序」两段拼接，渲染必须与输入顺序解耦；
//   - 小时桶标签带日期（MM-DD HH:00，跨天窗口下只有 HH:00 分不清是哪天），日桶
//     是 MM-DD；时间解析不出来的点丢弃而不是渲染出 NaN；
//   - 没有积分样本的时段「积分」列显示 —（没观测到 ≠ 0），有样本时 token/积分
//     = 合计 Token ÷ 实测积分；
//   - 按账号视图吃 series_by_account：多一列账号（昵称优先、uid 前 8 位兜底），
//     同一时段内按合计 Token 降序，表头列数随之变成 9。
func TestUsageHoursFrontendRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf('/* renderUsageHours'),end=src.indexOf('/* fmtTokTimeLabel');
assert(start>=0&&end>start,'renderUsageHours 区段定位失败');
const hosts={usHourBody:{innerHTML:''},usHourHead:{innerHTML:''}};
const ctx={$:id=>hosts[id],
 esc:s=>String(s),
 fmtTok:n=>String(Math.round(Number(n||0))),
 fmtCredit:n=>Number(n).toFixed(2),
 remarkName:(u,h)=>h};
vm.createContext(ctx);vm.runInContext(src.slice(start,end),ctx);
// 整体视图：输入故意乱序（日桶与小时桶交错），渲染必须自己按时间降序排。
ctx.renderUsageHours({series:[
 {t:'2026-09-15',scope:'day',requests:99,prompt_tokens:900,completion_tokens:100,total_tokens:1000,credits:20},
 {t:'2026-09-16T13',scope:'hour',requests:10,prompt_tokens:100,completion_tokens:50,total_tokens:150,credits:3},
 {t:'garbage',scope:'hour',requests:7},
 {t:'2026-09-16T12',scope:'hour',requests:5,prompt_tokens:40,completion_tokens:10,total_tokens:50,credits:0},
]});
let html=hosts.usHourBody.innerHTML;
const rows=html.split('<tr>').length-1;
assert.equal(rows,3,'坏时间点应丢弃，3 行：'+html);
assert(html.indexOf('>9-16 13:00<')<html.indexOf('>9-16 12:00<'),'最新小时桶应在最前');
assert(html.indexOf('>9-16 12:00<')<html.indexOf('<td>9-15</td>'),'小时桶应排在更早的日桶之前');
assert(html.includes('<td>9-16 13:00</td>'),'小时桶标签带日期');
assert(html.includes('<td>9-15</td>'),'日桶标签为 MM-DD');
assert(html.includes('<td class="num">—</td>'),'无积分样本显示 —');
assert(html.includes('<td class="num">50</td>'),'token/积分 = 150÷3 = 50');
assert(html.includes('<td class="num">3.00</td>'),'积分列走 fmtCredit');
assert(!html.includes('账号'),'整体视图不该有账号列');
assert.equal(hosts.usHourHead.innerHTML.split('<th').length-1,8,'整体视图表头应为 8 列');
// 按账号视图：同一时段内按合计 Token 降序；账号列昵称优先、uid 前 8 位兜底。
vm.runInContext('usHourDim="account"',ctx);
ctx.renderUsageHours({series:[{t:'2026-09-16T13',scope:'hour',requests:10}],
 series_by_account:[
  {t:'2026-09-16T13',scope:'hour',uid:'aaaaaaaa1111',nickname:'小号A',realm:'cn',requests:2,prompt_tokens:20,completion_tokens:10,total_tokens:30,credits:1},
  {t:'2026-09-16T13',scope:'hour',uid:'bbbbbbbb2222',realm:'global',requests:8,prompt_tokens:200,completion_tokens:100,total_tokens:300,credits:4},
  {t:'2026-09-16T12',scope:'hour',uid:'aaaaaaaa1111',requests:1,total_tokens:10},
  {t:'nope',scope:'hour',uid:'cccccccc3333',requests:1},
 ]});
html=hosts.usHourBody.innerHTML;
assert.equal(html.split('<tr>').length-1,3,'按账号视图：坏时间点丢弃，3 行：'+html);
assert(html.includes('小号A'),'昵称要出现在账号列：'+html);
assert(html.includes('aaaaaaaa'),'没昵称时用 uid 前 8 位兜底');
assert(html.indexOf('bbbbbbbb')<html.indexOf('小号A'),'同一时段内合计 Token 大的排前面');
assert.equal(hosts.usHourHead.innerHTML.split('<th').length-1,9,'按账号视图表头应为 9 列');
// 空数据：整行占位，colspan 必须等于表头列数（8），否则空表会错位。
hosts.usHourBody.innerHTML='';
vm.runInContext('usHourDim="total"',ctx);
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
