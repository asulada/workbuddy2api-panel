package main

import (
	"github.com/misakano7545/workbuddy2api-panel/internal/alert"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.WeightedAvailableUIDsForModelRealm 退化为现状
// （AvailableUIDsForModel），老调用（无前缀模型名）语义零改动。
//
// 返回列表可能对快过期账号重复同一 UID，作为虚拟实例权重；会话哈希分配无需感知
// 权重细节，已有绑定的快路径仍直接返回原账号，不做迁移。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}

// realmAwareNewSessionForModel 构造「新会话分配池」的 realm 感知闭包。
//
// 与 realmAwareAvailableForModel 的差别只在快过期聚焦（pool.expiring_focus）生效时：
// 该闭包只返回聚焦目标号，让**新会话**集中到目标账号；而 AvailableForModel 保持全量，
// 已绑定的老会话因此不会在切换瞬间被判失效并迁移——继续用原号直到 TTL 过期，
// 之后再按当时的目标重新分配。
func realmAwareNewSessionForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.NewSessionUIDsForModelRealm(bare, realm)
	}
}

type alertSource struct {
	pool *pool.Pool
	h    *server.Handler
}

func (s alertSource) Health(realm string) alert.Health {
	h := s.pool.RealmHealth(realm)
	return alert.Health{
		Total: h.Total, Healthy: h.Healthy, Cooling: h.Cooling, Disabled: h.Disabled,
		Breaker: h.Breaker, Degraded: h.Degraded, InFlight: h.InFlight,
	}
}

func (s alertSource) WAFActive() bool { return s.h.WAFActive() }
