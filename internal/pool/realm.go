// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"math"
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重（**未注入窗口时的
// 兼容口径**，见 expiringUrgency）。3:1 是温和偏好，不是固定比例：账号组成变化会
// 自然改变最终占比。
const expiringVirtualSlots = 3

// expiringUrgencyMin / expiringUrgencyMax 平滑紧迫度倍率的两端（灵感：上游 issue #140
// 第 1、2 点）：窗口边缘取 Min，越逼近到期越攀升到 Max。
//
// Max 必须 **≥ 基础权重的天花板**（1 + credits 项 10 + 闲置项上限 5 = 16），否则 issue
// 第 2 点无解：余量 50/500 分的号基础权重 2，压不过 500 分号的 11~16，倍率再陡也翻不了盘
// （实测 linear 5 / sqrt 5 都不够，见 expiring_urgency_test.go 的 flip 用例）。
//
// 已知边界：极端情形下仍可能压不过——高余额号**同时**闲置满载（权重 16）而低余额号刚用过
// （权重 2，无闲置补偿）。要连这种也压过，把 Max 提到 35；那会让批次临终几小时几乎独占流量
// （仍是加权随机，不是硬过滤），故不再默认加大。
// ponytail: 先作常量；要按部署调档再升级为 pool.* 配置项。
const (
	expiringUrgencyMin = 1.5
	expiringUrgencyMax = 20.0

	// expiringCreditScale 窗口内账号的 credits 项系数（原 10 → 2）。
	//
	// issue #140 第 2 点的根因：余额基数**恒**主导临期决策——余量 500 分号的 credits 项是 10、
	// 50 分号只有 1，而紧迫度倍率对「1 天 vs 6 天」这种差值能达到的比上限只有 ~8.4 倍（无论
	// Min/Max 怎么调、形状多陡），压不过 10:1 的基数差。所以必须把两轴对调：紧迫度为主轴
	// （1.5→20），余额退为次轴（1→3，只用来在同紧迫度内挑余量大的）。
	expiringCreditScale = 0.2
)

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if u := p.expiringUrgency(p.byUID[uid], now); u > 1 {
				slots = int(math.Round(u))
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// NewSessionUIDsForModelRealm 返回**新会话可分配**的账号列表（session 慢路径用）。
//
// 与 WeightedAvailableUIDsForModelRealm 的唯一差别：快过期聚焦（pool.expiring_focus）
// 生效时只返回目标号。这样新会话必然落到目标号上，而**已绑定的老会话不受影响**——
// 它们的可用性校验走 AvailableUIDsForModelRealm（不收窄），继续用原号直到 TTL 过期，
// 之后按当时的聚焦目标重新分配。渐进迁移，避免切换瞬间全部会话前缀缓存 miss。
//
// 未开聚焦 / 无目标 / 显式配置（account_share、非 weighted 策略）让位时，与
// WeightedAvailableUIDsForModelRealm 完全一致（聚焦的让位判定复用同一函数）。
func (p *Pool) NewSessionUIDsForModelRealm(model, realm string) []string {
	out := p.WeightedAvailableUIDsForModelRealm(model, realm)
	if len(out) <= 1 {
		return out
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.expiringFocus {
		return out
	}
	uid := p.sessionFocusTargetLocked(model, realm)
	if uid == "" {
		return out
	}
	// 目标号在 out 里至少出现一次（重量列表按 slots 重复）：保留全部副本，
	// 保持「虚拟实例权重」语义不变；目标号不可用时保持原列表，避免把新会话饿死。
	kept := make([]string, 0, len(out))
	for _, u := range out {
		if u == uid {
			kept = append(kept, u)
		}
	}
	if len(kept) == 0 {
		return out
	}
	return kept
}

// sessionFocusTargetLocked 返回快过期聚焦对**新会话**的分配目标；无目标返回空串。
//
// 与 pick 的 focusExpiringLocked 共用同一份锁定表（p.expiringFocusUID），但**不做
// 交棒判定**：session 慢路径只在「没有可用绑定」时调用，此时若顺手改写锁定表，
// 一个刚启动、余额还没刷新的进程会把锁定表写满错误目标。真正的交棒由 pick 负责
// （它每笔请求都会跑，且能看到成本层/在途等完整上下文）。
// 这里只回答「当前锁的是谁，它在这个模型上可用吗」，不可用就返回空串让新会话照常散列。
// 调用方须持 p.mu。
func (p *Pool) sessionFocusTargetLocked(model, realm string) string {
	uid := p.expiringFocusUID[realm]
	if uid == "" {
		return ""
	}
	// 显式配置让位：与 focusExpiringLocked 同判据（占比按比例分，与独占互斥；
	// 轮询/顺序耗尽的环也是显式配置）。这里只读锁定表，故用全集判断占比。
	if p.strategy != "" && p.strategy != StrategyWeighted {
		return ""
	}
	for _, e := range p.byUID {
		if p.shareOf(e.a.UID) > 0 {
			return ""
		}
	}
	e, ok := p.byUID[uid]
	if !ok {
		return ""
	}
	now := time.Now()
	if e.creditsExpiring <= 0 {
		return ""
	}
	if p.focusAccountUnusableLocked(e, now, model) {
		return ""
	}
	return uid
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
//
// **不套快过期聚焦**：这个列表同时用于「已绑定会话的可用性校验」（session 快路径
// availableSet）。一旦在这里收窄成单号，所有老会话会立刻判定「绑定号不可用」而被
// 迁移到目标号——那正是「新会话才迁移、老会话等 TTL 自然过期」要避免的瞬间全量
// 缓存失效。聚焦只作用于新会话的分配池，见 NewSessionUIDsForModelRealm。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
