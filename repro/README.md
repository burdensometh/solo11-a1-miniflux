# 故障复现与根因说明：服务运行数天后订阅停止更新

## 现象与根因结论

现象：进程存活、监听端口正常、日志没有明显报错，但所有订阅不再刷新；
人工重启后恢复，两三天后再次发生，没有固定规律，最长一次约 6 小时才被发现。

根因是**两个缺陷叠加形成的单点击穿**：

1. **数据库查询没有任何超时（根因，触发点）**
   `internal/database/postgresql.go` 里连接池只配置了连接数和连接寿命，
   既没有 `statement_timeout` / `lock_timeout`，DSN 里也没有 TCP keepalive。
   当一条 SQL 在等待行锁（例如另一个“idle in transaction”的会话持有锁），
   或 TCP 连接因 NAT 老化/静默丢包变成半开连接时，`database/sql` 的调用会
   **永久阻塞**，goroutine 永远不返回。

2. **任务队列为无缓冲 channel + 调度器同步阻塞推送（放大器）**
   旧版 `worker.Pool` 的 `queue` 是 `make(chan model.Job)`（无缓冲），
   `Push` 会一直阻塞到某个 worker 取走任务为止。而 `feedScheduler`
   只有一个 goroutine，在循环里直接调用 `pool.Push(jobs)`。

   于是故障链条是：

   ```
   某条 SQL 永久阻塞
        → 占用一个 worker 且永不释放
        → 同样的阻塞在数小时/数天内陆续耗尽全部 16 个 worker
        → 调度器下一轮 Push 时，无缓冲 channel 无人接收，调度 goroutine 永久阻塞
        → 不再产生任何新批次，所有订阅停止刷新
        → HTTP 服务不依赖该 goroutine，所以端口仍在、进程仍在
        → 阻塞期间没有错误日志（goroutine 只是“等”在那里，没有返回 error）
   ```

   重启能恢复是因为阻塞的连接和 goroutine 被整体清空；两三天后再犯，是因为
   锁等待/半开连接这类外部条件再次出现，与具体时刻无关，所以“找不到规律”。

## 修复内容（不是定时重启）

| 文件 | 修改 |
| --- | --- |
| `internal/database/postgresql.go` | 每个连接建立时执行 `SET statement_timeout` / `SET lock_timeout`；DSN 默认注入 TCP keepalive 参数，防止半开连接永久挂死 |
| `internal/config/options.go` | 新增 `DATABASE_QUERY_TIMEOUT`（默认 30s）和 `DATABASE_LOCK_TIMEOUT`（默认 10s） |
| `internal/worker/pool.go` | 队列改为缓冲 channel；`Push` 改为非阻塞，队列满时丢弃本批次并记录告警，任务顺延到下一调度周期而不是冻死调度器 |
| `internal/worker/worker.go` | 每个任务增加 `recover`，单个 feed 刷新 panic 不会再让 worker 永久退出 |
| `internal/cli/scheduler.go` | `time.Tick` 换成可停止的 `time.NewTicker`（旧写法会泄漏 ticker），并返回 stop 函数 |
| `internal/cli/daemon.go` | 优雅关闭时先停调度器再关 worker 池 |

修复后的行为：被锁住的查询在 10 秒（锁等待）或 30 秒（语句总时长）后报错返回，
worker 被释放并继续处理后续任务；即使所有 worker 都很慢，调度器也只会把多余
任务顺延，而不是整体停摆。错误会以 `slog.Warn/Error` 写进日志，不再“静默”。

## 稳定复现步骤

### 复现 A：数据库锁等待导致永久阻塞（触发点，无需等待几天）

用一条被行锁挡住的 UPDATE，模拟“没有超时的查询永久挂住 worker”：

```bash
./repro/01-reproduce-lock-hang.sh
```

脚本会用 docker 起一个隔离的 PostgreSQL，依次演示：

1. 无竞争的 UPDATE 立即返回；
2. 会话 A 持有行锁不提交，会话 B 的 UPDATE **没有超时时会一直阻塞**
   （脚本用 `timeout 5` 证明 5 秒后仍未返回——这就是线上 worker 的状态）；
3. 加上 `SET lock_timeout='3s'` 后，同样的竞争查询在约 3 秒后失败返回
   （对应本次修复，worker 被释放）；
4. `SET statement_timeout='3s'` 能中断任意长时间语句（例如 `pg_sleep(10)`）。

需要本机有 docker 和 psql；脚本退出时自动清理容器。

### 复现 B：调度器被无缓冲队列冻死（放大器，纯 Go，秒级）

`repro/scheduler_freeze_repro_test.go` 用最小模型对比新旧两种队列：

- `TestOldPoolFreezesTheScheduler`：无缓冲 channel + 阻塞 `Push`，
  worker 不消费时调度 goroutine 在 500ms 内被证明永久卡住；
- `TestNewPoolKeepsTheSchedulerRunning`：缓冲 channel + 非阻塞 `Push`，
  队列满后任务被顺延，调度逻辑始终可以进入下一轮。

正式代码里等价的回归测试是
`internal/worker/pool_test.go` 的 `TestPushNeverBlocksWhenQueueIsFull`
和 `TestWorkerRecoversFromPanic`，可直接随项目测试运行：

```bash
go test ./internal/worker/... ./internal/database/... ./internal/config/...
```

### 复现 C：端到端（最接近线上，需要测试环境的 PostgreSQL）

1. 启动 miniflux，把 worker 数调小以便快速打满：
   `WORKER_POOL_SIZE=2 POLLING_FREQUENCY=1`（单位分钟，可按需改小源码常量测试）。
2. 建一个 feed，然后在数据库里手动制造锁竞争：
   ```sql
   BEGIN;
   UPDATE feeds SET title = title WHERE id = <feed_id>;  -- 不 COMMIT，保持 idle in transaction
   ```
3. 等待该 feed 进入刷新批次。修复前：两个 worker 陆续卡在锁等待上，
   随后调度器停在 `Push`，所有订阅不再更新，且日志无报错；
   修复后：约 `DATABASE_LOCK_TIMEOUT`（默认 10s）后日志出现锁超时错误，
   worker 恢复，后续批次继续正常调度。
4. 提交或回滚第 2 步的事务即可释放锁。

## 线上验证建议

- 故障发生时（重启前）抓取 goroutine 栈，能直接看到大量 goroutine
  停在 `database/sql.(*DB).query` / `pq` 的网络读，以及调度器停在
  `chan send`（worker.Pool 的 `p.queue <- job`）：
  `kill -QUIT <pid>`（栈会打到日志/stderr）或 `curl` pprof（若已启用）。
- 修复后观察日志：出现 `canceling statement due to lock timeout` /
  `statement timeout` 以及 `Worker queue is full ...` 告警都属于“系统自愈”
  的预期信号，说明过去会静默冻死的场景现在被显式处理了。
- 可通过 PostgreSQL 侧确认参数生效：
  `SELECT name, setting FROM pg_settings WHERE name IN ('statement_timeout','lock_timeout');`
