// focus.go 快过期积分聚焦（pool.expiring_focus）：把流量压到「最快到期」的那个账号上，
// 让快过期的额度先被烧掉。
//
// **没有锁定状态**：每次选号都按当前快照现算一次目标，不保存选择结果。这样做的理由：
//
//  1. 排序主轴是 creditsEarliestExpiry（到期时刻）。它不随花钱漂移，只有「这一批烧光」
//     时才跳到下一批，所以实时重算不会逐笔翻转——不需要用锁来钉住目标。
//     （早期版本用「窗口内金额」当主轴，那个值每笔 NoteModelCost 都在减，两个号接近时
//     目标会 A 烧一笔→B 反超→B 烧一笔→A 反超 地来回翻转，当时才引入锁。）
//  2. 锁自己有反方向的病：锁住以后，池里新出现一个更快要过期的号也抢不走它，得等当前
//     那个烧完——而这恰好是「先用快过期的积分」要避免的情形。
//
// 与「独占」的关系：目标是确定性排序的头名，快照不变期间所有请求自然落在同一个号上，
// 一直烧到它窗口内那批积分归零，下一次选号自动换到下一个。独占是排序的**结果**，不是
// 需要额外维护的状态。
//
// 与显式配置的关系：人的配置永远优先。本功能在 prioritizeLocked **之后**生效，且候选
// 里存在 account_share 账号、或选号策略非 weighted 时整体让位。它也不能绕过健康/冷却/
// 在途/积分保底/停牌——那些都在候选收集阶段就逐出了。
package pool

import (
	"log"
	"time"
)

// SetExpiringFocus 注入快过期积分聚焦开关（pool.expiring_focus，默认关）。
// 关闭 → 清掉「上次烧的是谁」的观测标记，回到纯加权路由；开启 → 下一次选号即生效。
// 只在开关**发生变化**时清：saveConfig 的热应用每次保存配置都会调本方法，无条件清空会
// 让「随手保存一个无关配置」也抹掉面板上的观测值。
func (p *Pool) SetExpiringFocus(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.expiringFocus == enabled {
		return
	}
	p.expiringFocus = enabled
	p.expiringFocusUID = map[string]string{}
	p.expiringFocusDry = map[string]bool{}
}

// focusExpiringLocked 把候选收窄到「最快到期」的那一个号；无法聚焦时原样返回。
//
// 名字里的 Locked 指「调用方须持 p.mu」（包内 ...Locked 后缀的统一含义），与「锁定
// 语义」无关——那个语义已经删除了。
//
// 让位顺序（都在算目标之前）：显式选号策略非 weighted → 整体让位（轮询/顺序耗尽的语义
// 是「按环分摊 / 榨干一个再换」，与「全压一个号」互斥，不让位会把环收窄成单号而静默
// 失效）；候选里存在 account_share 账号 → 整体让位（占比是显式分配偏好，收窄后
// 「70/30 分流量」会静默变成独占）。
//
// 判据用**成本分层前的全集 all** 而不是本层 layer：share 号落在别的成本层时，只看
// layer 会判定「没有占比配置」，于是聚焦生效、候选只剩目标号，sharePoolLocked 随后
// 无从生效。
func (p *Pool) focusExpiringLocked(layer, all []*entry, realm string, now time.Time) []*entry {
	if !p.expiringFocus || len(layer) == 0 {
		return layer
	}
	if p.strategy != "" && p.strategy != StrategyWeighted {
		return layer
	}
	if p.shareCandidateLocked(all) {
		return layer
	}
	best := p.focusCandidateLocked(layer, now)
	if best == nil {
		// 没有任何号持有窗口内的快过期积分 → 不聚焦，行为与未开一致。这一支是
		// 「功能在跑但整池停摆」，与「一直没积分」外观相同，故留一条日志。
		p.noteFocusDryLocked(realm, len(layer))
		p.noteFocusTargetLocked(realm, "")
		return layer
	}
	delete(p.expiringFocusDry, realm) // 恢复：下次再停摆时重新告警一次
	p.noteFocusTargetLocked(realm, best.a.UID)
	return []*entry{best}
}

// shareCandidateLocked 报告候选里是否存在配了占比的账号（无 → false）。
// 与 sharePoolLocked 同一判据，但只回答「有没有」，供聚焦让位判定用。调用方须持 p.mu。
func (p *Pool) shareCandidateLocked(cands []*entry) bool {
	for _, e := range cands {
		if p.shareOf(e.a.UID) > 0 {
			return true
		}
	}
	return false
}

