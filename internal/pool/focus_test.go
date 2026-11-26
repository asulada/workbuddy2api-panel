// focus_test.go 快过期积分聚焦（pool.expiring_focus）的语义测试。
//
// 只测语义、不测实现：所有断言都用公开入口（Pick / SetCreditsDetailed / SetStrategy /
// SetAccountShare / SetAccountPriority）观察「流量落到谁身上」。这与
// accountprefs_test.go 同一哲学——内部的锁定表是手段，用户可见的是「谁在被烧」。
package pool

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// focusCredits 给账号写入「余额 + 窗口内快过期金额」。expiring 必须是 credits 的子集，
// 故余额按 expiring 抬足，避免 SetCreditsSnapshot 的 clamp 把金额削掉。
func focusCredits(p *Pool, uid string, expiring int64, expiresIn time.Duration) {
	credits := expiring
	if credits < 1000 {
		credits = 1000
	}
	at := time.Time{}
	if expiresIn > 0 {
		at = time.Now().Add(expiresIn)
	}
	p.SetCreditsDetailed(uid, credits, credits, expiring, at, expiring)
}

// TestExpiringFocusBurnsSingleAccount：开启后流量全部落到「最快到期」的那个号，而不是
// 余额最多的号——这正是它与加权路由的区别。
func TestExpiringFocusBurnsSingleAccount(t *testing.T) {
	withNoPickGap(t)
	// 余额故意让 u0 最多：不开聚焦时 u0 才是加权首选（下面的基线用例会验证）。
	p := mkPool(3, 100000, 1000, 1000)
	focusCredits(p, "u1", 500, 24*time.Hour) // 最先到期
	focusCredits(p, "u2", 100, 48*time.Hour)

	p.SetExpiringFocus(true)
	if got := pickAll(p, 50); got["u1"] != 50 {
		t.Fatalf("聚焦应独占最先到期的 u1，实际 %v", got)
	}
}

// TestExpiringFocusPrefersEarliestDeadline：到期时刻是主轴。u0 金额大但两天后才到期，
// u1 只有 100 分但两小时后到期 —— 先烧 u1。这条同时钉住「本功能 ≠ expiringUrgency」：
// 后者只把「窗口内有没有快过期批次」折成权重倍率，与金额、先后都无关。
func TestExpiringFocusPrefersEarliestDeadline(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 1000, 1000)
	focusCredits(p, "u0", 500, 72*time.Hour) // 金额大、到期晚
	focusCredits(p, "u1", 100, 2*time.Hour)  // 金额小、先到期

	p.SetExpiringFocus(true)
	if got := pickAll(p, 40); got["u1"] != 40 {
		t.Fatalf("应先烧最快要到期的 u1（哪怕 u0 金额更多），实际 %v", got)
	}
	// 状态透出：现在烧的是谁必须可观测，否则「开了但不知道烧的是谁」。
	if st, ok := p.Status("u1"); !ok || !st.FocusLocked {
		t.Fatalf("u1 应标记 focus_locked，实际 %+v", st)
	}
	if st, ok := p.Status("u0"); !ok || st.FocusLocked {
		t.Fatalf("u0 不该标记 focus_locked，实际 %+v", st)
	}
}

// TestExpiringFocusHandsOffWhenBurned：窗口内额度烧光（credits_expiring 归零）后，
// 下一次选号自动落到下一个持有快过期积分的号上——不需要任何「交棒」判定，因为目标
// 是现算的。这也是无锁设计的自愈点：烧空的号自然退出候选，流量不会一直压在它身上。
func TestExpiringFocusHandsOffWhenBurned(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	focusCredits(p, "u1", 500, 24*time.Hour) // 最先到期
	focusCredits(p, "u2", 300, 48*time.Hour)
	p.SetExpiringFocus(true)

	if got := pickAll(p, 10); got["u1"] != 10 {
		t.Fatalf("应先烧 u1，实际 %v", got)
	}
	// 模拟「u1 的窗口内批次已烧完」：余额仍在，只是 credits_expiring 归零。
	focusCredits(p, "u1", 0, 0)
	if got := pickAll(p, 10); got["u2"] != 10 {
		t.Fatalf("u1 烧光后应改烧 u2，实际 %v", got)
	}
	// 观测标记跟着换人：u1 不再标记，u2 标记。
	if st, _ := p.Status("u1"); st.FocusLocked {
		t.Fatalf("u1 烧光后不该仍标记 focus_locked: %+v", st)
	}
	if st, _ := p.Status("u2"); !st.FocusLocked {
		t.Fatalf("u2 应接任 focus_locked: %+v", st)
	}
}

