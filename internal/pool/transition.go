// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由五个正交维度决定：禁用(disabled)、手动停用(manualDisabled)、
// 账号级冷却(until/coolKind)、模型级冷却(modelCooldowns)、熔断(breakerUntil)。
// 维度之间以「迁移原语」收拢，禁止在其他文件散写这些字段——所有入口（applyErrorPolicy /
// refresh / keepalive / 签到 / 选号 / 运维端点）对状态的改动都必须经本文件的原语或经
// Cooldown/NoteError/NoteSuccess 等封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	manualDisabled     ← setManualDisabledLocked（运维端点 / CLI；只置位不清其他维度）
//	until/coolKind     ← Cooldown(CoolSoft/Hard，固定时长) / CooldownSoftRate / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked 清
//	breakerUntil       ← recordBreakerFailureLocked（NoteError 喂入）；NoteSuccess 清
//	softStreak         ← CooldownSoftRate / CooldownSoftForModel 无解析分支；NoteSuccess/reviveCoolingLocked 清
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked 与
//     reviveCoolingLocked（签到解冻）共用，二者对冷却域的处置因此永远一致。
//   - manualDisabled 与 disabled 各自独立：前者是运维意图（只能由运维入口清除），
//     后者是系统判定（可被签到解冻/refresh 等路径自动撤销）。二者都不清对方，
//     并存时 /status 分别透出（见 entry.go Status.manual_disabled/disabled 注释）。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 疑点 4 修正：旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，
// 会出现「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个
// 先被硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked 只清冷却域（until/coolKind/reason/softStreak/modelCooldowns）
// 并更新 credits，不动熔断器（fails/retryCount/breakerUntil）。签到解冻走这里：
// 签到成功只证明余额恢复与 billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx
// 信号）不应被签到覆盖。
// softStreak 属冷却域（与 until/coolKind 同域），随冷却一并清零——与「解冻只清冷却
// 不清熔断」的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是
// 历史软冷却累积。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	e.clearCoolingLocked()
}

// setManualDisabledLocked 手动停用迁移（运维入口）：只置 manualDisabled + 原因，
// **不清冷却域、不动熔断器**。
//
// 与 disableLocked（自动禁用）的关键差异——手动停用是「对话流量摘除」，不是
// 「账号冻结」：签到、token 保活、排程任务照常执行，账号凭证与积分状态都是活的。
// 因此刻意不碰 until/coolKind/modelCooldowns/breakerUntil：停用期间这些维度继续
// 按各自规律演进（冷却自然到期、熔断计数继续累计），恢复时拿到的是「停用期间
// 真实发生过什么」的完整状态，而不是被清空的一刀切。
//
// 为什么用独立状态位而非复用 disabled：自动禁用会被签到解冻、refresh 成功等路径
// 自动撤销，手动停用若复用同一字段，运维意图会被这些路径意外解除。两位独立、
// 各自清除，都清空才回到选号池。
// 调用方必须已持有 p.mu。
func (p *Pool) setManualDisabledLocked(e *entry, disabled bool, reason string) {
	e.manualDisabled = disabled
	if disabled {
		e.manualReason = reason
	} else {
		e.manualReason = ""
	}
	p.dirty.Store(true)
}
