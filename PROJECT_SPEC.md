# Zone Rollout Control 项目规格

## 1. 项目目标

Zone Rollout Control 是面向 DNS 运维人员的 Go HTTP 控制面。它把区域版本编排、解析节点领取租约、探针结果登记和一致性判定串成一条可追溯的上线链。服务只使用本机 JSON 快照，不连接真实 DNS 网络，也不依赖外部平台。

本仓库是健康项目初始化基线，要求可构建、可运行、无预置缺陷。测试在本阶段有意延后，由后续测试阶段补充；当前通过真实 HTTP 路由上的工作流探针进行生产路径验证。

## 2. 使用者

- 区域滚动负责人：创建滚动计划、添加区域版本、启动上线并完成计划。
- 解析节点值守人员：为目标节点领取有期限的租约，提交探针结果。
- DNS 平台集成者：通过稳定 JSON API 接入内部自动化流程。

## 3. 核心实体

### RolloutPlan

一次明确目标序列号、节点数量、成功比例和延迟上限的滚动计划。

- `id`：服务生成的稳定标识。
- `zone`：区域名称，必须是小写 DNS 名称。
- `state`：`draft`、`active` 或 `completed`。
- `minimum_success_ratio`：成功节点占比下限，范围为 0 到 1。
- `maximum_latency_ms`：允许的最大探针延迟。
- `expected_nodes`：计划预期参与的节点数，范围为 1 到 500。
- `version`：每次计划聚合变化后递增。
- `requires_dual_approval`：随计划持久化的双人审批策略位。
- `latest_revision_id`：最近一次添加的区域版本。
- `latest_assessment_id`：最近一次完成的一致性判定。
- `created_at`、`activated_at`、`completed_at`：RFC3339 时间。

### ZoneRevision

计划中不可变的区域内容版本。

- `id`、`plan_id`、`serial`、`authority`。
- `entries`：区域条目数组，每项包含 `owner`、`type` 和 `value`。
- `entry_count`：由服务计算，客户端不能覆盖。
- `created_at`。

同一计划内的序列号必须严格递增。条目类型只接受 `A`、`AAAA` 和 `CNAME`。`A` 与 `AAAA` 的值必须是 IPv4 或 IPv6 地址，`CNAME` 的值必须是规范域名。

### ResolverNode

边缘解析节点档案。

- `id`、`node_name`、`region`。
- `state`：`ready`、`leased` 或 `quarantined`。
- `version`、`created_at`、`updated_at`。

### NodeLease

计划对某个解析节点的有期限占用。

- `id`、`plan_id`、`node_id`、`allocation_id`。
- `state`：`claimed`、`reported`、`released` 或 `expired`。
- `claimed_at`、`expires_at`、`finished_at`。
- `probe_id`：成功登记探针后的结果标识。

### LeaseGroup

一次租约分配请求的幂等记录。

- `id`、`plan_id`、`request_key`、`request_fingerprint`、`region`、`lease_ids`。
- 相同请求键和相同内容重放时返回原分配组，内容不一致时返回冲突。

### Probe

解析节点对指定租约提交的一次上线观察。

- `id`、`lease_id`、`plan_id`、`node_id`。
- `request_key`、`success`、`observed_serial`、`latency_ms`。
- `observed_at`、`stored_at`、`payload_fingerprint`。

### RolloutAssessment

一次计划范围内的确定性一致性判定。

- `id`、`plan_id`、`revision_id`、`request_key`。
- `verdict`：`consistent` 或 `review_required`。
- `reasons`：稳定原因代码列表。
- `probe_count`、`success_count`、`success_ratio`、`maximum_latency_ms`。
- `created_at`。

### PlanApproval

审批台账中的一条双人复核记录，独立于计划存在，只通过审批入口读写。

- `id`、`plan_id`。
- `plan_version`：记录时刻的计划聚合版本，不可变。
- `revision_id`、`revision_serial`、`revision_fingerprint`：记录时刻最新区域版本的标识与内容指纹，均由服务在事务内计算，客户端无法在请求体中指定或伪造。
- `role`：审批角色，取 `reviewer` 或 `approver`。
- `reviewer_id`：复核人标识。
- `decision`：`approved` 或 `rejected`。
- `request_key`、`fingerprint`：审批写入的幂等键与仅覆盖客户端字段的载荷指纹。
- `status`：`active` 或 `withdrawn`；记录内容不可变，撤回只翻转状态并记 `withdrawn_at`。
- `created_at`、`expires_at`、`withdrawn_at`。