// TestExpiringFocusHandsOffWhenUnavailable：锁定号被逐出候选（这里用冷却模拟烧到阈值 /
// 停牌 / 手冻结等一切「不可选」）时，必须立刻让位，而不是把请求饿死。
func TestExpiringFocusHandsOffWhenUnavailable(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 1000, 1000)
	focusCredits(p, "u0", 500, 48*time.Hour)
	focusCredits(p, "u1", 100, 48*time.Hour)
	p.SetExpiringFocus(true)

	if got := pickAll(p, 5); got["u0"] != 5 {
		t.Fatalf("应先锁 u0，实际 %v", got)
	}
	p.Cooldown("u0", CoolSoft, time.Hour, "test")
	if got := pickAll(p, 5); got["u1"] != 5 {
		t.Fatalf("锁定号冷却后应让位给 u1，实际 %v", got)
	}
}

// TestExpiringFocusOffIsUnchanged：开关关闭时选号与「从未调用过该 Setter」逐笔一致。
// 这是零回归的硬约束：新增一个默认关的功能不能改变任何既有部署的流量分布。
func TestExpiringFocusOffIsUnchanged(t *testing.T) {
	withNoPickGap(t)
	build := func() *Pool {
		p := mkPool(3, 100000, 1000, 1000)
		focusCredits(p, "u1", 500, 48*time.Hour)
		focusCredits(p, "u2", 100, 24*time.Hour)
		p.SetRandomSource(func(n int64) int64 { return 0 }) // 固定抽签，结果可比
		return p
	}
	untouched := build()
	explicitOff := build()
	explicitOff.SetExpiringFocus(false)

	for i := 0; i < 20; i++ {
		a, b := untouched.Pick(), explicitOff.Pick()
		if a == nil || b == nil || a.UID != b.UID {
			t.Fatalf("iter %d: 关闭聚焦应与未引入该功能完全一致：%v vs %v", i, a, b)
		}
	}
	// 基线也说明「不开时不会独占」：u1 有快过期额度，靠 prefer_expiring 的软倍率会拿到
	// 一部分流量，但远远不是全部。否则上一个用例的独占断言就失去了意义。
	if got := pickAll(untouched, 100); got["u1"] >= 100 {
		t.Fatalf("关闭时不应出现 u1 独占，实际 %v", got)
	}
}

// TestExpiringFocusNoCandidatesIsUnchanged：池里没有任何号持有窗口内快过期积分时
// （典型：余额从未刷新 / 批次都在窗口外），聚焦不收窄，行为与关闭一致。
func TestExpiringFocusNoCandidatesIsUnchanged(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 100000, 1000, 1000) // 全池 credits_expiring 恒 0
	p.SetExpiringFocus(true)

	got := pickAll(p, 300)
	// 不收窄 = 退回普通加权：余额最多的 u0 占多数，但另外两个号照样分得到流量
	// （闲置补偿会拉平部分差距）。断言「有份额」而不是具体比例，避免绑死权重公式。
	if got["u0"] <= got["u1"] || got["u1"] == 0 || got["u2"] == 0 {
		t.Fatalf("无快过期批次时应退回普通加权（多号分摊，u0 居多），实际 %v", got)
	}
	for _, uid := range []string{"u0", "u1", "u2"} {
		if st, _ := p.Status(uid); st.FocusLocked {
			t.Fatalf("没有可聚焦目标时不该锁定任何号，%s 却标记了: %+v", uid, st)
		}
	}
}

// TestExpiringFocusYieldsToShare：account_share 是显式分配偏好，压过自动聚焦。
// 若聚焦先收窄，share 池只剩目标号，「70/30 分流量」会静默变成独占。
func TestExpiringFocusYieldsToShare(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	focusCredits(p, "u1", 500, 48*time.Hour) // 按聚焦本该独占它
	p.SetAccountShare(map[string]float64{"u0": 3, "u2": 1})
	p.SetExpiringFocus(true)

	got := pickAll(p, 800)
	if got["u0"] == 0 || got["u2"] == 0 {
		t.Fatalf("配了占比时聚焦应整体让位，实际 %v", got)
	}
	if got["u0"] < 500 || got["u0"] > 700 {
		t.Fatalf("占比 3:1 应分出约 600/200，实际 %v", got)
	}
}

