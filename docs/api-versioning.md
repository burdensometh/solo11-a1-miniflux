# API 版本协商与向后兼容方案

## 1. 目标与硬约束

- 老调用方**一行代码不改**，升级服务端后行为与升级前完全一致；老版本至少再保留 6 个月，期间不强制升级、不下线、不改语义。
- 新调用方可以显式选择新行为：新增响应字段 + 游标（keyset）分页。
- 新老行为在同一套 URL、同一组路由上共存，通过**版本协商**区分，不复制一套 `/v2/...` 路由。
- 任何对 v1 契约的意外漂移都必须被测试拦截。

## 2. 版本协商机制

协商逻辑位于 `internal/apiversion`，以中间件形式挂在认证之后、业务路由之前。

### 2.1 请求方如何选择版本

| 方式 | 示例 | 说明 |
| --- | --- | --- |
| 不发任何版本标识 | 普通 `GET /v1/entries` | 一律按 **v1** 处理，这就是老客户端的形态 |
| 请求头 | `X-API-Version: 2` | 推荐方式，优先级最高 |
| Accept 媒体类型参数 | `Accept: application/json; version=2` | 适合不方便加自定义头的环境 |

头部同时接受 `2`、`v2`、`2.0` 三种写法。无法识别、格式错误或版本号超出服务端支持范围时，**回退到 v1 而不是报错**——代理、旧服务端、未来新客户端之间的组合都不会把存量流量打挂。

### 2.2 响应里如何确认实际版本

每个响应都会带：

- `X-API-Version: 1|2`：本次请求实际协商出的版本（回头部）。
- `X-API-Version-Supported: 1, 2`：本服务端支持的版本清单。

新客户端应断言响应头与自己的请求一致，而不是靠猜。CORS 已同步放开这两个头（`Access-Control-Allow-Headers` 与 `Access-Control-Expose-Headers`），浏览器端可直接使用。

## 3. v1 契约为什么不会被破坏

这是方案的核心，落在三个具体机制上：

1. **冻结的 DTO**。v1 不再直接序列化内部 `model.Entry`，而是走 `internal/api/entry_dto.go` 中的 `v1Entry`，字段名、类型、`omitempty`、排列与历史输出逐项对齐。以后给 `model.Entry` 加字段不会再影响 v1 响应。
2. **逐字节回归测试**。`TestV1EntryJSONMatchesLegacyModel` 用同一份数据分别序列化旧 `model.Entry` 和新 `v1Entry` 并做深比较；`TestV1EntryHasNoV2Fields` 断言 v2 专有字段（`entry_id`、`external_url`、`excerpt`、`image_url`、`has_media` 等）永远不会出现在 v1 负载里；`TestV1EnvelopeShapeLocksTheHistoricalContract` 锁定列表信封仍然只有 `total`、`entries` 两个键。
3. **默认即 v1**。协商中间件的零值默认是 v1，即使某条请求路径绕过了中间件，也只会得到老行为。

v1 的查询参数（`limit`/`offset`、全部历史 `order` 取值、`before/after` 时间过滤等）、状态码（如更新条目仍是 201）、错误结构（`{"error_message": ...}`）均保持原样。

## 4. v2 的新行为

只有携带 `X-API-Version: 2`（或等价 Accept 参数）的请求才会进入 v2 处理器。

### 4.1 新增条目字段（只增不删）

v2 条目保留全部 v1 字段（同名同类型），并新增：

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `entry_id` | int64 | 资源 ID 的 v2 命名，`id` 保留为别名 |
| `external_url` | string | 页面 URL 的显式命名，`url` 保留为别名 |
| `excerpt` | string | 由正文派生的纯文本摘要（上限 350 字符，自动剥离 script/style） |
| `image_url` | string | 正文中第一张可用图片，无图时省略 |
| `has_media` | bool | 是否带音频/视频附件 |

列表信封变为：

```json
{
  "entries": [ ... ],
  "limit": 100,
  "has_more": true,
  "next_cursor": "eyJ2IjoxLCJjIjoicHVibGlzaGVkX2F0IiwiaWF0Ijo...",
  "total": 1234
}
```

其中 `total` 为**可选**：只有请求带 `with_total=true` 时才计算并返回（计数查询对大库不便宜，纯翻页场景用 `has_more` 即可）。

### 4.2 游标分页语义