读投影 `ApprovalView` 在记录之外附加 `current`、`effective` 和 `ineffective_code`（`withdrawn`、`expired`、`version_changed`、`revision_changed`），用于标记一条证据对当前计划是否仍有效。

## 4. 状态和规则

### 滚动计划状态

```text
draft --activate--> active --complete--> completed
```

- 只有 `draft` 可以激活。
- 只有 `active` 可以完成。
- 激活后不能继续添加区域版本。
- 激活和完成需要匹配当前版本。

#### 双人审批策略

`RolloutPlan.requires_dual_approval` 是随计划持久化的策略位：

- 未启用全局策略时新建计划默认为 `false`；请求体可显式置为 `true` 单计划启用。
- 通过 `--require-dual-approval` 启用全局策略后，新建计划默认为 `true`；请求体仍可显式置为 `false`。
- 历史快照中的计划没有该字段，反序列化后为 `false`：即使之后以全局策略启动，既有计划仍按旧流程直接激活，保持兼容。

启用双人审批的计划，激活必须由审批台账证明存在一组**仍有效**的批准：

- 每个必需角色（默认 `reviewer`、`approver`）各有一条有效的 `approved` 记录。
- 两个角色必须来自不同的 `reviewer_id`。
- 每条记录都绑定记录时的 `plan_version` 和最新区域版本；撤回（`withdrawn`）、超过 `expires_at`（`expired`）、计划版本变化（`version_changed`）或区域版本变化（`revision_changed`）后立即失效。
- 存在与当前版本匹配的有效 `rejected` 记录（`active_rejection`）时拒绝放行，即使两个批准角色已齐备。
- 审批只在 `draft` 状态可记录和撤回；激活后台账记录冻结。
- 审批绑定字段全部由服务在锁内计算，请求体只接受 `request_key`、`role`、`reviewer_id`、`decision`、`note`，严格解码会拒绝任何绑定字段。
- 激活事务在同一把锁、同一状态副本内核验台账，证据检查与状态迁移之间没有读改写窗口。

审批记录的确定边界：

- 幂等：同计划同 `request_key` 同载荷重放返回原记录；同键异载荷返回 `idempotency_conflict`。
- 重复审批：同一版本+区域版本下，一个角色只允许一条有效决策，重复返回 `approval_conflict`；同一复核人不得用第二个角色再次记录，重复返回 `approval_conflict`。
- 撤回仅限本人、仅限 `draft`、仅可撤回一次，否则返回 `approval_not_withdrawable`。
- 证据不足时激活返回 `approval_required`，并在 details 中给出 `missing_roles` 和稳定原因代码。
- 激活本身的并发：状态合法性检查先于台账闸门，重复迁移统一返回 `invalid_state`；与 `expected_version` 不匹配返回 `version_conflict`。

### 节点与租约生命周期

```text
ready --lease--> leased --report--> ready
                   |
                   +--release/expire--> ready
```

- 隔离节点不能被分配。
- 一个节点同一时刻最多有一个 `claimed` 租约。
- 租约到期后不能提交探针。
- 首次有效探针完成后，租约转为 `reported`，节点恢复 `ready`。

### 区域版本规则

- 计划必须已有一个版本才能激活。
- 同一计划中的序列号必须严格递增。
- 序列号使用无符号 32 位整数。
- 条目数量范围为 1 到 500。
- 区域名称、所有者名称和规范名称必须规范化为小写并移除末尾句点。
- 重复加入完全相同的版本内容返回 `duplicate_revision`。

### 探针幂等

- `observed_at` 不能晚于服务时间五分钟以上。
- `latency_ms` 范围为 0 到 600000。
- 同租约同请求键同内容重放返回原探针，不重复计数。
- 同请求键不同载荷返回 `idempotency_conflict`。
- 探针标识符由服务生成，客户端不能覆盖。

### 一致性判定