// TestExpiringFocusYieldsToStrategy：轮询/顺序耗尽的语义是「按环分摊 / 榨干一个再换」，
// 与「全部压到一个号」互斥，故非 weighted 策略存在时聚焦让位。
func TestExpiringFocusYieldsToStrategy(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	focusCredits(p, "u2", 500, 48*time.Hour)
	p.SetStrategy(StrategyRoundRobin)
	p.SetExpiringFocus(true)

	got := pickAll(p, 30)
	if got["u0"] == 0 || got["u1"] == 0 {
		t.Fatalf("轮询策略下聚焦应让位（环上每个号都要分到），实际 %v", got)
	}
}

// TestExpiringFocusWithinPriorityLayer：account_priority 与聚焦**可叠加**——优先级
// 决定「先烧哪一层」，聚焦决定「层内先烧哪个号」。两者是不同的维度，不冲突：
// 优先级层外的号照样不接单，层内则按快过期金额挑目标。
func TestExpiringFocusWithinPriorityLayer(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 100000)
	focusCredits(p, "u0", 100, 48*time.Hour)
	focusCredits(p, "u1", 500, 48*time.Hour)
	focusCredits(p, "u2", 900, 48*time.Hour) // 金额最多，但在优先级层外
	p.SetAccountPriority(map[string]int{"u0": 1, "u1": 1})
	p.SetExpiringFocus(true)

	if got := pickAll(p, 30); got["u1"] != 30 {
		t.Fatalf("应在优先级层 {u0,u1} 内聚焦到金额多的 u1（u2 在层外不参与），实际 %v", got)
	}
}

// TestExpiringFocusRealmScoped：锁定按 realm 分桶。两域积分不同源，cn 请求锁的号
// 不该被 global 请求覆盖，反之亦然。
func TestExpiringFocusRealmScoped(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.SetCredits("cn1", 1000, 1000)
	p.SetCredits("cn2", 1000, 1000)
	p.SetCredits("g1", 1000, 1000)
	focusCredits(p, "cn2", 300, 48*time.Hour) // 只有 cn2 有快过期额度
	focusCredits(p, "g1", 200, 48*time.Hour)  // global 只有 g1 可选
	p.SetExpiringFocus(true)

	// 交替请求两个域：各自都应稳定落在本域目标上，互不干扰。
	for i := 0; i < 10; i++ {
		if a := p.PickExcludingForRealm(nil, "", "cn"); a == nil || a.UID != "cn2" {
			t.Fatalf("iter %d: cn 域应聚焦 cn2，实际 %v", i, a)
		}
		if a := p.PickExcludingForRealm(nil, "", "global"); a == nil || a.UID != "g1" {
			t.Fatalf("iter %d: global 域应聚焦 g1，实际 %v", i, a)
		}
	}
}

// TestSetExpiringFocusToggleClearsLock：关闭再打开会清空锁定并重选。
// 为什么需要：saveConfig 热应用每次都调 SetExpiringFocus，若无条件清空，
// 「随手保存一个无关配置」就会打乱当前目标——所以只在开关**变化**时清空。
func TestSetExpiringFocusToggleClearsLock(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	focusCredits(p, "u1", 500, 48*time.Hour)
	focusCredits(p, "u2", 100, 48*time.Hour)
	p.SetExpiringFocus(true)
	p.Pick()
	if st, _ := p.Status("u1"); !st.FocusLocked {
		t.Fatalf("开启后应锁定 u1: %+v", st)
	}

	// 重复设置同一值：不清空锁定（热应用的常见形态）。
	p.SetExpiringFocus(true)
	if st, _ := p.Status("u1"); !st.FocusLocked {
		t.Fatalf("重复设置同值不该清空锁定: %+v", st)
	}

	// 关闭：清空。
	p.SetExpiringFocus(false)
	for _, uid := range []string{"u1", "u2"} {
		if st, _ := p.Status(uid); st.FocusLocked {
			t.Fatalf("关闭后不该留有锁定（%s）: %+v", uid, st)
		}
	}
	// 再开启：重新按当下的金额选（此时 u2 已被抬到最多 → 锁 u2）。
	focusCredits(p, "u2", 900, 48*time.Hour)
	p.SetExpiringFocus(true)
	if got := pickAll(p, 5); got["u2"] != 5 {
		t.Fatalf("重新开启后应按新金额锁 u2，实际 %v", got)
	}
}

