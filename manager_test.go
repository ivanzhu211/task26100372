package sessionmgr_test

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"sessionmgr"
)

// fakeClock 是可手动推进的单调时钟。
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

func pool(t *testing.T, ips ...string) []netip.Addr {
	t.Helper()
	addrs := make([]netip.Addr, len(ips))
	for i, s := range ips {
		addrs[i] = mustAddr(t, s)
	}
	return addrs
}

func mustConnect(t *testing.T, m *sessionmgr.Manager, user string) (uint64, netip.Addr) {
	t.Helper()
	id, addr, err := m.Connect(user)
	if err != nil {
		t.Fatalf("Connect(%q): %v", user, err)
	}
	return id, addr
}

func TestConnectAssignsExclusiveIPs(t *testing.T) {
	m := sessionmgr.NewManager(pool(t, "10.0.0.1", "10.0.0.2"), &fakeClock{now: time.Now()})

	_, a1 := mustConnect(t, m, "alice")
	_, a2 := mustConnect(t, m, "bob")
	if a1 == a2 {
		t.Fatalf("two sessions share IP %v", a1)
	}
	if a1 != mustAddr(t, "10.0.0.1") && a1 != mustAddr(t, "10.0.0.2") {
		t.Fatalf("assigned IP %v not from pool", a1)
	}
}

func TestPerUserQuota(t *testing.T) {
	m := sessionmgr.NewManager(pool(t, "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"), &fakeClock{now: time.Now()})

	for i := 0; i < 3; i++ {
		mustConnect(t, m, "alice")
	}
	if _, _, err := m.Connect("alice"); !errors.Is(err, sessionmgr.ErrQuotaExceeded) {
		t.Fatalf("4th connect: got %v, want ErrQuotaExceeded", err)
	}
	// 配额失败不留下占用：其他用户仍可正常连接。
	mustConnect(t, m, "bob")
}

func TestAddressExhaustedLeavesNoOccupancy(t *testing.T) {
	m := sessionmgr.NewManager(pool(t, "10.0.0.1", "10.0.0.2", "10.0.0.3"), &fakeClock{now: time.Now()})

	for i := 0; i < 3; i++ {
		mustConnect(t, m, "alice")
	}
	if _, _, err := m.Connect("bob"); !errors.Is(err, sessionmgr.ErrNoAddress) {
		t.Fatalf("connect with empty pool: got %v, want ErrNoAddress", err)
	}
	// 失败未消耗 bob 的配额：释放地址后 bob 可连续占满 3 个会话。
	m.DropAll()
	for i := 0; i < 3; i++ {
		mustConnect(t, m, "bob")
	}
	if _, _, err := m.Connect("bob"); !errors.Is(err, sessionmgr.ErrQuotaExceeded) {
		t.Fatalf("bob quota: got %v, want ErrQuotaExceeded", err)
	}
}

func TestSessionIDsNeverReused(t *testing.T) {
	m := sessionmgr.NewManager(pool(t, "10.0.0.1"), &fakeClock{now: time.Now()})

	id1, _ := mustConnect(t, m, "alice")
	m.DropAll()
	id2, _ := mustConnect(t, m, "alice")
	if id1 == id2 {
		t.Fatalf("sessionID %d reused after DropAll", id1)
	}
}

func TestHeartbeatUnknownSession(t *testing.T) {
	m := sessionmgr.NewManager(pool(t, "10.0.0.1"), &fakeClock{now: time.Now()})
	if m.Heartbeat(42) {
		t.Fatal("heartbeat on unknown session succeeded")
	}
}

// 场景一：会话达到过期时间但尚未回收，Heartbeat 续期后 Reap 不得回收它。
func TestLateHeartbeatBeforeReapKeepsSession(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m := sessionmgr.NewManager(pool(t,
		"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"), clock)

	id, addr := mustConnect(t, m, "alice")

	clock.Advance(200 * time.Second) // 已超过 180 秒，但尚未 Reap
	if !m.Heartbeat(id) {
		t.Fatal("late heartbeat on un-reaped session failed")
	}

	clock.Advance(100 * time.Second) // 距续期仅 100 秒
	m.Reap()

	// 会话仍然有效。
	if !m.Heartbeat(id) {
		t.Fatal("session was reaped despite recent heartbeat")
	}
	// 用户配额仍被占用：alice 已有 1 个会话，还能再建 2 个，第 4 个超限。
	_, a2 := mustConnect(t, m, "alice")
	_, a3 := mustConnect(t, m, "alice")
	if _, _, err := m.Connect("alice"); !errors.Is(err, sessionmgr.ErrQuotaExceeded) {
		t.Fatalf("quota should still count renewed session, got %v", err)
	}
	// 地址占用仍然存在：续期会话的地址未被回收重发。
	// alice 占 3 个，bob 再占 2 个后池恰好耗尽，且 5 个地址互不相同。
	_, a4 := mustConnect(t, m, "bob")
	_, a5 := mustConnect(t, m, "bob")
	if _, _, err := m.Connect("carol"); !errors.Is(err, sessionmgr.ErrNoAddress) {
		t.Fatalf("pool should be exhausted (renewed session still holds %v), got %v", addr, err)
	}
	seen := map[netip.Addr]bool{addr: true, a2: true, a3: true, a4: true, a5: true}
	if len(seen) != 5 {
		t.Fatalf("address of renewed session was handed out twice: %v", seen)
	}
}

