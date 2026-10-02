package session

import (
	"net/netip"
	"testing"
	"time"
)

func mustAddrs(t *testing.T, ips ...string) []netip.Addr {
	t.Helper()
	addrs := make([]netip.Addr, len(ips))
	for i, ip := range ips {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			t.Fatalf("bad addr %q: %v", ip, err)
		}
		addrs[i] = a
	}
	return addrs
}

func newTestManager(t *testing.T, ips ...string) (*Manager, *ManualClock) {
	t.Helper()
	clock := NewManualClock(time.Unix(1_700_000_000, 0))
	return NewManager(mustAddrs(t, ips...), clock), clock
}

// 会话达到过期时间但尚未回收，Heartbeat 完成续期后执行 Reap，
// 会话与其地址、配额占用仍然存在。
func TestLateHeartbeatRenewsBeforeReap(t *testing.T) {
	m, clock := newTestManager(t, "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4")

	id, addr, err := m.Connect("alice")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// 推进到过期阈值之后，但尚未 Reap：会话仍然有效。
	clock.Advance(ExpireAfter + time.Minute)
	if !m.Heartbeat(id) {
		t.Fatal("late heartbeat should renew a session that has not been reaped")
	}

	// 续期后再推进到接近阈值，Reap 不应回收该会话。
	clock.Advance(ExpireAfter - time.Second)
	m.Reap()

	if !m.Heartbeat(id) {
		t.Fatal("session should still be alive after Reap")
	}

	// 地址仍被 alice 独占：bob 必须拿到不同的地址。
	_, bobAddr, err := m.Connect("bob")
	if err != nil {
		t.Fatalf("Connect bob: %v", err)
	}
	if bobAddr == addr {
		t.Fatalf("addr %v should still be exclusively held by alice", addr)
	}

	// 配额仍被占用：alice 再开 2 个会话后应触发配额上限。
	if _, _, err := m.Connect("alice"); err != nil {
		t.Fatalf("Connect alice #2: %v", err)
	}
	if _, _, err := m.Connect("alice"); err != nil {
		t.Fatalf("Connect alice #3: %v", err)
	}
	if _, _, err := m.Connect("alice"); err != ErrQuotaExceeded {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}

	_ = addr
}

// Reap 回收过期会话后地址被新 Connect 复用；旧心跳不得影响新会话及配额。
func TestReapReleasesAddressAndOldHeartbeatIsInert(t *testing.T) {
	m, clock := newTestManager(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")

	oldID, oldAddr, err := m.Connect("alice")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// 过期并被 Reap 回收。
	clock.Advance(ExpireAfter)
	m.Reap()
	if m.Heartbeat(oldID) {
		t.Fatal("reaped session must not accept heartbeats")
	}

	// 地址被复用：新 Connect 应拿到同一个地址。
	newID, newAddr, err := m.Connect("bob")
	if err != nil {
		t.Fatalf("Connect after reap: %v", err)
	}
	if newAddr != oldAddr {
		t.Fatalf("expected address reuse %v, got %v", oldAddr, newAddr)
	}
	if newID == oldID {
		t.Fatal("sessionID must not be reused")
	}

	// 投递旧心跳：不得改变新会话、其地址占用或 bob 的配额。
	if m.Heartbeat(oldID) {
		t.Fatal("stale heartbeat must fail")
	}
	// 旧心跳之后立即 Reap：新会话刚创建（或刚被心跳），不应被回收。
	m.Reap()
	if !m.Heartbeat(newID) {
		t.Fatal("new session must be unaffected by stale heartbeat")
	}

	// bob 的配额未被旧心跳改变：仍可再开 2 个会话，第 4 个失败。
	if _, _, err := m.Connect("bob"); err != nil {
		t.Fatalf("Connect bob #2: %v", err)
	}
	if _, _, err := m.Connect("bob"); err != nil {
		t.Fatalf("Connect bob #3: %v", err)
	}
	if _, _, err := m.Connect("bob"); err != ErrQuotaExceeded {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
}

// 地址池耗尽时 Connect 失败且不留下占用；释放后可再次分配。
func TestAddressExhaustion(t *testing.T) {
	m, _ := newTestManager(t, "10.0.0.1")

	if _, _, err := m.Connect("alice"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, _, err := m.Connect("bob"); err != ErrNoAddress {
		t.Fatalf("expected ErrNoAddress, got %v", err)
	}
	// 失败的 Connect 不占用 bob 的配额。
	m.DropAll()
	if _, _, err := m.Connect("bob"); err != nil {
		t.Fatalf("Connect after DropAll: %v", err)
	}
}

// DropAll 使全部会话失效，整个地址池恢复可用。
func TestDropAll(t *testing.T) {
	m, _ := newTestManager(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")

	ids := make([]uint64, 0, 3)
	for i := 0; i < 3; i++ {
		id, _, err := m.Connect("alice")
		if err != nil {
			t.Fatalf("Connect #%d: %v", i, err)
		}
		ids = append(ids, id)
	}

	m.DropAll()

	for _, id := range ids {
		if m.Heartbeat(id) {
			t.Fatalf("session %d must be invalid after DropAll", id)
		}
	}
	// 地址池完全恢复，且 alice 的配额已清零。
	for i := 0; i < 3; i++ {
		if _, _, err := m.Connect("alice"); err != nil {
			t.Fatalf("Connect after DropAll #%d: %v", i, err)
		}
	}
	if _, _, err := m.Connect("bob"); err != ErrNoAddress {
		t.Fatalf("expected ErrNoAddress, got %v", err)
	}
}

// 每用户最多 3 个会话；超限失败不留下占用。
func TestPerUserQuota(t *testing.T) {
	m, _ := newTestManager(t, "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4")

	for i := 0; i < MaxSessionsPerUser; i++ {
		if _, _, err := m.Connect("alice"); err != nil {
			t.Fatalf("Connect #%d: %v", i, err)
		}
	}
	if _, _, err := m.Connect("alice"); err != ErrQuotaExceeded {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
	// 超限失败不影响其他用户。
	if _, _, err := m.Connect("bob"); err != nil {
		t.Fatalf("Connect bob: %v", err)
	}
}

// 同一 IP 不会同时属于两个有效会话。
func TestExclusiveAddress(t *testing.T) {
	ips := []string{"10.0.0.1", "10.0.0.2"}
	m, _ := newTestManager(t, ips...)

	seen := map[netip.Addr]uint64{}
	for i := 0; i < len(ips); i++ {
		id, addr, err := m.Connect("alice")
		if err != nil {
			t.Fatalf("Connect #%d: %v", i, err)
		}
		if prev, dup := seen[addr]; dup {
			t.Fatalf("addr %v assigned to both session %d and %d", addr, prev, id)
		}
		seen[addr] = id
	}
}
