# task26100372 — VPN 网关进程内会话管理库

`vpngw/session` 提供进程内的会话管理：会话生命周期、每用户连接配额（最多 3 个）
与虚拟 IPv4 地址的独占分配。所有方法可并发调用。

## 语义

- `Connect(userID)`：创建会话，返回 `sessionID` 与独占 IP。配额已满返回
  `ErrQuotaExceeded`，地址耗尽返回 `ErrNoAddress`，失败不留下任何占用。
  `sessionID` 在进程生命周期内不复用；创建视为刚收到心跳。
- `Heartbeat(sessionID)`：有效会话续期返回 `true`，否则 `false`。
  超过 180 秒不自动失效，只要未被 `Reap` 回收，迟到心跳仍可续期；
  已失效会话永远不能复活。
- `Reap()`：回收最后活跃时间距检查时刻达到 180 秒的会话，释放地址与配额。
- `DropAll()`：使全部会话失效，整个地址池恢复可用。

## 使用示例

```go
addrs := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2")}
m := session.NewManager(addrs, nil) // nil 使用系统时钟；测试可注入 *session.ManualClock

id, ip, err := m.Connect("alice")
if err != nil { /* ErrQuotaExceeded / ErrNoAddress */ }

ok := m.Heartbeat(id)

m.Reap()    // 定期调用，回收到期会话
m.DropAll() // 关停时清空全部会话
```

## 测试

```sh
go test ./...
```
