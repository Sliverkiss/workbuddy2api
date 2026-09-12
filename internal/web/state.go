// state.go Web 管理台自有状态：探测缓存 / 积分快照 / 刷新标记 / 任务事件。
// 持久化到 <DataDir>/web-state.json（tmp+rename 原子写）；请求行与 stdout 行仅内存。
package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"workbuddy2api/internal/upstream"
)

// ---- 对外 JSON 形状（与 Workbuddy-Web 前端契约逐字一致，camelCase）----

type CheckinView struct {
	Checked bool `json:"checked"`
}

type BuddyView struct {
	Has  bool   `json:"has"`
	Name string `json:"name"`
}

// TravelView 旅行状态视图（upstream.TravelState 的 camelCase 投影）。
type TravelView struct {
	State             string `json:"state"`
	LocationID        int64  `json:"locationId"`
	LocationName      string `json:"locationName"`
	RecordID          int64  `json:"recordId"`
	RewardCredit      int64  `json:"rewardCredit"`
	DailyLimitReached bool   `json:"dailyLimitReached"`
	BuddyID           int64  `json:"buddyId"`
	DepartAtMs        int64  `json:"departAtMs"`
	ArriveAtMs        int64  `json:"arriveAtMs"`
	ServerNowMs       int64  `json:"serverNowMs"`
}

func travelView(st *upstream.TravelState) *TravelView {
	if st == nil {
		return nil
	}
	v := &TravelView{
		State:             st.State,
		RecordID:          st.RecordID,
		RewardCredit:      st.RewardCredit,
		DailyLimitReached: st.DailyLimitReached,
		BuddyID:           st.BuddyID,
		DepartAtMs:        st.DepartAtMs,
		ArriveAtMs:        st.ArriveAtMs,
		ServerNowMs:       st.ServerNowMs,
	}
	if st.Location != nil {
		v.LocationID = st.Location.ID
		v.LocationName = st.Location.Name
	}
	return v
}

// CreditView 积分视图（probe/任务刷新时写入）。
type CreditView struct {
	Source   string             `json:"source"` // "new" | "legacy"
	Total    float64            `json:"total"`
	Used     float64            `json:"used"`
	Remain   float64            `json:"remain"`
	Packages []upstream.Package `json:"packages"`
}

// LiveStatus 单账号只读探测快照（AccountCard 的全部实时区块数据源）。
type LiveStatus struct {
	Ts      int64             `json:"ts"`
	Checkin *CheckinView      `json:"checkin"`
	Buddy   *BuddyView        `json:"buddy"`
	Travel  *TravelView       `json:"travel"`
	Credits *CreditView       `json:"credits"`
	Errors  map[string]string `json:"errors"`
}

// Snapshot 积分快照（今日消耗 delta 用）。
type Snapshot struct {
	Ts     int64   `json:"ts"`
	Remain float64 `json:"remain"`
}

// Event 任务/探测事件（日志页任务日志标签数据源）。
type Event struct {
	ID       int64  `json:"id"`
	Ts       int64  `json:"ts"`
	Kind     string `json:"kind"` // checkin/keepalive/travel/activity/credit/probe/oauth/delete
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Status   string `json:"status"` // ok/already/skip/warn/fail
	Msg      string `json:"msg"`
}

// ---- 持久化 ----

type persistState struct {
	LiveStatus      map[string]*LiveStatus `json:"liveStatus"`
	CreditSnapshots map[string][]Snapshot  `json:"creditSnapshots"`
	RefreshMarks    map[string]int64       `json:"refreshMarks"`
	Events          []Event                `json:"events"`
}

const (
	snapshotCap = 120 // 每账号快照上限（与 Node 版一致）
	eventCap    = 300 // 事件留存上限
)

type webState struct {
	mu   sync.Mutex
	path string
	data persistState
}

func loadState(path string) *webState {
	s := &webState{path: path, data: persistState{
		LiveStatus:      map[string]*LiveStatus{},
		CreditSnapshots: map[string][]Snapshot{},
		RefreshMarks:    map[string]int64{},
	}}
	raw, err := os.ReadFile(path)
	if err == nil {
		var p persistState
		if json.Unmarshal(raw, &p) == nil {
			if p.LiveStatus != nil {
				s.data.LiveStatus = p.LiveStatus
			}
			if p.CreditSnapshots != nil {
				s.data.CreditSnapshots = p.CreditSnapshots
			}
			if p.RefreshMarks != nil {
				s.data.RefreshMarks = p.RefreshMarks
			}
			if p.Events != nil {
				s.data.Events = p.Events
			}
		}
	}
	return s
}

// save 原子落盘（调用方须已持锁）。
func (s *webState) saveLocked() {
	raw, err := json.Marshal(&s.data)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// ---- 变更操作（各自持锁 + 落盘）----

func (s *webState) setLiveStatus(uid string, ls *LiveStatus) {
	s.mu.Lock()
	s.data.LiveStatus[uid] = ls
	s.saveLocked()
	s.mu.Unlock()
}

func (s *webState) liveStatus(uid string) *LiveStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.LiveStatus[uid]
}

func (s *webState) allLiveStatus() map[string]*LiveStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*LiveStatus, len(s.data.LiveStatus))
	for k, v := range s.data.LiveStatus {
		out[k] = v
	}
	return out
}

func (s *webState) addSnapshot(uid string, remain float64, ts int64) {
	s.mu.Lock()
	arr := append(s.data.CreditSnapshots[uid], Snapshot{Ts: ts, Remain: remain})
	if len(arr) > snapshotCap {
		arr = arr[len(arr)-snapshotCap:]
	}
	s.data.CreditSnapshots[uid] = arr
	s.saveLocked()
	s.mu.Unlock()
}

func (s *webState) snapshots() map[string][]Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]Snapshot, len(s.data.CreditSnapshots))
	for k, v := range s.data.CreditSnapshots {
		out[k] = v
	}
	return out
}

func (s *webState) markRefreshed(uid string, ts int64) {
	s.mu.Lock()
	s.data.RefreshMarks[uid] = ts
	s.saveLocked()
	s.mu.Unlock()
}

func (s *webState) refreshMark(uid string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.RefreshMarks[uid]
}

func (s *webState) addEvent(ev Event) {
	s.mu.Lock()
	var maxID int64
	for _, e := range s.data.Events {
		if e.ID > maxID {
			maxID = e.ID
		}
	}
	ev.ID = maxID + 1
	s.data.Events = append(s.data.Events, ev)
	if len(s.data.Events) > eventCap {
		s.data.Events = s.data.Events[len(s.data.Events)-eventCap:]
	}
	s.saveLocked()
	s.mu.Unlock()
}

func (s *webState) events(kind string, limit int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.data.Events {
		if kind == "" || e.Kind == kind {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