// TestExpiringFocusCostTierWins：成本分层优先于聚焦。快过期号因成本分层暂时不在最优层
// 时，请求交给更便宜的层，不强行把流量拉回它；额度还在，层恢复或别的号不可用时它自然
// 又被选中（目标现算，不需要「保锁」）。
func TestExpiringFocusCostTierWins(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 1000, 1000)
	p.SetCostExploreInterval(0) // 关掉 tier 探索改道：本用例只测分层
	focusCredits(p, "u0", 500, 48*time.Hour)
	p.SetExpiringFocus(true)

	if a := p.PickExcludingForModel(nil, "m"); a == nil || a.UID != "u0" {
		t.Fatalf("应先烧 u0，实际 %v", a)
	}
	// u1 实测免费 → tier 0 层只有 u1；u0 落在 tier 1。请求交给 u1（层优先）。
	p.NoteModelCost("u1", "m", 0, 100)
	if a := p.PickExcludingForModel(nil, "m"); a == nil || a.UID != "u1" {
		t.Fatalf("成本分层应优先（免费层 u1 接单），实际 %v", a)
	}
	// 本次没落到 u0，观测标记必须跟着走——否则面板会显示「正在烧 u0」而实际流量在别处。
	if st, _ := p.Status("u0"); st.FocusLocked {
		t.Fatalf("本次落点不是 u0，观测标记应已清掉: %+v", st)
	}
	// u1 不可用后，快过期额度还在的 u0 立刻回到唯一候选。
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	if a := p.PickExcludingForModel(nil, "m"); a == nil || a.UID != "u0" {
		t.Fatalf("u1 不可用后应继续烧 u0，实际 %v", a)
	}
}

// TestExpiringFocusInFlightFullYieldsThenReturns：目标号只是在途名额占满时，本次请求让给
// 同层其他号（不能把请求饿死），名额一释放立刻回到目标号——目标是现算的，不需要「保锁」。
func TestExpiringFocusInFlightFullYieldsThenReturns(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 1000, 1000)
	focusCredits(p, "u0", 500, 48*time.Hour)
	p.SetExpiringFocus(true)
	p.SetMaxInFlight(1)

	if a := p.Pick(); a == nil || a.UID != "u0" {
		t.Fatalf("应先烧 u0，实际 %v", a)
	}
	// 把 u0 的在途名额占满 → 本轮只能给 u1。
	if !p.Acquire("u0") {
		t.Fatal("Acquire(u0) 应成功")
	}
	if a := p.Pick(); a == nil || a.UID != "u1" {
		t.Fatalf("u0 在途满时应临时让位给 u1，实际 %v", a)
	}
	// 名额释放 → 立刻回到快过期号，而不是停在 u1。
	p.Release("u0")
	if a := p.Pick(); a == nil || a.UID != "u0" {
		t.Fatalf("名额释放后应回到 u0，实际 %v", a)
	}
}

// TestExpiringFocusHandsOffOnReserveBlocked：锁定号因保留积分停牌（余额见底）时要交棒。
// 与「在途满」相反：这不是下一轮就会消失的状态，继续锁定只会每个请求都白算一次，
// 且真请求会一直落到别的号上——那不如直接换目标，语义也更诚实。
func TestExpiringFocusHandsOffOnReserveBlocked(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 1000, 1000)
	focusCredits(p, "u0", 500, 48*time.Hour)
	focusCredits(p, "u1", 300, 48*time.Hour)
	p.SetExpiringFocus(true)
	p.SetReserveCredits(100)

	if a := p.PickExcludingForModel(nil, "m"); a == nil || a.UID != "u0" {
		t.Fatalf("应先锁 u0，实际 %v", a)
	}
	// u0 余额跌到保留线以下（creditsTotal 已知）→ 停牌 → 交棒给 u1。
	p.SetCreditsSnapshot("u0", CreditsSnapshot{Remain: 50, Total: 1000, Expiring: 500,
		EarliestAt: time.Now().Add(48 * time.Hour), EarliestRemaining: 500})
	if a := p.PickExcludingForModel(nil, "m"); a == nil || a.UID != "u1" {
		t.Fatalf("u0 停牌后应交棒给 u1，实际 %v", a)
	}
	if st, _ := p.Status("u0"); st.FocusLocked {
		t.Fatalf("停牌属账号级不可用，应已交棒: %+v", st)
	}
}

