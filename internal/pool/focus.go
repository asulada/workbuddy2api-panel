// focus.go 快过期积分聚焦（pool.expiring_focus）：把流量硬压到「窗口内快过期积分最多」
// 的那个账号上，让快过期的额度先被烧掉。
//
// 为什么需要它：prefer_expiring 只是权重上的软倍率（1.5~20），且倍率只由「剩余时间」
// 决定、**与批次金额无关**（见 expiringUrgency）——多号同时命中窗口时按概率分摊，
// 压不出「独占」。而 credit_floor / reserve_credits 是保命语义，不解决集中。
// 要真正做到「先把这个号的快过期积分烧完」，只能是硬分层：与 account_priority
// 同一档语义，区别只是目标由程序按金额自动选，不必手写 uid（手写的那份还随积分
// 格局变化而过期）。
//
// 与显式配置的关系：人的配置永远优先。本功能在 prioritizeLocked **之后**生效，
// 且候选里存在 account_share 账号时整体让位（占比是显式分配偏好）。同理它不能
// 绕过健康/冷却/在途/积分保底/停牌——那些都在候选收集阶段就逐出了。
package pool

import (
	"log"
	"time"
)

// SetExpiringFocus 注入快过期积分聚焦开关（pool.expiring_focus，默认关）。
// 关闭 → 立即清空锁定，回到纯加权路由；开启 → 下一次 pick 选目标。
// 只在开关**发生变化**时清空锁定：saveConfig 的热应用每次保存配置都会调本方法，
// 无条件清空会让「随手保存一个无关配置」也打乱当前目标。
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