// focusCandidateLocked 选出聚焦目标：窗口内快过期积分**最快要到期**者。
//
// 快路径只读 creditsExpiring / creditsEarliestExpiry，热路径零成本。**全零时走一次
// 慢路径**：用批次明细按当前窗口重算并回写（refreshExpiringFromBucketsLocked）。
//
// 慢路径要解决的不是精度，是「空转」：creditsExpiring 是刷新那一刻算的快照，批次滑进
// 窗口不会触发重算，于是整池字段全是 0 时本函数返回 nil，聚焦静默退回普通加权、一点
// 痕迹都没有。只在全零时才补算——只要有一个号有值，需求落在它身上就已经达成「烧快过期
// 的积分」这个目的。
//
// 调用方须持 p.mu。
func (p *Pool) focusCandidateLocked(cands []*entry, now time.Time) *entry {
	if best := focusBest(cands); best != nil {
		return best
	}
	recomputed := false
	for _, e := range cands {
		if p.refreshExpiringFromBucketsLocked(e, now) > 0 {
			recomputed = true
		}
	}
	if !recomputed {
		return nil // 连明细里都没有：确实没有快过期积分，不是快照滞后
	}
	return focusBest(cands)
}

// focusBest 纯读择优，供快慢两条路径共用。
func focusBest(cands []*entry) *entry {
	var best *entry
	for _, e := range cands {
		if e.creditsExpiring <= 0 {
			continue // 窗口内没有快过期积分（含从未刷新过余额的号）：不参与
		}
		if best == nil || focusBetter(e, best) {
			best = e
		}
	}
	return best
}

// noteFocusDryLocked 聚焦「本域一个候选都没有」的一次性告警。
//
// 去抖到「状态变化」：某 realm 从有候选变成零候选时打一条，恢复后静默清标记，下次再
// 停摆时重新打。为什么不每次都打：整批积分烧完、或本来就没积分要过期时全池为 0 是
// 正常形态，逐请求打会把日志刷满。
// 为什么要有这条：停摆的唯一后果是「一批积分到期前没被烧掉」，要等账单出来才看得见；
// 而日志里那行一出现，就知道是「窗口太长/刷新太稀」这类可调的东西，不是功能坏了。
// 调用方须持 p.mu。
func (p *Pool) noteFocusDryLocked(realm string, candidates int) {
	if p.expiringFocusDry[realm] {
		return
	}
	if p.expiringFocusDry == nil { // 包内直接构造 Pool 时可能是 nil
		p.expiringFocusDry = map[string]bool{}
	}
	p.expiringFocusDry[realm] = true
	r := realm
	if r == "" { // 与 Auth.Realm 的归一化口径一致，避免日志里出现 realm=
		r = "cn"
	}
	log.Printf("INFO: [pool] 快过期积分聚焦：realm=%s 快过期窗口 %s 内没有任何账号持有快过期积分（候选 %d 个）→ 本次起退回普通加权，直到有积分进窗口；若积分确实在近期到期，考虑调大 expiring_soon",
		r, p.expiringWindow, candidates)
}

// noteFocusTargetLocked 记录「本次选号落在谁身上」，**只供 Status 透出**
// （面板 focus_locked：让「配置有没有真的生效、现在烧的是谁」能自证）。
// 不参与任何决策——判断目标永远现算。uid 为空表示本次没有落在任何账号上。
// 调用方须持 p.mu。
func (p *Pool) noteFocusTargetLocked(realm, uid string) {
	if uid == "" {
		delete(p.expiringFocusUID, realm)
		return
	}
	if p.expiringFocusUID == nil { // 包内直接构造 Pool 时可能是 nil
		p.expiringFocusUID = map[string]string{}
	}
	p.expiringFocusUID[realm] = uid
}

// focusBetter 报告 a 是否比 b 更适合当聚焦目标（判据见 focusCandidateLocked）。
//
// 主轴是**到期时刻**：谁先到期谁先烧，这正是本功能与 expiringUrgency 的分工——后者
// 只把「窗口内有没有快过期批次」折成选号权重（×N），不排序、也不看金额。
// 到期时刻相同（常见于同一批签到积分）时才比窗口内金额，金额多者先烧；最后用 UID
// 兜底，保证纯确定性——随机挑目标会让相邻两次请求在并列候选间换人。
func focusBetter(a, b *entry) bool {
	ae, be := a.creditsEarliestExpiry, b.creditsEarliestExpiry
	switch {
	case ae.IsZero() != be.IsZero():
		return !ae.IsZero() // 有到期时刻的优先：零值意味着「不知道哪批先到期」
	case !ae.Equal(be):
		return ae.Before(be) // 主轴：先到期者先烧
	}
	if a.creditsExpiring != b.creditsExpiring {
		return a.creditsExpiring > b.creditsExpiring // 同到期时刻：金额多的先烧
	}
	return a.a.UID < b.a.UID
}

// isFocusLocked 报告该账号是否是「上一次选号」的聚焦目标（Status 透出用）。
// 只读观测表，代表最近一次实际落点，不是锁定状态。调用方须持 p.mu。
func (p *Pool) isFocusLocked(uid string) bool {
	for _, v := range p.expiringFocusUID {
		if v == uid {
			return true
		}
	}
	return false
}
