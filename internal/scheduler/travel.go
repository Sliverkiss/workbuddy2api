// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// activityReportGap 同一账号内连续上报之间的间隔：5 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条。测试可置 0。
var activityReportGap = 1500 * time.Millisecond

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
func (s *Scheduler) RunTravelNow() {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		s.travelOne(a)
	}
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
func (s *Scheduler) travelOne(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.UID8(a.UID), err)
		return
	}
	if buddy == nil {
		s.travelAdopt(a)
		return
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.UID8(a.UID), err)
		return
	}
	switch ts.State {
	case travelStateArrived:
		s.travelClaim(a, ts)
	case travelStateIdle:
		s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.UID8(a.UID), ts.RecordID)
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.UID8(a.UID), ts.State)
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.UID8(a.UID))
		return
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.UID8(a.UID), err)
		return
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.UID8(a.UID), travelLocationID)
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.UID8(a.UID))
		return
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.UID8(a.UID), ts.RecordID, err)
		return
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.UID8(a.UID), ts.RecordID, reward)
}

// travelAdopt 旅行巡检时领养：受 adoptTriedToday 当日防抖约束。
func (s *Scheduler) travelAdopt(a *auth.Auth) {
	s.adoptBuddy(a, false)
}

// travelAdoptForce 活跃上报补满对话量后领养：豁免 adoptTriedToday 当日防抖。
// 背景：旅行排程 09 点已领养且因对话量未达 skip，10 点活跃上报 5 连发把
// 对话量补满——此时是「门槛刚达成」的新状态，不算对上游重试轰炸，放行重试。
// 有猫账号 BuddyInfo 非空时直接跳过（不重复领养）。
func (s *Scheduler) travelAdoptForce(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("activity %s: buddy-info: %v", logfmt.UID8(a.UID), err)
		return
	}
	if buddy != nil {
		return // 已有猫，无需领养
	}
	s.adoptBuddy(a, true) // force=true 豁免当日防抖
}

// adoptBuddy 无猫时领养：先同意协议（幂等）再 buddy/first。
// conversation 门槛未达标（HTTP 400 first_buddy task not completed yet）属预期行为，
// 记一次当日已试后静默跳过，不再重试。force=true 时豁免当日防抖（活跃上报补满对话量后重试）。
func (s *Scheduler) adoptBuddy(a *auth.Auth, force bool) {
	if !force && s.adoptTriedToday(a.UID) {
		return
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.UID8(a.UID), err)
		return
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.UID8(a.UID))
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.UID8(a.UID))
	default:
		log.Printf("travel %s: adopt: %v", logfmt.UID8(a.UID), err)
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