- 至少需要一个探针。
- 成功比例按成功探针数除以全部探针数计算，结果保留 6 位小数。
- 每个参与节点都必须提交至少一个探针。
- 所有成功探针的 `observed_serial` 必须等于计划最新版本的序列号。
- 成功比例必须不低于 `minimum_success_ratio`。
- 最大延迟必须不超过 `maximum_latency_ms`。

不满足条件时结论为 `review_required`，并返回以下稳定原因代码：

- `no_probes`
- `node_not_observed`
- `serial_mismatch`
- `success_ratio_below_minimum`
- `latency_limit_exceeded`

## 5. 工作流

### W1 计划生命周期和区域版本

入口：

- `PUT /v1/plans`
- `PATCH /v1/plans/{id}/revisions`
- `PATCH /v1/plans/{id}/activate`
- `PATCH /v1/plans/{id}/complete`

步骤：

1. HTTP 层严格解码 JSON。
2. 计划服务校验资金、区域名称、版本内容和状态迁移。
3. 存储层在锁内创建计划或版本，并原子替换状态快照。
4. 返回带版本的新聚合。

成功行为：计划按 `draft -> active -> completed` 迁移，区域版本在激活前可追加。

失败行为：重复序列号、非法名称、缺少版本、版本冲突或非法迁移不会修改状态。

### W2 节点领取租约

入口：`PATCH /v1/plans/{id}/leases`。

步骤：

1. 入口校验计划、区域、请求键、租约秒数和数量。
2. 分配服务清理已到期租约。
3. 按区域筛选 `ready` 节点并以稳定顺序选择。
4. 在一个事务中把节点改为 `leased`，写入租约组和租约。

成功行为：每个节点获得一个有期限的 `claimed` 租约。

失败行为：计划非活动、无可用节点、请求键同键异载荷或保存失败时不会产生部分租约。

### W3 探针登记

入口：`PATCH /v1/leases/{id}/probes`。

步骤：

1. 入口校验租约、请求键、序列号、延迟和时间。
2. 探针服务计算载荷指纹。
3. 存储层写入探针、完成租约并释放节点。
4. 返回探针及关联租约状态。

成功行为：首条探针落库；同键同内容安全重放。

失败行为：租约过期、计划未激活、同键异载荷或持久化失败时保持原状态。

### W4 一致性判定

入口：

- `PATCH /v1/plans/{id}/assessments`
- `GET /v1/plans/{id}/assessments/latest`

步骤：

1. 入口校验计划、请求键和最新区域版本。
2. 判定服务收集计划探针并按节点分组。
3. 纯领域计算器生成比例、原因和最终结论。
4. 存储层写入不可变判定并更新计划摘要。

成功行为：返回确定性的 `consistent` 或 `review_required`。

失败行为：缺少区域版本、没有探针、请求键冲突或保存失败不会更新计划摘要。

### W5 双人审批与激活闸门

入口：

- `POST /v1/plans/{id}/approvals`
- `GET /v1/plans/{id}/approvals`
- `GET /v1/approvals/{approvalId}`
- `POST /v1/approvals/{approvalId}/withdraw`
- `PATCH /v1/plans/{id}/activate`（闸门在激活事务内生效）

步骤：

1. HTTP 层严格解码审批请求，只接受复核人控制的字段。
2. 审批服务在锁内读取计划和最新区域版本，校验草稿状态、角色和策略。
3. 服务计算计划版本、区域版本标识、序列号、内容指纹和过期时间，写入不可变记录与请求键索引。
4. 激活时在同一事务内用纯领域计算评估每条记录的有效性，确认两个必需角色、两名不同复核人的仍有效批准。
5. 只有评估通过才执行 `draft -> active` 迁移。

成功行为：两名不同复核人分别以 `reviewer` 和 `approver` 批准当前版本后，计划可以激活；台账读接口返回每条记录的当前有效性和整计划的就绪摘要。

失败行为：证据缺失、单人双角色、有效拒绝、记录撤回、记录过期、版本漂移、请求键冲突或非草稿状态都返回稳定错误且不产生部分写入；激活请求体无法提供任何审批证据。

## 6. 模块边界