// TestNewSessionUIDsConvergeToFocusTarget：新会话分配池只含最快到期的那个号，
// 而「已绑定会话可用性校验」用的列表（AvailableUIDsForModelRealm）保持全量。
// 两者必须分开：前者决定新会话去哪，后者决定老会话是否还合法——合并任一侧都会错。
func TestNewSessionUIDsConvergeToFocusTarget(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	focusCredits(p, "u1", 500, 48*time.Hour)
	p.SetExpiringFocus(true)
	p.SetPreferExpiring(false, 0) // 关掉虚拟实例权重，让列表长度可比

	// 不需要先跑一次 pick：目标现算，新会话直接就能收敛。
	newSessions := p.NewSessionUIDsForModelRealm("m", "")
	if len(newSessions) == 0 {
		t.Fatal("新会话分配池不该为空")
	}
	for _, uid := range newSessions {
		if uid != "u1" {
			t.Fatalf("新会话应只分到目标号 u1，实际列表 %v", newSessions)
		}
	}
	// 老会话的校验集必须仍是全量。
	if got := n(p.AvailableUIDsForModelRealm("m", "")); len(got) != 3 {
		t.Fatalf("老会话校验集必须保持全量（否则会瞬间全量迁移），实际 %v", got)
	}
}

// TestNewSessionFollowsEarliestDeadline：每个新会话都按**当下**的快照重算，不沿用上一次的
// 选择。所以池里一旦出现更快要过期的号，下一个新会话立刻改投它，不必等前一个烧完。
// 这正是「先用快过期的积分」而不是「锁定某个账号」的分界线。
func TestNewSessionFollowsEarliestDeadline(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	p.SetExpiringFocus(true)
	p.SetPreferExpiring(false, 0)
	focusCredits(p, "u1", 900, 72*time.Hour) // 金额大、到期远
	focusCredits(p, "u2", 100, 48*time.Hour) // 金额小、先到期

	if got := n(p.NewSessionUIDsForModelRealm("m", "")); len(got) != 1 || !got["u2"] {
		t.Fatalf("新会话应先分给最快要到期的 u2，实际 %v", got)
	}
	// u1 的批次变成更快到期（比如签到又补了一批 2 小时后到期的）→ 不用等 u2 烧完，
	// 下一个新会话就该改投 u1。
	focusCredits(p, "u1", 900, 2*time.Hour)
	if got := n(p.NewSessionUIDsForModelRealm("m", "")); len(got) != 1 || !got["u1"] {
		t.Fatalf("出现更快要到期的号后，新会话应立刻改投 u1，实际 %v", got)
	}
}

// TestNewSessionUIDsUnchangedWhenFocusOff：聚焦关闭时，新会话分配池与旧列表逐项一致。
func TestNewSessionUIDsUnchangedWhenFocusOff(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	focusCredits(p, "u1", 500, 48*time.Hour)

	got := p.NewSessionUIDsForModelRealm("m", "")
	want := p.WeightedAvailableUIDsForModelRealm("m", "")
	if len(got) != len(want) {
		t.Fatalf("关闭聚焦时新会话分配池应与旧列表一致：%v vs %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("关闭聚焦时新会话分配池应与旧列表一致：%v vs %v", got, want)
		}
	}
}

// TestNewSessionUIDsFallBackWhenTargetUnusable：目标号在该模型上不可用时，
// 新会话分配池回落为原列表（不能把新会话饿死）。
func TestNewSessionUIDsFallBackWhenTargetUnusable(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	focusCredits(p, "u1", 500, 48*time.Hour)
	p.SetExpiringFocus(true)
	p.Pick()

	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	got := p.NewSessionUIDsForModelRealm("m", "")
	if len(n(got)) < 2 {
		t.Fatalf("目标号不可用时新会话应回落全池，实际 %v", got)
	}
}

