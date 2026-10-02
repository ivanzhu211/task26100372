# sessionmgr

VPN 网关的进程内会话管理库：管理会话生命周期、每用户连接配额（最多 3 个）
与虚拟 IPv4 地址分配。前提是用户已获准接入，本库不做认证。

## 语义

- `Connect` 创建会话并分配独占 IP；配额满返回 `ErrQuotaExceeded`，
  地址耗尽返回 `ErrNoAddress`，失败不留任何占用。新会话视为刚收到心跳。
  sessionID 单调递增，进程生命周期内不复用。
- `Heartbeat` 仅对有效会话续期。超过 180 秒不会自动失效：只要尚未被
  `Reap` 回收，迟到心跳仍可续期；已回收/已 DropAll 的会话永远不能复活。
- `Reap` 回收最后活跃时间距检查时刻达到 180 秒的会话，释放地址与配额，
  释放的地址立即可重新分配。
- `DropAll` 使全部会话失效，整个地址池恢复可用。
- 地址被重新分配后，旧 sessionID 的心跳不影响新会话、地址占用或配额。
- 所有方法并发安全。

## 使用

```go
addrs := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")}
m := sessionmgr.NewManager(addrs, nil) // clock 为 nil 使用真实时钟

id, ip, err := m.Connect("user-123")
if err != nil { /* ErrQuotaExceeded 或 ErrNoAddress */ }

ok := m.Heartbeat(id) // false 表示会话已失效

m.Reap()    // 周期性调用，回收闲置满 180 秒的会话
m.DropAll() // 关停时失效所有会话
```

测试通过注入假时钟（实现 `Clock` 接口）控制时间，见 `manager_test.go`。

## 测试

```sh
go test ./...
```