| 模块 | 路径 | 责任 |
| --- | --- | --- |
| entry | `cmd/zrc` | 进程启动、参数解析、信号关闭 |
| httpapi | `internal/httpapi` | 路由、严格解码、中间件和错误映射 |
| plan | `internal/plan` | 计划生命周期和区域版本规则 |
| fleet | `internal/fleet` | 解析节点档案、唯一名称和分页查询 |
| allocation | `internal/allocation` | 节点筛选、幂等分配组和租约生命周期 |
| probe | `internal/probe` | 探针校验、幂等写入和租约完成 |
| assessment | `internal/assessment` | 探针聚合和一致性判定 |
| approval | `internal/approval` | 独立审批台账的记录、撤回、读投影和激活闸门 |
| domain | `internal/domain` | 聚合、值校验、状态迁移和纯计算 |
| store | `internal/store` | 锁内副本事务和原子 JSON 持久化 |
| platform | `internal/platform` | 时钟、标识、游标和文件锁 |
| runtime-probe | `internal/workflowprobe`, `cmd/workflowprobe` | 真实 HTTP 路径验证 |

依赖方向：入口和 HTTP 层依赖应用服务；应用服务依赖领域规则和存储；领域包不依赖 HTTP、文件系统或网络。

## 7. 对外接口

成功响应：

```json
{"data": {}, "meta": {"request_id": "..."}}
```

失败响应：

```json
{"error": {"code": "validation_failed", "message": "human readable message", "details": {}}}
```

主要路由：

- `GET /healthz`
- `PUT /v1/nodes`
- `GET /v1/nodes`
- `PUT /v1/plans`
- `GET /v1/plans`
- `GET /v1/plans/{id}`
- `PATCH /v1/plans/{id}/revisions`
- `PATCH /v1/plans/{id}/activate`
- `PATCH /v1/plans/{id}/complete`
- `PATCH /v1/plans/{id}/leases`
- `PATCH /v1/leases/{id}/release`
- `PATCH /v1/leases/{id}/probes`
- `PATCH /v1/plans/{id}/assessments`
- `GET /v1/plans/{id}/assessments/latest`
- `POST /v1/plans/{id}/approvals`
- `GET /v1/plans/{id}/approvals`
- `GET /v1/approvals/{approvalId}`
- `POST /v1/approvals/{approvalId}/withdraw`

## 8. 工程机制

- 状态迁移：计划、节点和租约使用显式迁移表。
- 乐观并发：计划版本不匹配时拒绝变更。
- 幂等写入：租约分配、探针登记和审批记录都保存请求键和载荷指纹。
- 资源生命周期：租约带截止时间，支持释放、完成和过期清理；审批带过期时间，撤回仅翻转状态。
- 不可变绑定：审批记录在服务端绑定计划版本和区域版本内容指纹，激活在锁内重新核验证据有效性。
- 原子持久化：事务先复制状态，成功后再通过临时文件替换快照。
- 错误传播：领域错误稳定映射到 HTTP 状态和错误代码。
- 边界校验：DNS 名称、IP、序列号、延迟、比例、分页和能力均显式验证。
- 并发入口：HTTP 处理器可并发运行，状态修改通过互斥锁串行化；并发激活只有一个赢家。

这些机制为后续工程任务提供了多个独立行为面，但本阶段不创建缺陷、不写测试，也不生成缺陷票据。

## 9. 验证计划

`.project-runtime.json` 的 `testing` 固定为 `deferred`，不生成单元测试、夹具或浏览器测试。交付前使用 `cmd/workflowprobe` 启动真实本机服务并在随机端口完成：

1. 计划创建、版本追加、激活、完成和非法重复迁移。
2. 节点分配、租约唯一性、幂等重放和无可用节点冲突。
3. 探针首写、同键重放、同键异载荷冲突和释放冲突。
4. 一致性通过、审核触发、最新结果查询和幂等重放。
5. 策略关闭时无审批直接激活、台账读接口可用，以及计划显式启用仍被闸门拦截。
6. 双人审批：缺少角色、单人批准、双人放行、伪造绑定字段拒绝、重复角色/同人双角色冲突、同键异载荷冲突、拒绝阻断、撤回失效、过期失效和审批后追加版本失效。
7. 并发激活只有一个请求成功，其余得到确定性的版本冲突或非法迁移错误。

每条探针使用独立临时目录，不访问外部网络，结束后关闭服务。