// focusExpiringLocked 把候选收窄到聚焦目标（返回单元素切片），无法聚焦时原样返回。
//
// 锁定语义（关键，别改成每次现算最大值）：
//
//	creditsExpiring 会被每笔 NoteModelCost 扣减。若每次 pick 都重新取最大值，两个号
//	接近时目标会逐笔来回翻转（A 烧一笔 → B 反超 → B 烧一笔 → A 反超），结果是两边都
//	烧不完，恰好是本功能要消除的形态。所以选定即锁定，只在账号**真的不可用**时交棒：
//	  1. 账号已从池里移除 / 换了 realm；
//	  2. 账号级不可选（冷却、熔断、连败降权、暂停、保留积分、积分保底）；
//	  3. 锁定号的 creditsExpiring 归零（窗口内那批积分已烧光，目标达成）；
//	  4. 开关关闭（SetExpiringFocus 清表）。
//
// **临时让位不交棒**（在途占满、被本次请求的 tried 排除、成本分层换了层）：这些情况
// 下一个请求就会回来，此时清掉锁定会让高并发下的聚焦退化成普通加权——目标号每被占满
// 一次就重新选一次，而「重新选」在金额接近时又会来回换人。
//
// 锁定按 realm 分桶：cn 请求只能聚焦 cn 号、global 同理（两域积分不同源，锁一个
// 跨域 uid 会让另一域每次请求都判定「不在候选 → 交棒」并覆盖掉本域的锁定）。
// 锁定是运行态、不落 state.json：重启后按当下快照重选一次，代价最多是并列候选换一个；
// 落盘反而会让「重启后仍锁在一个已不该锁的号上」难以解释。调用方须持 p.mu。
func (p *Pool) focusExpiringLocked(layer, all []*entry, realm, reqModel string, now time.Time) []*entry {
	if !p.expiringFocus || len(layer) == 0 {
		return layer
	}
	// 显式选号策略优先：round_robin / sequential 的语义是「按环分摊 / 榨干一个再换」，
	// 聚焦的语义是「全部压到某一个号」——两者互斥。人的显式配置压过自动启发式
	// （与 account_priority / account_share 同一条原则），故策略非 weighted 时整体让位。
	// 不让位的话，聚焦会把 candsAll 收窄成单号，策略环上只剩一个号，静默失效。
	if p.strategy != "" && p.strategy != StrategyWeighted {
		return layer
	}
	// 显式占比优先：候选里只要有一个配了 account_share 的号就整体让位，而不是聚焦完
	// 再让占比扑空——收窄后 share 池只剩目标号，「70/30 分流量」会静默变成独占。
	// 这层判定放在锁定之前：否则会白白锁一个本次根本不会生效的号。
	//
	// 判据用**成本分层前的全集**（all）而不是本层 layer：share 号落在别的成本层时，
	// 只看 layer 会判定「没有占比配置」，于是聚焦生效、candsAll 只剩目标号，
	// sharePoolLocked 随后无从生效。
	if p.shareCandidateLocked(all) {
		return layer
	}
	if uid := p.expiringFocusUID[realm]; uid != "" {
		// 存活判定读 p.byUID 而不是只看候选集：候选集已经过 tried / 在途 / 优先级层 /
		// 成本层的多重过滤，无法区分「账号坏了」和「这次只是轮不到它」。两者处置相反：
		//   账号级不可用 → 交棒（继续锁着只会让后续每个请求都白算一次）；
		//   临时让位     → 保持锁定、本次不收窄（下一个请求就会回来）。
		// 混为一谈的后果是高并发下目标被反复清空重选，聚焦退化成普通加权。
		e, ok := p.byUID[uid]
		switch {
		case !ok:
			delete(p.expiringFocusUID, realm) // 账号已移除
		case realm != "" && e.a.Realm() != realm:
			delete(p.expiringFocusUID, realm) // realm 变更（逃生门切换等）
		case e.creditsExpiring <= 0:
			delete(p.expiringFocusUID, realm) // 窗口内额度已烧光，目标达成
		case p.focusAccountUnusableLocked(e, now, reqModel):
			delete(p.expiringFocusUID, realm) // 账号级不可选（冷却/熔断/降权/暂停/保底）
		default:
			// 锁定号仍然可用：在本层就收窄到它，否则原样返回（层优先）但**不换人**。
			for _, c := range layer {
				if c.a.UID == uid {
					return []*entry{c}
				}
			}
			return layer
		}
	}
	best := p.focusCandidateLocked(layer, now)
	if best == nil {
		// 没有任何号持有窗口内的快过期积分 → 不聚焦，行为与未开一致。
		// 这一支是「功能在跑但整池停摆」，与「一直没积分」外观相同，故留一条日志。
		p.noteFocusDryLocked(realm, len(layer))
		return layer
	}
	delete(p.expiringFocusDry, realm) // 恢复：下次再停摆时重新告警一次
	// 惰性初始化：New() 已建好这张表，但包内直接构造 Pool（测试/嵌入方）时可能是 nil，
	// 而 nil map 赋值会 panic。这里补一次，代价可忽略。
	if p.expiringFocusUID == nil {
		p.expiringFocusUID = map[string]string{}
	}
	p.expiringFocusUID[realm] = best.a.UID
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

// focusAccountUnusableLocked 报告账号是否**账号级**不可选（而非本次临时轮不到它）。
//
// 判据取自 selectable，但**故意排除在途占满**与请求级 tried：这两者是「这一刻轮不到
// 它」，下一个请求就会回来，用来交棒会造成高并发下反复清空锁定。
// 保留的判据都是「要等外部事件才能恢复」的：冷却/熔断/连败降权/暂停/保留积分/
// 积分保底——继续锁定只会让后续每个请求都白算一次。
// 调用方须持 p.mu。
func (p *Pool) focusAccountUnusableLocked(e *entry, now time.Time, model string) bool {
	if p.reserveBlocked(e, p.modelFreeOf(e, model)) {
		return true // 余额见底停牌：等充值/签到，不是这一轮的事
	}
	if p.floorBlockedForRealmModel(e, model, e.a.Realm(), now) {
		return true // 积分保底拦截：触底号不接该收费模型
	}
	if model != "" {
		return !e.healthyForModel(now, model) // 含 6004 模型级豁免口径
	}
	return !e.healthy(now)
}

// focusCandidateLocked 选出聚焦目标：窗口内快过期积分最多者。
// 并列时的次序：最早到期 → UID 字典序（纯确定性，不引入随机——目标是「这段时间
// 就烧这一个号」，随机挑目标只会让相邻两次重启/交棒换号）。
//
// 快路径只读 creditsExpiring，热路径零成本。**全零时走一次慢路径**：用批次明细按
// 当前窗口重算并回写（refreshExpiringFromBucketsLocked）。
//
// 慢路径要解决的不是精度，是「空转」：creditsExpiring 是刷新那一刻算的快照，批次
// 滑进窗口不会触发重算，于是整池字段全是 0 时本函数返回 nil，聚焦静默退回普通加权、
// 一点痕迹都没有。只在全零时才补算——只要有一个号有值，需求落在它身上就已达成「烧
// 快过期的积分」这个目的，别号的数字准不准不影响总浪费（金额/到期先后的排序在
// 浪费总量上是等价的，见 focusBetter）。
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
// 去抖到「状态变化」：某 realm 从有候选变成零候选时打一条，恢复后静默清标记，
// 下次再停摆时重新打。为什么不每次都打：整批积分烧完、或本来就没积分要过期时
// 全池为 0 是正常形态，逐请求打会把日志刷满。
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

// focusBetter 报告 a 是否比 b 更适合当聚焦目标。判据见 focusCandidateLocked。
func focusBetter(a, b *entry) bool {
	if a.creditsExpiring != b.creditsExpiring {
		return a.creditsExpiring > b.creditsExpiring // 金额是主轴：这正是本功能与 expiringUrgency 的差别
	}
	ae, be := a.creditsEarliestExpiry, b.creditsEarliestExpiry
	switch {
	case ae.IsZero() != be.IsZero():
		return !ae.IsZero() // 有批次信息的优先
	case !ae.Equal(be):
		return ae.Before(be) // 同金额：更早到期的先烧
	}
	return a.a.UID < b.a.UID
}

// isFocusLocked 报告该账号当前是否是某个 realm 的聚焦目标（Status 透出用，
// 让「配置有没有真的生效」能自证——与 Priority/Share 同一哲学）。
// 调用方须持 p.mu。
func (p *Pool) isFocusLocked(uid string) bool {
	for _, v := range p.expiringFocusUID {
		if v == uid {
			return true
		}
	}
	return false
}