// 场景二：Reap 回收过期会话，地址被新 Connect 复用；
// 旧心跳不得影响新会话、地址占用或用户配额。
func TestReapReclaimsAndStaleHeartbeatIsHarmless(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m := sessionmgr.NewManager(pool(t, "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"), clock)

	oldID, oldAddr := mustConnect(t, m, "alice")

	clock.Advance(180 * time.Second)
	m.Reap()

	// 旧会话已失效，心跳不可复活。
	if m.Heartbeat(oldID) {
		t.Fatal("heartbeat revived a reaped session")
	}
	// 回收后地址立即可重新分配（LIFO，弹出的正是刚回收的地址）。
	newID, newAddr := mustConnect(t, m, "bob")
	if newAddr != oldAddr {
		t.Fatalf("address not reused: got %v, want %v", newAddr, oldAddr)
	}
	if newID == oldID {
		t.Fatal("sessionID reused")
	}

	// 投递旧心跳：必须失败，且不影响新会话。
	if m.Heartbeat(oldID) {
		t.Fatal("stale heartbeat succeeded")
	}
	clock.Advance(100 * time.Second)
	if !m.Heartbeat(newID) {
		t.Fatal("new session broken by stale heartbeat")
	}
	clock.Advance(100 * time.Second)
	m.Reap() // newID 距今 100 秒，不应被回收
	if !m.Heartbeat(newID) {
		t.Fatal("new session wrongly reaped after stale heartbeat")
	}
	// 旧心跳未改变地址占用：newAddr 仍由 newID 独占持有，
	// 旧会话不占用任何地址——alice 的配额已随回收释放，
	// 可重新占满 3 个会话（池中还有 3 个空闲地址）。
	for i := 0; i < 3; i++ {
		mustConnect(t, m, "alice")
	}
	if _, _, err := m.Connect("alice"); !errors.Is(err, sessionmgr.ErrQuotaExceeded) {
		t.Fatalf("alice quota after reclaim: got %v, want ErrQuotaExceeded", err)
	}
}

func TestReapBoundary(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m := sessionmgr.NewManager(pool(t, "10.0.0.1", "10.0.0.2"), clock)

	stay, _ := mustConnect(t, m, "alice")
	gone, _ := mustConnect(t, m, "bob")

	clock.Advance(179 * time.Second)
	if !m.Heartbeat(stay) {
		t.Fatal("heartbeat failed")
	}
	clock.Advance(1 * time.Second) // gone 距今 180 秒，stay 距今 1 秒
	m.Reap()

	if m.Heartbeat(gone) {
		t.Fatal("session at exactly 180s should be reaped")
	}
	if !m.Heartbeat(stay) {
		t.Fatal("recently renewed session should survive Reap")
	}
}

func TestDropAllReleasesEverything(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	addrs := pool(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
	m := sessionmgr.NewManager(addrs, clock)

	id1, _ := mustConnect(t, m, "alice")
	id2, _ := mustConnect(t, m, "alice")
	id3, _ := mustConnect(t, m, "bob")

	m.DropAll()

	for _, id := range []uint64{id1, id2, id3} {
		if m.Heartbeat(id) {
			t.Fatalf("session %d still valid after DropAll", id)
		}
	}
	// 整个地址池可用：3 个地址可全部分配出去。
	got := make(map[netip.Addr]bool)
	for i := 0; i < 3; i++ {
		_, a := mustConnect(t, m, "carol")
		got[a] = true
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 distinct addresses after DropAll, got %d", len(got))
	}
	if _, _, err := m.Connect("dave"); !errors.Is(err, sessionmgr.ErrNoAddress) {
		t.Fatalf("pool should be exhausted again, got %v", err)
	}
}

func TestConnectIsFreshHeartbeat(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	m := sessionmgr.NewManager(pool(t, "10.0.0.1"), clock)

	id, _ := mustConnect(t, m, "alice")
	clock.Advance(179 * time.Second)
	m.Reap()
	if !m.Heartbeat(id) {
		t.Fatal("newly connected session should count as just-heartbeat")
	}
}