// n 去重计数（分配池可能因虚拟实例权重含重复 uid）。
func n(uids []string) map[string]bool {
	m := make(map[string]bool, len(uids))
	for _, u := range uids {
		m[u] = true
	}
	return m
}

// focusBucket 写一份「只有按日批次、窗口内金额为 0」的余额快照，模拟真实形态：
// 刷新那一刻这批还在窗口外（expiring = 0），之后才滑进窗口；而 creditsExpiring 是
// 快照，不会随时间推进自动重算。
func focusBucket(p *Pool, uid string, credits int64, at time.Time, remain int64) {
	p.SetCreditsSnapshot(uid, CreditsSnapshot{
		Remain:        credits,
		Total:         credits,
		ExpiryBuckets: []ExpiryBucket{{Date: at.Format("2006-01-02"), ExpireAt: at, Remain: remain}},
	})
}

// TestExpiringFocusSlowPathSeesBucketCrossedIntoWindow：全池 creditsExpiring 都是 0、
// 但批次明细里确实有一批已进窗口时，慢路径要用明细现算并**回写**——否则第一个请求
// 选中它、第二个请求就因「字段还是 0」判成烧光而交棒，流量在两个号之间来回抖。
func TestExpiringFocusSlowPathSeesBucketCrossedIntoWindow(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 1000, 1000)
	p.SetPreferExpiring(true, 48*time.Hour) // 慢路径要窗口：未注入（<=0）时无从重算
	focusBucket(p, "u1", 1000, time.Now().Add(12*time.Hour), 500)
	p.SetExpiringFocus(true)

	if got := pickAll(p, 20); got["u1"] != 20 {
		t.Fatalf("批次已进窗口时聚焦应独占 u1，实际 %v", got)
	}
	if st, _ := p.Status("u1"); st.CreditsExpiring != 500 {
		t.Fatalf("慢路径应把重算值回写到字段（交棒判定/会话分配/面板都读它），实际 %d", st.CreditsExpiring)
	}
	if st, _ := p.Status("u0"); st.FocusLocked {
		t.Fatalf("u0 不该被标成聚焦目标：%+v", st)
	}
}

// TestExpiringFocusSlowPathClampsBucketSumToCredits：上游「总余额已扣、批次明细没跟着
// 扣」时批次合计会大于余额，重算必须按 SetCreditsSnapshot 同一口径钳到 credits，
// 否则脏数据直接决定目标（两个号到期时刻相同，此时比的就是金额）。
func TestExpiringFocusSlowPathClampsBucketSumToCredits(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(3, 1000, 1000, 1000)
	p.SetPreferExpiring(true, 48*time.Hour)
	at := time.Now().Add(12 * time.Hour) // 两个号同一到期时刻
	focusBucket(p, "u1", 400, at, 900)   // 脏：批次 900 > 余额 400
	focusBucket(p, "u2", 1000, at, 500)  // 干净：500
	p.SetExpiringFocus(true)

	if got := pickAll(p, 20); got["u2"] != 20 {
		t.Fatalf("钳到总余额后 u2(500) 应胜过 u1(400)，实际 %v", got)
	}
	if st, _ := p.Status("u1"); st.CreditsExpiring != 400 {
		t.Fatalf("批次合计应钳到余额 400，实际 %d", st.CreditsExpiring)
	}
}

// TestExpiringFocusNoCandidateFallsBackAndLogsOnce：整池确实没有快过期积分时，聚焦
// 退回普通加权（不能把流量锁死在一个号上），并且只告警一条——去抖到「状态变化」，
// 否则逐请求打会把日志刷满。
func TestExpiringFocusNoCandidateFallsBackAndLogsOnce(t *testing.T) {
	withNoPickGap(t)
	p := mkPool(2, 1000, 1000)
	p.SetPreferExpiring(true, 48*time.Hour)
	p.SetExpiringFocus(true)

	prev := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	got := pickAll(p, 30)
	if len(got) != 2 {
		t.Fatalf("零候选时应退回普通加权（两号都分到流量），实际 %v", got)
	}
	if c := strings.Count(buf.String(), "快过期积分聚焦"); c != 1 {
		t.Fatalf("停摆只应告警一次，实际 %d 次：%s", c, buf.String())
	}
}