- 首页：不带 `cursor`，可带 `limit`（默认 100，最大 1000）、`order`、`direction`。
- 下一页：把上一页响应里的 `next_cursor` 原样回传即可。
- 排序采用 `(排序列, id)` 复合键，顺序是全序且稳定的，同一 `published_at` 下不会翻页丢行或重复。
- 游标令牌为不透明 base64 字符串（`RawURLEncoding`，可直接放进 query），内含格式版本号、排序位置、签发时间；有效期 24 小时，过期或被篡改返回明确的 400 并提示重新从首页开始。
- v2 下 `offset` 与 `before_entry_id`/`after_entry_id` 被显式拒绝（400），防止两种分页模型混用产生静默错乱。`before`/`after`/`published_before` 等**时间过滤**参数不受影响，继续可用。
- v2 仅支持可做全序 keyset 的排序：`id`、`published_at`、`changed_at`、`created_at`；`title`/`author` 等 v1 排序值在 v2 下返回 400 并给出可选值提示。

## 5. 各类调用方需要做什么

### 5.1 十几个停留在老版本的客户端

**什么都不用改。** 不发版本头即得到 v1；字段集合、信封、分页参数、状态码与升级前一致。服务端在整个支持窗口内保证 v1 可用。

### 5.2 想使用新字段/游标的新客户端

- 请求加头 `X-API-Version: 2`，并读取响应头确认协商结果。
- 用 `cursor` + `has_more` 替代 `offset` 翻页；需要总数时显式传 `with_total=true`。
- 对 400 做显式处理：混用了 `offset`、游标过期、排序不支持等都会返回带 `error_message` 的 400，游标过期时丢弃旧令牌、从首页重新开始即可。
- 新字段全部是增量字段，即便部署节奏不一致（新客户端打到稍旧的服务端），解码也不会失败，只是新字段取零值。

### 5.3 使用本仓库 Go 客户端库的调用方

- 现有代码（`client.Entries`、`Filter.Offset` 等）签名与行为不变，默认不发送版本头。
- 需要 v2 时在构造处加一个选项：
  `NewClientWithOptions(endpoint, WithAPIKey(key), WithAPIVersion(APIVersion2))`，
  然后调用新增的 `EntriesV2` / `EntriesV2Context`，用 `EntryPageFilterV2`（无 Offset 字段）和返回的 `EntryPageV2.NextCursor` 翻页。
- 在非 v2 客户端上调用 `EntriesV2` 会立即返回 `ErrV2Required`，属于编程期就能发现的错误，不会发出语义错误的请求。

## 6. 6 个月支持窗口与退役计划

1. **当前（窗口开始）**：v1 为默认且冻结，v2 可显式协商；两套行为都有自动化测试守护。
2. **窗口内（约第 1–5 个月）**：只允许给 v2 加增量字段；v1 除安全修复外不动。可通过访问日志按 `X-API-Version` 统计 v1 流量，评估剩余存量。
3. **退役前（第 5 个月）**：若仍有 v1 调用，通过响应头之外的渠道（发布说明、登录横幅等）公告；不改变 v1 响应本身，避免误伤。
4. **窗口结束后（第 6 个月起）**：v1 路由保留但可改为返回 `426 Upgrade Required`（或按团队决策继续保留）；在此之前任何 PR 都不得删除 v1 DTO 与 v1 处理器，对应回归测试一并保留。

## 7. 改动清单

| 文件 | 作用 |
| --- | --- |
| `internal/apiversion/` | 版本协商：头部/Accept 解析、上下文、回显中间件及测试 |
| `internal/api/entry_dto.go` | 冻结的 v1 DTO、增量 v2 DTO、摘要/首图提取 |
| `internal/api/cursor.go` | 游标令牌编解码、白名单、过期与篡改校验 |
| `internal/api/entry_handlers.go` | `findEntries` 按版本分流；v1 处理器保持历史契约 |
| `internal/api/entry_handlers_v2.go` | v2 游标分页处理器与参数校验 |
| `internal/storage/entry_cursor.go` | keyset 分页 SQL 构造、游标位置提取、去分页计数 |
| `internal/api/api.go`、`middleware.go` | 中间件接线、CORS 放开版本头 |
| `client/`（`options.go`、`request.go`、`model.go`、`entries_v2.go`） | Go 客户端：`WithAPIVersion` 选项与 v2 翻页方法，旧 API 不变 |
