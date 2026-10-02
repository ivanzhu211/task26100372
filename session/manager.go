package session

import (
	"errors"
	"net/netip"
	"sync"
	"time"
)

// 会话过期阈值：最后活跃时间距检查时刻达到该值即被回收。
const ExpireAfter = 180 * time.Second

// 每用户最大并发会话数。
const MaxSessionsPerUser = 3

var (
	// ErrQuotaExceeded 表示该用户的会话配额已满。
	ErrQuotaExceeded = errors.New("session: per-user session quota exceeded")
	// ErrNoAddress 表示地址池已耗尽。
	ErrNoAddress = errors.New("session: no available address")
)

// Manager 管理 VPN 网关的进程内会话、每用户配额与虚拟 IP 分配。
// 所有方法可并发调用。
type Manager struct {
	mu       sync.Mutex
	clock    Clock
	sessions map[uint64]*sessionState
	byUser   map[string]int
	free     []netip.Addr
	nextID   uint64
}

type sessionState struct {
	id         uint64
	userID     string
	addr       netip.Addr
	lastActive time.Time
}

// NewManager 构造会话管理器。addrs 为固定且不重复的 IPv4 地址集合；
// clock 为可注入的单调时钟，传 nil 时使用系统时间。
func NewManager(addrs []netip.Addr, clock Clock) *Manager {
	if clock == nil {
		clock = SystemClock{}
	}
	free := make([]netip.Addr, len(addrs))
	copy(free, addrs)
	return &Manager{
		clock:    clock,
		sessions: make(map[uint64]*sessionState),
		byUser:   make(map[string]int),
		free:     free,
		nextID:   1,
	}
}

// Connect 创建会话，返回 sessionID 与一个独占 IP。
// 每用户最多占用 MaxSessionsPerUser 个会话；配额已满或地址耗尽时失败，
// 不留下任何占用。sessionID 在进程生命周期内不复用；创建视为刚收到心跳。
func (m *Manager) Connect(userID string) (uint64, netip.Addr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.byUser[userID] >= MaxSessionsPerUser {
		return 0, netip.Addr{}, ErrQuotaExceeded
	}
	if len(m.free) == 0 {
		return 0, netip.Addr{}, ErrNoAddress
	}

	addr := m.free[len(m.free)-1]
	m.free = m.free[:len(m.free)-1]

	id := m.nextID
	m.nextID++

	m.sessions[id] = &sessionState{
		id:         id,
		userID:     userID,
		addr:       addr,
		lastActive: m.clock.Now(),
	}
	m.byUser[userID]++
	return id, addr, nil
}

// Heartbeat 对有效会话续期并返回 true；会话不存在或已失效返回 false。
// 超过 ExpireAfter 本身不使会话失效：只要尚未被 Reap 回收，
// 迟到心跳仍可续期；已失效会话永远不能通过心跳复活。
func (m *Manager) Heartbeat(sessionID uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return false
	}
	s.lastActive = m.clock.Now()
	return true
}

// Reap 回收最后活跃时间距检查时刻达到 ExpireAfter 的会话，
// 释放其地址与用户配额。返回后释放的地址可立即重新分配。
func (m *Manager) Reap() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock.Now()
	for id, s := range m.sessions {
		if now.Sub(s.lastActive) >= ExpireAfter {
			m.releaseLocked(id, s)
		}
	}
}

// DropAll 使全部有效会话失效并释放其地址与用户配额。
// 返回后整个地址池可用。
func (m *Manager) DropAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, s := range m.sessions {
		m.releaseLocked(id, s)
	}
}

func (m *Manager) releaseLocked(id uint64, s *sessionState) {
	delete(m.sessions, id)
	m.byUser[s.userID]--
	if m.byUser[s.userID] == 0 {
		delete(m.byUser, s.userID)
	}
	m.free = append(m.free, s.addr)
}
