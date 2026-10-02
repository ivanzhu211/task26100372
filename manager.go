package sessionmgr

import (
	"errors"
	"net/netip"
	"sync"
	"time"
)

// 会话过期阈值：最后活跃时间距检查时刻达到该值即被回收。
const sessionTTL = 180 * time.Second

// 每用户最大并发会话数。
const maxSessionsPerUser = 3

var (
	// ErrQuotaExceeded 表示该用户的会话配额已满。
	ErrQuotaExceeded = errors.New("sessionmgr: per-user session quota exceeded")
	// ErrNoAddress 表示地址池已耗尽。
	ErrNoAddress = errors.New("sessionmgr: no available address")
)

type session struct {
	id         uint64
	userID     string
	addr       netip.Addr
	lastActive time.Time
}

// Manager 管理进程内的 VPN 会话、每用户配额与虚拟 IP 分配。
// 所有方法均可并发调用。
type Manager struct {
	mu     sync.Mutex
	clock  Clock
	nextID uint64

	free     []netip.Addr        // 可分配的地址
	sessions map[uint64]*session // 有效会话
	byUser   map[string]int      // 每用户当前会话数
}

// NewManager 构造会话管理器。addrs 为固定且不重复的 IPv4 地址集合；
// clock 为 nil 时使用真实时钟。
func NewManager(addrs []netip.Addr, clock Clock) *Manager {
	if clock == nil {
		clock = realClock{}
	}
	free := make([]netip.Addr, len(addrs))
	copy(free, addrs)
	return &Manager{
		clock:    clock,
		nextID:   1,
		free:     free,
		sessions: make(map[uint64]*session),
		byUser:   make(map[string]int),
	}
}

// Connect 创建会话，返回 sessionID 与独占 IP。
// 每用户最多 3 个会话；配额已满或地址耗尽时失败，不留下任何占用。
// 新会话视为刚收到心跳。
func (m *Manager) Connect(userID string) (uint64, netip.Addr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.byUser[userID] >= maxSessionsPerUser {
		return 0, netip.Addr{}, ErrQuotaExceeded
	}
	if len(m.free) == 0 {
		return 0, netip.Addr{}, ErrNoAddress
	}

	addr := m.free[len(m.free)-1]
	m.free = m.free[:len(m.free)-1]

	id := m.nextID
	m.nextID++
	m.sessions[id] = &session{
		id:         id,
		userID:     userID,
		addr:       addr,
		lastActive: m.clock.Now(),
	}
	m.byUser[userID]++
	return id, addr, nil
}

// Heartbeat 为有效会话续期，返回是否成功。
// 超过 180 秒不使会话失效：只要尚未被 Reap 回收，迟到心跳仍可续期。
// 已失效（被回收或 DropAll）的会话永远不能复活。
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

// Reap 回收最后活跃时间距检查时刻达到 180 秒的会话，
// 释放其地址与用户配额，释放的地址立即可重新分配。
func (m *Manager) Reap() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock.Now()
	for id, s := range m.sessions {
		if now.Sub(s.lastActive) >= sessionTTL {
			m.reclaimLocked(id, s)
		}
	}
}

// DropAll 使全部有效会话失效并释放所有占用，
// 返回后整个地址池可用。
func (m *Manager) DropAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, s := range m.sessions {
		m.reclaimLocked(id, s)
	}
}

func (m *Manager) reclaimLocked(id uint64, s *session) {
	delete(m.sessions, id)
	m.byUser[s.userID]--
	if m.byUser[s.userID] == 0 {
		delete(m.byUser, s.userID)
	}
	m.free = append(m.free, s.addr)
}
