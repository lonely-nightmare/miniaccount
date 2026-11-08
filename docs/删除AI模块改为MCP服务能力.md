# 移除 AI 辅助模块，改为对外 MCP 服务能力

> 第 3 版（第 4 次修订）：占用锁改为文件身份制（符号/硬链接
> 归并同一身份）、空分片经 `audit_segment` 恢复链状态、
> 幂等行持久化 `audit_state`/`initial_voucher_updated_at`、
> SSE 失效改关流+后续 404；订正 Windows 无 `syscall.Flock`
> 符号、旧连接恢复语义。

## 目标

1. **整体删除 AI 辅助模块**（记账助手 + 会计顾问 agent + 提示词配置 +
   建议审批流 + 模型服务配置 + CLI `ai`/`ai-config` 命令）。
2. **新增完整 MCP 服务端能力**：外部 MCP 客户端（Claude Desktop 等）
   通过 MCP 直接查账、建凭证草稿、维护辅助档案。

## 一、删除清单（AI 模块全景）

### Go 后端

| 路径 | 内容 | 处理 |
|---|---|---|
| `internal/ai/aiprovider/` | 27 文件：provider/agent/suggest/prompt/answer + 领域工具 | 整包删除 |
| `internal/domain/ai/` | 提示词分层配置 + AI 建议→凭证 | 整包删除 |
| `internal/service/ai_wire.go` | provider 工厂、Accept/Reject、提示词配置 | 删除 |
| `internal/service/accountant.go` | 会计顾问会话 + agent 工具集 | 删除 |
| `internal/service/service.go` | `AIConfig`/`AIConfigInfo` 及 `sqlite.AIProviderConfig`/`AIStats` 依赖 | 删除 |
| `internal/service/{taxreturn,auditdoc,evidence}.go` 的 `*Brief` 方法 | 只喂 agent 工具；GUI 用同名 View | 删除方法及测试 |
| `internal/service` AI 测试 | `ai_test.go`/`accountant_tools_test.go` 等 | 删除 |
| `internal/store/sqlite/ai_repo.go` | `ai_provider`/`ai_suggestion`/`ai_session` 读写 | 删除 |
| `internal/store/sqlite/ai_ledger.go` | agent 工具供数查询 | 逐项核对（见三） |
| `desktop` AI 绑定 | `AISuggest`/`AIConfig`/`SaveAIProvider`/`AISuggestions`/`Accept`/`Reject`/提示词×4/`Accountant×3` | 删除 |
| `cmd/miniaccount/main.go`+`helpers.go` | `ai`、`ai-config` 子命令 | 删除 |

### 数据库

`ai_*` 三表**不删**（历史审计痕迹保留）；备份剔除 `api_key`
的逻辑保留。

### 前端

`AIAssistant.vue`、`/ai` 路由、api.js AI 段与 mock 假数据、
Settings 模型服务卡、GUI 两条 AI 用例 —— 全删。

### 保留

`voucher.SourceAI`/`CreatedByAI`/`audit.SourceAI` —— 历史数据
标记，枚举留，不再产生新值。

## 二、MCP 服务架构

### 2.1 组成

```
internal/mcpserver/            服务端核心：工具注册 + 契约 + service 适配
cmd/miniaccount mcp --db <路径> [--readonly] [--operator <名>]
                               stdio 传输（Claude Desktop 子进程模式）
desktop App「MCP 服务」卡      HTTP 传输：127.0.0.1:<port>
```

SDK：`github.com/modelcontextprotocol/go-sdk`（版本钉 ≥7 天前发布）。

### 2.2 HTTP 访问控制

四层，中间件统一拦截（tools/resources/prompts 全路径）：

1. **凭据**：开启服务生成 256-bit token，存
   `~/.mini-account/mcp.json`（0600，不入账套）；
   每请求必带 `Authorization: Bearer <token>`。
2. **Origin**：有 Origin 头只放行 `localhost|127.0.0.1` 来源；
   无 Origin（非浏览器客户端）放行。
3. **Host**：精确匹配 `127.0.0.1:<port>`/`localhost:<port>`，
   挡 DNS rebinding。
4. **失效**：设置页「重新生成」立即签发新 token 并吊销旧值；
   关闭服务即吊销。

### 2.3 账套会话生命周期（明确顺序，不用双机制）

`book()` 是「取指针即放锁」，本设计**新增**「调用持锁」语义，
不复用现有语义也不叠加 WaitGroup：

- **读侧**：每次 MCP 业务调用 =
  `mu.RLock → 按 MCP-Session-Id 查会话 → 比对会话 generation
  与 a.bookGeneration → 不符：RUnlock 并销毁会话、HTTP 404；
  相符：执行 service 调用 → RUnlock`。
  RLock 只覆盖**一次业务调用**，绝不覆盖整个 HTTP 连接。
- **写侧**（Open/Close/Restore）：`mu.Lock → generation++ →
  换 svc → Unlock`。写锁排队本身就是排空——在途调用持读锁
  期间写锁拿不到，天然等其完成；不再需要单独 WaitGroup。
- **流连接**（SSE）：不持账套锁。服务端向该连接发请求或推
  事件前，在短 RLock 内复核 generation；不符则**关闭该条
  SSE 流**（已建立的响应不能中途改状态码）——此后携带旧
  Session ID 的请求返回 HTTP 404，客户端按规范重新
  initialize。
- **会话机制用标准通道**：initialize 响应按规范下发
  `MCP-Session-Id`；`{bookId(=path), generation}` 放响应
  `_meta` 作附加元数据，**不替代**标准会话管理——失效一律
  以 HTTP 404 表达，客户端按规范重新 initialize，而不是
  靠工具错误文字「请重连」。
- **stdio**：独立进程持有 Service 无切换问题，但受 2.4
  占用协议约束。`--db` 必填且先 `PeekBook` 验证文件存在且
  为已初始化账套——打错路径**拒绝启动**，绝不建空库。

### 2.4 跨进程账套占用协议（新增，恢复安全）

GUI `RestoreBackup` 只关自己的连接；stdio/CLI 持旧连接继续
读写时，恢复替换 DB 文件、清 WAL/SHM —— 旧进程对着已换
inode 的句柄继续写，数据写进「幽灵账套」。

**方案：按账套文件身份（而非调用方路径）取锁。**

- **锁身份 = 文件身份**：unix 取 `(device, inode)`，
  Windows 取 `GetFileInformationByHandle` 的
  (volume serial, file index)。路径经 `EvalSymlinks`+
  `Abs` 规范化仅用于定位文件与展示——符号链接、硬链接
  别名天然归并到同一身份、同一把锁，不存在「stdio 走
  软链、GUI 走真实路径锁住两个文件」的绕过。
  平台/文件系统给不出稳定文件身份时**拒绝打开**，
  不回退到按路径锁。
- **锁文件位置**：统一放 `~/.mini-account/locks/
  <身份散列>.lock`。不在账套旁边——`--readonly` 打开
  只读目录里的账套也能取锁；恢复替换账套文件时锁文件
  不受影响；文件常驻，Shutdown **不删除重建**
  （删除本身引入重建竞态），崩溃由 OS 释放锁。
- **取锁时序**：stat 得身份 → 取锁 → **复核路径当前
  身份与所锁身份一致**（取锁间隙文件被换则释放重取）
  → 打开 SQLite。关闭顺序相反：连接彻底关闭后才释放锁。
- **平台实现**：`syscall.Flock` 只在 unix 系存在，
  Windows **无此符号**——unix 用 `flock`，Windows 用
  `x/sys/windows.LockFileEx`，或引入跨平台库（钉版本）。
- **共享/独占**：`service.Open` 获共享占用（非阻塞失败
  →「恢复中，请稍后」）；`service.Restore` 获独占
  （TryLock 失败 →「正被 GUI/CLI/MCP 使用，请先断开」），
  完成后释放再走 `service.Open`。`App.RestoreBackup` 与
  CLI `restore` 都经 `service.Restore`，一处两端生效。
- **新建账套无身份**：先按规范化路径（父目录
  `EvalSymlinks` + 文件名）取路径锁 → 建文件 →
  换身份锁。
- **已知限制如实写**：文件锁不吊销不遵守协议的进程
  （旧版本软件、第三方工具直接改库）——协议只约束
  本次及以后版本的客户端。
- 验收：「符号链接打开 + 真实路径恢复 → 恢复被拒」；
  「stdio 持续读写时恢复被拒」；「旧连接关闭并释放
  占用后恢复成功，重新连接读到恢复后的数据」。

### 2.5 并发与幂等

- **乐观并发（GUI 同改）**：`voucher.updated_at` 已有。
  `VoucherDetail` 下发 `updatedAt`；`VoucherRequest` 增
  `expectedUpdatedAt`，`VoucherEditor` 载入时保存原值、
  保存时回传；`DeleteVoucher(id, expectedUpdatedAt)` 绑定
  同步改造。底层 `UPDATE/DELETE … WHERE id=? AND
  updated_at=?`，0 行 → `conflict` 错误。
  **GUI 也强制携带**（仅 MCP 传挡不住 GUI 编辑器的
  陈旧窗口覆盖）；新建豁免。接口改造范围：编辑器组件、
  `VoucherRequest`、删除绑定、`VoucherInput`、
  `DeleteVoucher` 签名。
- **创建幂等**：`create_draft_voucher` 可选
  `clientRequestId`。账套库内新表
  `mcp_idempotency(request_id PK, request_hash TEXT,
  voucher_id, initial_voucher_updated_at TEXT,
  audit_state TEXT, created_at)`：
  - `request_hash` = 规范化请求（字段排序后 JSON）摘要；
    **同键不同内容返回 `idempotency_conflict`**，
    不得返回旧凭证冒充成功。
  - `initial_voucher_updated_at` = 首次落库时凭证的
    `updated_at` 精确值（插入路径 `created_at`/`updated_at`
    是两次 `nowString()`，不能靠它们相等判断修改）。
    重试时与当前 `updated_at` 比较 → `modifiedSince`。
  - `audit_state` = `pending`/`recorded`/`failed`：
    幂等行与凭证同事务以 `pending` 落库；提交后写
    审计成功置 `recorded`、失败置 `failed`。
    **重试返回持久化的原操作审计状态**——新
    context/sink、进程重启都不影响证明原操作是否
    留痕；`pending` 表示「提交后崩溃，未确认已留痕」，
    `failed` 附带原失败信息为 `auditWarning`。
  - 命中同键同内容：原凭证未变 → `{voucher,
    idempotent:true}`；已变 → `{voucher(当前态),
    modifiedSince:true}`；已删 →
    `idempotent_target_deleted`，**不得悄悄重建**。
  - 并发同键靠 PK 唯一约束裁决；未传键 → schema
    写明不保证幂等。
- **审计跨进程**（见 2.6）。

### 2.6 审计与操作人

- **来源传播**：`service.WithCaller(ctx, Caller{Operator,
  Source, Warn *CallWarnings})`。`recordAudit` 组装
  `audit.Entry` 时优先取 caller operator/source；
  **写日志失败时除 stderr 外，把错误追加进
  `Caller.Warn`** —— MCP 适配层调用后读 Warn，映射为
  工具结果的 `auditWarning` 字段。GUI 不传 sink 时
  行为同今（stderr）。
  注意：sink 只覆盖**本次调用**的警告传递；幂等重试
  需要证明**原操作**是否留痕，由 `mcp_idempotency.
  audit_state` 持久化承担（见 2.5），不靠内存 sink。
- `audit.Source` 新增 `SourceMCP`（「外部接口」）；
  `voucher.Source` 新增 `SourceMCP`（「MCP」）—— 两套
  枚举分记。
- **凭证来源字段更新时保留**（P1 修正）：`SaveDraft`
  更新路径（id>0）从事务内原记录保留 `source`/
  `source_id`/`created_by_ai` —— 现有 `buildVoucher` 只在
  `in.Source != ""` 时设置而 repo UPDATE 无条件覆写，
  GUI 重存 MCP 草稿会把来源刷回 `manual`。修复后：
  GUI 原样保存 MCP 草稿来源仍是 `mcp`；MCP 修改
  银行/发票生成的草稿保留原 `source`；本次调用的来源
  只进审计日志。**双向编辑回归**纳入验收。
- **补缺口**：`SaveDepartment`/`SaveEmployee` 补
  `recordAudit`（含 caller）；`DeleteVoucher` operator 改
  caller（原制单人挪进 detail 上下文）。
- 操作人：stdio `--operator` 缺省记账人；HTTP 用设置页
  「MCP 操作人」。query 走 `RecordView`（查看类开关沿用）。

### 2.7 审计链跨进程正确性（锁 + 状态刷新，缺一不可）

`auditdb` 的 `seq`/`head`/活动分片是**进程内缓存**。仅给
`Append` 套文件锁不够：A 已轮转到第 2 片，B 仍缓存第 1
片句柄和旧链头 —— 串行执行也会写错片或断链。

**设计：`Store.Append` 临界区内完整状态再同步。**

```
文件锁（audit 目录 .lock，跨平台实现同 2.4）
  发现活动分片（max audit-*.db）
  活动片 ≠ 缓存片 → 打开并切换句柄
  恢复链状态（按分片内容分两种）：
    分片非空 → 取本片末条日志的 seq/hash
    分片为空 → 取 audit_segment 行的 prev_seq/prev_hash
    元数据缺失或读取失败 → 报错，绝不退回 Genesis
  rotateIfNeeded → 追加 → 更新缓存
解锁
```

空分片恢复必须走 `audit_segment` 元数据：现有
`refreshChain` 对空片会恢复成 `seq=0/head=Genesis` ——
「第二片已建元数据、首条日志插入失败或进程退出」后，
下次追加会从错误的起点续链。首次建片也纳入同一临界区。
文件锁跨平台（同 2.4 实现）。
**测试用确定性交错**：「A 写满轮转 → B 以陈旧缓存追加
→ A 再追加」链不断、seq 唯一；「轮转后、首条插入前
模拟故障 → 重开进程追加」链起点正确，不靠并发压测碰运气。

### 2.8 工具清单（14 读 + 6 写）

方法名按源码核对；「*」为需新增的 service 入口。

**读**

| 工具 | 说明 | service 方法 |
|---|---|---|
| `book_info` | 账套信息 | `s.Book` |
| `list_accounts` | 科目树 | `s.AccountOptions`（已存在） |
| `list_periods` | 期间状态 | `s.Book().Periods`（已存在） |
| `query_vouchers` | 凭证列表 | `s.Vouchers`；`VoucherQuery` 扩 `AccountPrefix`/`Offset` |
| `get_voucher` | 凭证详情 | `s.Voucher`；返回补 `updatedAt` |
| `get_ledger` | 明细账 | `s.LedgerDetail`*（从 desktop 层收进） |
| `trial_balance` | 试算平衡 | `s.TrialBalance`*（包 repo） |
| `balance_sheet` / `income_statement` | 报表 | `s.BalanceSheet`/`s.IncomeStatement`*（包 repo `Build*`） |
| `list_contacts` | 往来单位 | `s.Contacts` |
| `list_departments` / `list_employees` | 部门/员工 | `s.Departments`/`s.Employees` |
| `list_invoices` / `list_bank_flows` | 发票/流水 | `s.Invoices`/`s.BankFlows` |
| `list_attachments` | 单据附件列表 | `s.ListAttachments`（不含宿主机 path） |
| `read_attachment` | 读附件内容 | `store.Get`（sha256 校验）→ base64 |
| `list_orphan_attachments` | 孤儿附件报告 | `s.OrphanAttachments`（只报告） |
| `attachment_coverage` | 凭证附件覆盖率 | `s.AttachmentCoverage` |

**写（全部进操作日志）**

| 工具 | 说明 | 约束 |
|---|---|---|
| `create_draft_voucher` | 建凭证草稿 | `SaveVoucher` 全量校验；`source=mcp`；可选幂等键 |
| `update_draft_voucher` | 改草稿 | 仅 draft；`expectedUpdatedAt` 必填；来源字段保留 |
| `delete_draft_voucher` | 删草稿 | 仅 draft；`expectedUpdatedAt` 必填 |
| `upsert_contact` | 建/改往来单位 | 部分更新（2.9） |
| `save_department` | 建/改部门 | 部分更新（2.9） |
| `update_employee` | **仅修改**员工档案 | 部分更新 + 字段白名单（2.9） |
| `upload_attachment` | 挂附件 | `AttachDocument`；base64 内容；内容寻址天然幂等 |
| `delete_attachment` | 解除附件关联 | 只删关系行，物理文件留孤儿清理 |

**不开放**：过账、结账/反结账、备份/恢复、宿主机路径
（附件只走 base64 内容，不给服务端磁盘路径）、审计日志
写入接口、密钥配置、任意 SQL、文件系统。

**Resources**：`book://info`/`book://accounts`/`book://periods`
（同样过凭据与会话检查）。无 Prompts。

### 2.9 档案部分更新（Patch）

- **三态字段**：每个可写字段用 `Field[T]{Set, Null, Value}`：
  `Set=false` 缺席不动；`Set=true,Null=true` 显式清空
  （`parentId` 由此置顶层部门）；`Set=true,Null=false` 设值。
  统一覆盖 string/int64/bool，解决 `*int64` 分不出
  「缺席 vs null」的问题。SDK 侧用
  `map[string]json.RawMessage` 先测出席再解码。
- **事务复用**：`saveContact`/`SaveDepartment`/
  `SaveEmployee` 各自 `WithTx`，外层 Patch 不能直接套——
  抽出收 tx 参数的内部方法（`saveContactTx` 等）：
  `Patch = WithTx{ 读旧 → 合并 → 校验 → 写 }`，
  `recordAudit` 在**提交后**（审计库独立，本就不该进账套事务）。
- **不重放创建默认值**：Patch 写**合并后的领域实体**，
  不重建 `EmployeeInput` 再过 `SaveEmployee` —— 否则
  `SIBase=BaseSalary` 之类的创建回落会在不该触发时触发；
  白名单外字段原样保留由「读旧合并」天然保证。
- **员工白名单**（`update_employee` 仅修改、不可建）：
  name/phone/idCard/bankName/bankAccount/
  expenseAccountCode/companyAccountCode/remark。
  **排除**：baseSalary/siBase/hfbBase（走 `AdjustSalary`）、
  deptId（走 `TransferEmployee`）、leaveDate（走
  `ResignEmployee`）、**`enabled`**（`IsActiveIn` 首查它，
  改了等于绕过人事状态留痕，从白名单剔除）。
- 档案写**立即生效**，工具描述写清无草稿缓冲。

### 2.10 契约约定

- **金额**：一律字符串元进出；输入 `ParseYuan`，输出
  `Money.String()`。整数分不出现在工具结果里。
- **分页**：统一 `{items,total,limit,offset,hasMore}`；
  `limit ≤ 500` 全工具一致（凭证默认 100）。
- **报表口径**：仅已过账分录，描述写明；勾稽异常返回
  `balanced`+`issues[]` 不报错。
- **错误**：结构化 `{code,message}`；新增码 `conflict`、
  `idempotency_conflict`、`idempotent_target_deleted`。

### 2.11 GUI 实时性

- HTTP 同进程：mutation 成功后
  `wails.EventsEmit("mcp:changed",{entity,id})`；列表页在
  **编辑器/弹窗未开**时自动重查，编辑中只弹「数据已被
  外部修改」通知，不覆盖在录表单；编辑器保存被乐观锁
  挡住时明确提示。
- stdio 跨进程：本进程感知不到，设置页写明「外部改动
  请刷新」；mtime 轮询列后续。

### 2.12 stdio 细节

stdout 只出协议消息（诊断走 stderr）；`--readonly` 用
`mode=ro` 打开**不跑迁移**、只注册读工具；`--operator`
署名；文档给绝对路径配置示例。

## 三、复用与影响评估

- `ai_ledger.go` 逐项核对：agent 专用删，MCP 需要的查询
  挪进对应 repo 或由 2.8 新增 service 方法替代。
- Settings 模型卡 → 「MCP 服务」卡：开关、端口、token
  复制/重新生成、MCP 操作人、两种接入的连接说明。
- 删绑定后重生成 wailsjs；mockApp 同步删 AI 假数据。

## 四、实施顺序

1. **批 1 删 AI**：前端 → 绑定 → service → CLI → aiprovider/
   domain/ai → sqlite ai_* → 重生成 wailsjs，每步编译+测试。
2. **批 2 核心**：占用协议与文件锁基建（2.4/2.7）→
   `WithCaller`/`CallWarnings`/`audit.SourceMCP` →
   `voucher.SourceMCP` + 更新保留来源 → 乐观锁（含 GUI
   接口改造）→ `mcp_idempotency` → Patch 三态与 tx 内方法
   → `VoucherQuery` 扩展与新增 service 查询 → `mcpserver`
   工具集 → `mcp` stdio 子命令 + Go 测试。
3. **批 3 HTTP**：`BookProvider`/generation/持锁语义 →
   四层访问控制 → `MCP-Session-Id` 标准会话 →
   `mcp:changed` 事件与前端订阅 → Settings 卡。
4. 全量回归 + wails build + 手工 MCP 冒烟。

## 五、验收测试（逐项对应评审风险）

| 风险 | 验收 |
|---|---|
| 切账隔离 | 会话中切账 → 旧 SSE 流关闭、持旧 Session ID 的请求 404，客户端重连；在途调用完成后切换才生效；SSE 不持账套锁 |
| 跨进程恢复 | 符号链接打开 + 真实路径恢复 → 恢复被拒；stdio 持续读写时恢复被拒；旧连接关闭释放占用后恢复成功，重连读到恢复后数据 |
| 审计跨进程 | 确定性交错（A 轮转后 B 旧缓存追加）链不断、seq 唯一；轮转后首条插入前故障→重开追加，空片经 `audit_segment` 恢复起点正确；Windows 锁路径覆盖 |
| 并发覆盖 | GUI/MCP 同改一张草稿，后提交 `conflict`；GUI 陈旧窗口同测 |
| 幂等 | 同键同内容重试返回原凭证（未修改 `modifiedSince:false`、修改后 `true`，基准为 `initial_voucher_updated_at`）；同键异内容 `idempotency_conflict`；删后重试 `idempotent_target_deleted`；并发同键只建一张；首次审计失败→丢响应→重启→同键重试返回持久化 `failed` 警告 |
| 来源保留 | GUI 重存 MCP 草稿仍 `mcp`；MCP 改银行生成草稿保留 `bank`；`created_by_ai` 同理 |
| 档案字段 | `{id,name}` 不清其他字段；`parentId:null` 置顶层；传 baseSalary/enabled 被拒 |
| 审计完整 | MCP 操作留痕 source=外部接口 + 正确 operator；`DeleteVoucher` 记真实删除人；日志写失败回 `auditWarning`；幂等重试警告不丢 |
| 访问控制 | 无/错 token、异常 Origin/Host 一律拒；重生成后旧凭据即失效 |
| 凭证红线 | 草稿落地、checkAux/平衡/期间校验全生效、source 标 MCP |

## 已确认的决策

Settings 模型卡 → MCP 服务卡；写工具全开；HTTP 本次实施。

---

## 批 1 实施记录（AI 模块删除）

**范围**：AI 助手/供应商/提示词/会计 agent/AI 建议全链路与测试全部移除；
**保留**：`voucher.source='ai'`、`created_by_ai`、`audit.source='ai'`、
`bank_flow.match_layer='ai'`（历史行可显示）、`ai_*` 表结构与迁移、
备份的 API Key 剥离逻辑、`DeleteDraft` 对 `ai_suggestion` 的 unlink
（`UnlinkedAISuggest`）。

**顺手切除的隐藏 AI 件**（文档清单外、按规约「永远不会成立的分支就删」）：

- `bank.Matcher` 第 3 层 `AISuggester` 接口与 `AI` 字段 —— 实现者
  （aiprovider）已删，生产链路本就不接线；`LayerAI` 常量保留供历史行显示。
- `domain/ai`（提示词配置 + 建议提案）、`internal/ai/aiprovider` 整包、
  `sqlite/ai_repo.go`、`sqlite/ai_ledger.go` 中 AI 专属部分；
  `AIRepo.LedgerContext` 的共享职责由 `VoucherRepo.LedgerContext`
  （非 tx 版）承接，`validationContext` 事务版不变。
- Service：`ai_wire.go`、`accountant.go`、AI 配置/建议/提示词方法与
  `AuditDocBrief`/`TaxReturnBrief`/`EvidenceBrief` 三个仅服务 AI 的适配器；
  `Service.AI()` 访问器与 `AIConfigInfo`。
- Desktop：`accountant.go`、全部 AI 绑定（AISuggest/AIConfig/
  SaveAIProvider/AISuggestions/Accept/RejectAISuggestion/
  AIPromptConfig/Save/Reset/PreviewAIPrompt/PreviewAccountantPrompt/
  AccountantSend/Session/Reset）。
- CLI：`ai`/`ai-config` 命令及帮助中的 AI 段。
- 前端：`AIAssistant.vue`、`/ai` 路由（导航自动消失）、api.js 的
  AI 调用封装与 mock 块、Settings 的「AI 记账助手」卡（模型服务商
  表单 + AI 表现统计，含 providerForm/aiCfg/stats 等全部 JS）、
  gui.test.mjs 的 5 条 AI 用例。
- wailsjs 已 `wails generate module` 重生成。

**验证**：`go build ./...` + `go vet` 干净；`go test ./...` 全绿
（desktop 105s 全过）；前端 `npm run test` 18/18；`vite build` 通过；
GUI 套件 39 项全过（AI 相关 5 条已删，路由数断言从 ≥15 维持成立）。

---

## 批 2 实施记录（核心基础设施 + MCP stdio）

按 §2.4–§2.9 实施，顺序与计划一致。

**B2.1 `internal/filelock`**：文件身份（Unix dev+inode / Windows 卷序列+
文件索引）→ `~/.mini-account/locks/<identity-hash>.lock`；`TryLock`/
`TryLockExclusive` 非阻塞；锁文件持久不删；无稳定身份的平台拒绝打开
而非退回路径锁。`LockFile` 另提供目录级锁（审计目录用）。

**B2.2 账套占用**：`service.Open` 共享占用、`service.Restore` 独占占用；
GUI `RestoreBackup` 改走 `service.Restore`——独占语义在恢复操作两端
（GUI/CLI/MCP）一致生效。

**B2.3 审计追加临界区**：`auditdb.Append` = 目录文件锁 → 重扫分片与
链状态 → 追加。空分片也会写元数据行，否则两个进程同时建分片会产出
两条 genesis。测试 `TestAppendInterleavedAcrossStores` 用两个独立
Store（模拟两进程）确定性交错验证链连续。

**B2.4 调用方身份**：`WithCaller`/`CallWarnings`/`audit.SourceMCP`/
`voucher.SourceMCP`；`recordAudit` 返回 error 并写 sink；
`SaveEmployee`/`DeleteVoucher` 审计缺口补齐；`SaveVoucher` 保留原
来源（MCP 改一张银行流水生成的凭证，来源仍是 bank）。

**B2.5 凭证乐观锁**：`expectedUpdatedAt` 贯穿
VoucherEditor→api.js→SaveVoucher/DeleteVoucher→`repo UPDATE/DELETE
... WHERE updated_at=?`；零行命中 → `ErrVoucherConflict` → GUI
`FaultBlocked`；`VoucherDetail.UpdatedAt` 暴露原文戳（RFC3339Nano
存的是原文，重格式化会丢尾零，回传必须原文）。

**B2.6 幂等**：迁移 `0017_mcp_idempotency`（request_id PK、请求 hash、
voucher_id、initial_updated_at、audit_state）；
`CreateDraftVoucherIdempotent`——键与凭证同账套事务落库，提交后写
审计并按成败落 `audit_state`（recorded/failed/pending），重试凭持久化
状态回答；同键不同内容 → `idempotency_conflict`；目标已删 →
`idempotent_target_deleted`；被改过 → `modifiedSince`；并发同键由
主键裁决只建一张。

**B2.7 三态补丁**：`Field[T]{Set,Null,Value}`；抽出 `saveContactTx`/
`saveDepartmentTx`/`UpsertEmployeeTx`/`EmployeeByIDTx`，
Patch = WithTx{读旧→合并→校验→写}，审计提交后记录。员工补丁
白名单仅 name/phone/idCard/bankName/bankAccount/expenseAccountCode/
companyAccountCode/remark——薪酬基数/部门/离职/启用各有业务动作。

**B2.8 查询下沉**：`VoucherQuery` +`AccountPrefix`/`Offset`；
`VouchersPage`（{items,total,limit,offset,hasMore}，limit≤500）；
`LedgerDetail` 从 desktop 下沉到 service（GUI 改别名委托，消除两份
实现）；`TrialBalance`/`BalanceSheet`/`IncomeStatement` 服务接口；
`InvalidInput` 错误类型让绑定层映射 `FaultInvalid`、MCP 映射
`invalid_params`。

**B2.9 `internal/mcpserver`**：go-sdk **v1.7.0**（2026-07-27 发布，
钉版满足 ≥7 天）。14 读 + 6 写 + 3 资源，无 prompts。金额全部元
字符串出入（`money.Parse` / `Money.String`）；写工具注入
`WithCaller{Source:mcp}` 并把 `CallWarnings` 映射为响应里的
`auditWarnings`；业务错误 → `IsError` + 错误名前缀
（invalid_params/idempotency_conflict/idempotent_target_deleted/
conflict/blocked/not_found/readonly/internal），协议层 error 只留给
调用形态错误。档案三工具用 `map[string]any` 输入以保三态。

**B2.10 stdio**：`miniaccount mcp --db <path> [--readonly]
[--operator <name>]`；`--db` 必须已存在已初始化，不静默建库；
stdout 只跑 JSON-RPC，日志一律 stderr。

**冒烟与测试**：真实 stdio 端到端验证——initialize/tools/list
（恰 26 工具）/resources（3 个）/create_draft_voucher
（source=mcp、audit_state=recorded）/同键重试（idempotent）/
同键异内容（idempotency_conflict）/错戳删除（conflict）/对戳删除
（deleted）。Go 测试：`mcpserver_test`（工具面+元字符串契约+只读）、
`mcpidem_test`、`patch_test`（三态+白名单）、乐观锁冲突、
filelock、auditdb 交错追加。

**已知边界**：并发同键只建一张由主键裁决，但**单连接池**下没有真正的
并发模拟测试；MCP 修改保留凭证原来源（bank 等），来源字段本身
不可由工具改。

**待批 3**：HTTP 传输（token/Origin/Host 校验、会话世代、SSE）、
GUI 设置页 MCP 卡、端到端 Wails 构建验证。

### 批 2 自查修复（复审后追补）

复审批 2 发现的问题与本批复修：

1. **元字符串契约违约（严重）**：`mustJSON` 直接序列化 service 结构，
   `money.Money` 无 MarshalJSON → 输出分整数。修法：`yuanize` 变换——
   JSON 往返后递归遍历，**只改数字值**且仅当键名在金额白名单内
   （白名单只含 6 个涉金额工具的真实输出键；`total`/`count`/`level`/
   `taxrateppm`/`confidence` 等非金额数字键刻意不入集）。
2. **乐观锁可空串绕过**：`update_draft_voucher`/`delete_draft_voucher`
   的 `expectedUpdatedAt` 空串在 repo 层等于「不比对」。现工具层
   强制非空。
3. **patch 类型混淆静默清零**：`patchStr/patchBool/patchInt` 对错误
   JSON 类型不再当零值处理，聚合后报 `invalid_params`。
4. **list_* 缺分页封套**：统一 `{items,total,limit,offset,hasMore}`，
   适配层对完整结果集切片（档案列表规模小，服务端本就全量物化）。
5. **CLI 帮助补 `mcp`**。
6. **已知遗留（未修，协议级）**：新建账套路径锁→身份锁的换锁间隙，
   以及 Restore「stat 时不存在、取锁时已存在」的同类竞态——两个
   不同锁身份无法互斥。需要把「路径锁持有期间就完成文件落位+换锁
   为原子协议」才能根除，列入批 3 讨论项。

### 批 3 实施记录（HTTP 传输 + GUI 卡 + 锁闸门补齐）

**锁闸门（批 2 遗留协议竞态，先修）**：`filelock.acquire` 现在对所有
调用方（Open 共享 / Restore 独占）统一先取**路径锁 `p-`**，再在
其保护下解析文件身份、取身份锁 `i-`，最后才放 `p-` —— 「建文件→
拿身份锁」全程在共享闸门内完成，另一进程想用身份锁抢跑必须先过
同一个 `p-` 文件，窗口关闭。锁顺序固定 `p → i`。确定性回归：
`TestGateBlocksAcrossIdentities`（不同身份不能绕过共享闸门）、
`TestSharedGateStillBlocksExclusive`（共享闸门挡住独占恢复）。

**架构调整**：`mcpserver.Server` 的 `svc *service.Service` 字段换成
`bookSource` 接口（`call(ctx, fn)`）；stdio 用 `directSource` 直通，
HTTP 用 `providerSource` → `mcpserver.BookProvider`（desktop.App
实现：`Call` = `App.mu.RLock` 覆盖一次业务调用，`BookID` = 当前
账套路径，`Generation` = `bookGen` 原子计数）。所有 20 个工具与
3 个资源改经 `s.run`/`s.src.call` 统一入口，写工具成功后触发
`OnChange` 回调。

**世代推进点**：`desktop.App` 新增 `bookGen atomic.Int64`，所有
`a.svc` 交换点改走 `setBookLocked`（OpenBook / CloseBook /
shutdown / RestoreBackup 三处换账点）—— 每次换账世代加一。

**HTTP 层（internal/mcpserver/http.go）**：
- 四层拦截顺序 Host → Origin → Bearer → 会话世代，全部在 SDK
  streamable handler 之前的中间件完成。
- Host：由监听地址推导精确白名单 `{127.0.0.1:<port>,
  localhost:<port>}`，其余 403。
- Origin：无头放行（非浏览器客户端）；有头解析 hostname 只放行
  `localhost`/`127.0.0.1`。
- Token：`Authorization: Bearer <tok>` 常数时间比对；凭据存
  `~/.mini-account/mcp.json`（ConfigStore：临时文件 + rename +
  0600），`SetToken` 原子格即时换值 —— 重新生成不重启服务。
- 会话世代：`sessionTable` 记录 `MCP-Session-Id → {bookID,
  generation}`；initialize 响应头里的会话 ID 在 ServeHTTP 返回后
  从响应头读出钉入（SSE 路径可能绕过 WriteHeader，故不包装
  writer）。带会话 ID 的请求先 `check`：世代/账套不符或未知 →
  移出注册表 + HTTP 404。SDK 内部孤儿会话由
  `SessionTimeout: 30min` 清扫。
- **SSE 说明**：本服务不主动向流推事件，过期 GET 流为空转；
  下一次带旧会话 ID 的请求在世代层 404 后客户端按规范重建。

**desktop 绑定**：`MCPStatus` / `MCPStart`（幂等：先停再开；
无凭据则签发）/ `MCPStop`（关闭即吊销：mcp.json 的 token 清空，
下次开启重签）/ `MCPRegenerateToken`（新值立即生效于运行中的
服务）。`OnChange` → `wails EventsEmit("mcp:changed",
{entity,id})`。

**前端**：Settings 新增「MCP 服务」卡（开关、端口、操作人、
凭据显示/复制/重新生成、接入说明）；api.js 加 4 个绑定 +
`onMcpChanged` 订阅（`window.runtime.EventsOn`）；Vouchers 页
订阅 `mcp:changed`：编辑器/弹窗未开自动重查，编辑中只弹
「数据已被外部修改」不覆盖表单；wailsjs 手工按生成格式补齐
（wails CLI 不在 PATH）。

**stdio 对齐 §2.12**：`--readonly` 现在 `service.Open{ReadOnly:
true}`（mode=ro 不跑迁移）且**只注册读工具**（写工具不在工具
表里，调用即协议层 tool-not-found）；`TestReadonlyMode` 改判
工具缺席。

**测试**：`http_test.go` 覆盖 Host/Origin/Token 四层、会话世代
失效 404、重生成即时生效、mcp.json 0600 与坏文件报错；
`TestHTTPSessionGeneration` 走真实 initialize → 会话 → 世代
递增 → 404 的完整链路。

**已知边界**：SDK 会话表是内部的，世代失效无法主动 kill 已建
SSE 流（上面 SSE 说明即后果）；`yuanize` 键名白名单仍待显式
DTO 化。

## 附：客户端接入配置示例

### stdio（Claude Desktop 等本地客户端）

`command` 与 `--db` 都必须用**绝对路径**（客户端不继承
shell 的 PATH/工作目录）：

```json
// ~/Library/Application Support/Claude/claude_desktop_config.json
{
  "mcpServers": {
    "miniaccount": {
      "command": "/usr/local/bin/miniaccount",
      "args": [
        "mcp",
        "--db", "/Users/<你>/账本/公司.db",
        "--operator", "Claude"
      ]
    }
  }
}
```

- 加 `"--readonly"` 进 `args` 即只读模式（`mode=ro` 打开、
  不跑迁移、工具表只有 14 个读工具）。
- `--db` 指向的必须是已初始化账套（`PeekBook` 校验；
  不存在/非账套文件直接报错退出，不会顺手建库）。
- stdout 只跑 JSON-RPC 协议帧；所有诊断走 stderr。

### HTTP（桌面应用开启 MCP 服务后）

在「设置 → MCP 服务」卡里启动后，对支持 HTTP/SSE 传输的
客户端填：

```
URL:  http://127.0.0.1:<端口>/mcp
头:   Authorization: Bearer <卡里显示的凭据>
```

凭据持久化在 `~/.mini-account/mcp.json`（0600）；服务停止
即吊销，重开重新签发——卡片里的「重新生成」可随时轮换。

**注意**：App 重启后 MCP 服务**不会自动重开**，需再到
设置卡开启（已知边界，非缺陷）。

### 复查修复记录（外部验收前实测发现的 9 项）

**锁与占用（3 项）**

- **写者锁**：读写打开在共享占用之外再占一把 `w-<身份>`
  独占锁 —— 硬链接/符号链接别名归并同一文件身份，第二个
  读写进程被拒「正被读写」；只读打开不取写者锁，可与一个
  读写进程共存（WAL 多读单写安全）。`Open` 的
  `CheckSchemaVersion`/`Migrate` 失败路径此前泄漏占用锁，
  现统一 `release()`。
- **恢复闸门全程持有**：`acquire` 独占获取改为「身份锁 +
  路径闸门」复合锁直到 Release —— 恢复替换文件换 inode
  期间，身份锁只锁旧 inode，闸门不放手让同路径 opener
  整段被拒；释放顺序 i→p。共享获取维持「过闸门即放」。
- **会话世代锁内复核**：`providerSource.call` 在
  BookProvider 读锁临界区内再比一次——中间件检查与取锁
  之间的换账窗口被堵死，旧会话写不进新账套（报
  `session_stale`，下次请求 404）。**通路不是请求
  ctx**：SDK 把 POST 推进会话 chan、在会话 goroutine 里
  派发 handler，请求 ctx 的键值到不了那里；正确通路是
  handler 的 `req.Session.ID()`（streamable conn 的
  SessionID 即 `Mcp-Session-Id` 头值），`run` 把它放进
  调用 ctx，`providerSource` 按它回查注册表拿钉住的世代。
  另一被否掉的方案是把整个 `inner.ServeHTTP` 包进
  `Provider.Call`：handler 会再进 `Call` 嵌套 RLock，
  写者等待时 Go `RWMutex` 挡新读者 → 三方死锁。
  端到端回归：`TestHTTPSessionIDReachesCall`。

**停止与凭据（1 项）**

- `MCPStop` 改「先吊销、后关停」：先持久化清空
  `mcp.json` 的 token，再给运行中实例 `SetToken` 随机值，
  最后 `Shutdown`；SSE 排空超时改为兜底 `srv.Close()`。
  旧顺序是先关停后吊销，超时让旧凭据留在文件里复活。

**查询与写入（4 项）**

- `query_vouchers` 的 `amount` 改由 `lineStats` 一次
  `SUM(debit)` 聚合（凭证头不载分录，`v.Amount()` 恒 0）。
- `update_draft_voucher` 回填原 `word` —— 留空会被服务层
  默认成「记」，只改摘要会把「付」字改掉。
- `get_ledger` 的 `closingBalance` 跑完整个日期区间，不再
  停在 `limit` 截断位置；`Truncated` 语义不变。
- `list_invoices` 的 `direction` 公布值修正为
  `input|output`，`in`/`out` 作缩写映射，其余值显式
  `invalid_params`（此前静默不匹配返回空表）。

**审计（1 项）**

- `auditdb.Append` 的目录锁竞争改 2 秒退避重试（25ms 步进）
  —— 临界区只有几条 SQL，短暂占用就失败会把业务操作永久
  记成 `failed`，幂等重试还回放这个结果。

### 二次复查修复记录（自查 + 复核发现）

**世代复核通路纠错（承上）**：初版把钉住世代放**请求 ctx**
——复核永不触发，形同空转；改为 `req.Session.ID()` 回查
注册表，在 `Call` 读锁内比对（见上）。配套：注册表 `check`
命中时刷新 `at`，否则活跃超过 `sessionIdle` 的会话会被
`put` 清扫误删，之后锁内复核永远拿不到钉值。

**审计与来源（3 项）**

- `run()` 注入 `Caller{Operator, Source: SourceMCP}`——此前
  读侧（查看/报表/资源）的审计全被记成「界面 + OS 用户名」，
  与 §2.6 外部来源口径不符；写工具自带 sink 的 caller 不覆盖。
- `resolveIdem` 命中 `failed` 态时**补记日志**：审计内容
  可由已落库凭证确定性重建，重试修复后状态写回
  `recorded`（此前永久回放旧 failed，锁释放也修不好）。
  `pending` 不补——审计可能已成功落库，再写是无法分辨的
  重复记录，如实上报比静默重复好。
- `VouchersPage` 补 `ActionViewVouchers` 查看记录，
  与 GUI `Vouchers` 同口径。

**入参严格化（防静默错账）**

- 枚举入参统一过 `enumIn`，非法值 `invalid_params` 而非
  静默空表：`status`（凭证/发票/流水）、`kind`（往来单位）、
  `direction`（发票，in|out 缩写到 input|output）。
  `list_bank_flows` 公布值改为域值 `imported|matched|
  posted|ignored`（unmatched/booked 作别名）。
- map 输入三工具（upsert_contact/save_department/
  update_employee）：白名单外键拒绝、`id` 必须正整数。
  **浮点 id 截断修复**：`{"id":5.9}` 此前 `int64(f)` 截断
  成 5，静默改到别的记录——现非整数/非数字/非正值一律
  `invalid_params`；`patchInt`（parentId）同样拒绝小数。
- `upsert_contact` 的 `kind` 过 `contactKinds` 枚举——
  saveContactTx 只验名称非空，非法值会落成脏数据。
- `create_draft_voucher` 补 `word` 入参（记|收|付|转，
  缺省记）——不带时 MCP 建的凭证全是「记」，付款凭证
  凭证字就写错了；`voucher.ErrBadWord` 映射为
  `invalid_params`。服务层 `buildVoucher` 补拒负
  `attachCount`。
- struct 输入的未知键由 SDK 层兜底（jsonschema-go 对
  struct 生成 `additionalProperties:false`），无需自验。

**打开与锁（2 项）**

- `pathOnly` 换真实身份写者锁失败必须报错，不能降级保留
  `w-p` 占位——经别名路径并发建账时两边各持不同 `w-` 键，
  「保留占位」等于放行双写（`occ` 路径锁降级保留安全，
  双方都过同一把 `p-` 闸门）。
- `sessionTable` 有界化：`put` 时清扫 `sessionIdle`（1h）
  未活跃项，`check` 命中刷新 `at`；上限 256+清扫窗口。

**HTTP 细节（1 项）**

- `injectMeta` 注入 `_meta` 后 `Content-Length` 过期——
  `bufferWriter` flush 前 `Header().Del("Content-Length")`，
  SSE 与裸 JSON 两种形态都覆盖。

**来源保留死代码（1 项，自查复现）**

- `SaveVoucher` 更新路径：repo 的「`v.Source==""` 才保留
  旧来源」判断是**死代码**——`voucher.New` 恒置
  `SourceManual`，`v.Source` 永不为空，任何不带 Source 的
  更新都把 `bank/invoice/salary/mcp` 来源静默刷成
  `manual`（GUI 与 MCP 同病，实测复现）。修复：
  `buildVoucher` 对 `in.ID!=0 && in.Source==""` 显式置空
  （约定「空=沿用旧值」），repo 保留条件去掉
  `oldSource!=""` 限制——旧值是什么就保留什么，
  不凭空编造 manual。回归：
  `TestUpdatePreservesSource`（bank 来源凭证改摘要后
  仍是 bank）。

### 三轮复查修复记录（验收实测：并发/文件别名/真实进程）

**世代复核死锁（2 个，其一为新引入回归）**

- `providerSource.call` 在 `provider.Call` 的 `a.mu.RLock`
  临界区内调 `BookID()`——`BookID` 自己也取 `a.mu.RLock`，
  嵌套读锁遇写者排队即死锁（请求与 CloseBook 互卡）。
- 同一调用链还藏着**逆序锁**：`providerSource.call` 是
  `a.mu→sessionTable.mu`，而中间件 `check` 持
  `sessionTable.mu` 时调 `BookID()` 又是 `t.mu→a.mu`。
- 根治：`BookProvider` 改单次原子 `BookSnapshot()`
  （{bookID,generation} 一并返回不撕裂）；desktop 用
  `atomic.Value` 存快照、`setBookLocked` 写锁内刷新——
  BookID/Generation 不再经任何锁，两种死锁同消。

**文件别名（2 项）**

- `PathIdentity` 只解析父目录：恢复持锁期间，「指向目标
  路径的文件符号链接」算出不同 `p-` 键，绕过闸门去开刚被
  替换的新文件。修法：`CanonicalPath` 对存在文件做**全路径**
  `EvalSymlinks`（连末段文件链接一起解析），不存在则解析
  最深存在祖先接回文件名；闸门/锁身份/SQLite 打开路径一律
  用规约结果。
- 硬链接别名的只读打开静默读旧数据：SQLite 的 WAL 按
  **打开路径字符串**命名（`<path>-wal`），经别名打开用
  另一份（不存在的）WAL——写进程 book.db-wal 里的草稿，
  别名只读端读到 0 条且无报错。修法：`<身份>.owner` 记录
  首个持锁进程的「pid+规约路径」（`<身份>.claim` 短锁串行
  读写；顺序固定 gate→i→claim）；后开者规约路径不同→换用
  登记路径打开，看到同一份 WAL。陈记录按 pid 活性 +
  「路径仍指向同一身份」双重校验回收。
- `service.Open` 全程用归并后 `openPath`（SQLite 打开、
  `s.path`、写者锁）；`OpenBook` 重开同一文件（含别名）
  先关旧连接再开新——否则被自己的写者锁拒。

**幂等补记失真与双写（2 项）**

- failed 态补记曾从「当前凭证」重建审计明细：凭证落库后被
  改过，补记把 900 元修改记成 100 元创建——日志失真。
  修法：迁移 0018 加 `audit_payload`，审计明细 JSON 与凭证
  同事务落库，补记只回放原文。
- 并发同键重试都进 failed 分支各补一条 → 一条创建日志变
  多条。修法：`repair_claim_at` 认领时间戳 + CAS
  （failed→repairing，或过期 repairing 重领）；认领窗口
  内的 repairing 视为「别人在补」如实上报。
- `shutdownMCP` 排空超时强制 `Close()` 后不再上报
  DeadlineExceeded——服务已死，报错会让设置页跳过刷新，
  把已停止显示成运行中。

**JSON 精度（1 项）**

- map 工具的 `id`/`parentId` 先被 SDK 解成 float64：
  `{"id":9007199254740993}` 静默取整成 ...992，补丁打到
  另一条记录。修法：`decodeMapArgs` 用 `UseNumber` 重解
  `req.Params.Arguments`，`idFromJSON`/`patchInt` 精确解析
  `json.Number`。

### 四轮复查修复记录（验收实测：边界与半完成态）

验收复核三轮修复：关账死锁、SSE 停止不刷新、同实例重开
已通过；以下 5 项边界仍可复现，逐项修复。

**符号链接恢复锁（1 项）**

- 恢复暂存原库后目标短暂不存在，指向它的符号链接变成
  悬空链接：旧 `CanonicalPath` 只回溯到「存在的祖先」，
  悬空末段按链接名算出另一把 p- 闸门，新建占位把库文件
  写穿链接落在恢复目标上。修法：`CanonicalPath` 增加
  「EvalSymlinks 失败 → Lstat 全路径 → 末段是符号链接就
  Readlink 跳目标再重试」循环（上界 40 防环），再回溯
  祖先。`service.Restore` 把目标路径先规约再锁再替换——
  直接往符号链接上恢复，`os.Rename` 会把链接本身换成
  独立文件、原账套原地不动，命令报成功实际一字没恢复。

**owner 表继承（1 项）**

- 单行 owner 记录绑定首个 PID：登记进程退出、写进程继续
  运行时，新开的硬链接只读端失去归并路径，又读到 0 行。
  修法：`.owner` 升级为多行表「pid → 归并后路径」，每个
  持锁进程登记自己的行（登记的是归并后的实际打开路径，
  继承链不断）；取首个活行，`readOwnerTable` 按 pid 活性
  + 「记录路径仍指向同一身份」双重校验过滤陈行。

**幂等补记半完成态（1 项）**

- 「审计 Append 成功、`audit_state` 更新失败」留下
  已落库日志 + repairing 状态；认领过期后重试又写一条
  创建日志。修法：CAS 认领后先 `hasCreateAudit` 查重——
  按 entity+Book+`clientRequestId`（detail_json 里）找
  已有创建日志，命中只把状态修回 recorded，不再追加。
  查重用 `QueryAudit` 的 Text 过滤（detail_json 已进
  检索列）+ 逐条精确比对。

**大整数输出（1 项）**

- 输入侧 `UseNumber` 已精确，但 `mustJSON` 输出侧普通解码
  把 int64 变 float64：`list_contacts` 把 ...992 和 ...993
  都序列化成 ...992，客户端照抄列表 id 改错记录。修法：
  `mustJSON` 改 `json.Decoder.UseNumber`，`yuanWalk` 对
  `json.Number` 走 `Int64()` 转换；分页/报表非金额数字
  仍按数字输出，金额键仍转元字符串。

**`:memory:` 回归（1 项）**

- 路径归约把 SQLite 内存库标记变成当前目录绝对路径，
  磁盘上落了名为 `:memory:` 的文件。修法：`service.Open`
  在规约前先判 `:memory:` ——不规约、不取文件锁、
  `openPath`/`s.path` 原样保留。

## 附件能力（五轮开发）

GUI 早已有完整的附件生命周期（上传/列表/删除/孤儿报告/覆盖
率统计），MCP 一个都没暴露——外部自动化「建了凭证但挂不上
发票扫描件，也读不回来」。本节补齐，仍走服务层同一入口。

### 功能目标

- 上传：给 voucher | invoice | expense_claim | expense_item
  四类单据挂附件（base64 内容，不走宿主机路径——远程 HTTP
  客户端根本碰不到服务端的文件系统）。
- 读取：按 hash 取回附件内容（base64）与元数据。
- 列表：某单据挂了哪些附件（含「原件已丢失」标记）。
- 辅助：孤儿附件报告、凭证附件覆盖率统计。
- 删除：解除单据与附件的关联（只删关系行，物理文件留给
  孤儿清理——内容寻址下同一份文件可能被多单据共享）。

### 技术方案与边界

**接口**（工具名沿用动词_名词惯例）：

| 工具 | 读/写 | 说明 |
|------|-------|------|
| `list_attachments` | 读 | ownerType+ownerId → {hash,name,size,mime,missing}[] |
| `read_attachment` | 读 | hash → {hash,size,mime,dataBase64} |
| `list_orphan_attachments` | 读 | → {hash,size}[] |
| `attachment_coverage` | 读 | year?/month? → {posted,withAttach,without} |
| `upload_attachment` | 写 | ownerType+ownerId+fileName+dataBase64 → {hash,name,size,mime} |
| `delete_attachment` | 写 | ownerType+ownerId+hash → 解除关联 |

**路径绝不外泄**：`AttachmentInfo.Path` 是宿主机绝对路径——
对远程客户端没用、对信息泄漏有用。MCP 输出层按字段重建，
不带 path。GUI 的 OpenAttachment/AttachmentPath（系统程序
打开）属于「本机交互」，MCP 不提供——外部客户端要的是
内容本身，不是替服务端开窗口。

**owner 校验**：ownerType 白名单枚举（与表 CHECK 一致）；
upload 前按类型查对应表确认 owner 真的存在——往不存在的
id 上挂附件会留下「文件在、归属悬空」的孤儿。`AttachToVoucher`
泛化为 `AttachDocument(ownerType,…)`， voucher 路径委托之。

**幂等**：附件内容寻址（sha256），同文件重复上传 =
UNIQUE(owner,hash) + INSERT OR IGNORE → 天然幂等，不需要
clientRequestId。不同文件得到不同 hash，是两条合法附件。

**大小上限**：附件仓库 20MB/文件（已有约束）。MCP 层对
dataBase64 先做长度预检（base64 膨胀约 4/3，超 ~28MB 直接
invalid_params），避免解码出几百 MB 的内存申请。

**审计**：上传沿用 `attachment.upload`；`DeleteAttachment`
补上 `attachment.delete`（枚举早已存在，此前没人写——
删除与上传同样是凭证内容变更，缺了日志不对称）。ctx 里
的 MCP caller 经 recordAudit 自动落到 Source/Operator。

**视图审计**：读附件内容暂不记查看日志——GUI 用系统程序
打开附件同样不记，口径一致；若以后要求「外发留痕」再加
专门动作，不混在这次。

**证据链附件**：`audit_evidence` 的挂依据（AddEvidence
的 Hash/Data 分支）是另一条链路，本期不做——它需要连同
证据链管理（结论、引用种类）一起设计，不在附件范围内。

### 涉及模块

- `internal/mcpserver/tools.go`：四个读工具。
- `internal/mcpserver/writes.go`：upload/delete 两个写工具
  + base64 解码（兼容 data: URL 前缀，同 GUI）。
- `internal/service/voucher.go`：`AttachDocument` 泛化 +
  `AttachToVoucher` 委托 + `DeleteAttachment` 补删除审计。
- 输出契约：金额键规则不变；hash/size/mime/missing 按原样，
  path 不外发。

### 附件链路复查修复（验收实测 9 项）

**账套隔离与路径（P1，5 项）**

- `buildDSN` 按 `file:`+path+`?`+参数拼接：文件名含 `?` 被当
  参数分隔，MCP 打开的是另一份库。修法：路径段先转义
  `%`→`%25`、`?`→`%3F`、`#`→`%23`（SQLite URI 允许百分号
  编码），`file:` 开头的显式 DSN 不动。
- `FilesDir` 剥扩展名取 `<stem>.files`：`same.db` 与
  `same.sqlite` 共用 `same.files`，互相列孤儿、互相打进
  备份。修法：改为 `<完整文件名>.files`（`same.db.files` /
  `same.sqlite.files`），`migrateLegacyFilesDir` 扩展为遍历
  两个旧布局——共享 `<dir>/.files` 与 `<stem>.files`——
  只复制本账套引用的文件。
- `service.Restore` 规约了 DB 目标但 `FilesDir` 由调用方按
  原路径推导：恢复到符号链接 → DB 落真实路径、附件落链接
  名目录。修法：`o.FilesDir` 为空时由规约后的 dbPath 推导
  （`dbPath+".files"`）；`RestoreResult` 新增 `FilesDir`
  回传实际落点；CLI/GUI 改为留空由它推导。
- 备份只按「磁盘内容 hash」记清单：内容与文件名 hash 不符
  的坏附件原样打进包、恢复也接受。修法：`collectFiles` 对
  「文件名是 64 位 hex 的 sha256」强制 `内容hash==文件名`；
  恢复侧对每个解出的文件同样校验文件名 hash 与 manifest
  记录一致——两处都报 `ErrChecksumFail`，不让坏件过手。
- `migrateLegacyFilesDir` 只查 `attachment` 表，漏掉
  `audit_evidence` 引用的原件。修法：查询改
  `UNION audit_evidence`（与 OrphanAttachments 同口径）。

**边界与口径（P2，4 项）**

- HTTP `MaxRequestBodyBytes` 默认 4MiB：base64 上传 20MB
  文件需要 ~27MB。修法：显式设 `32<<20`，与工具声明的
  单文件上限一致。
- `AttachDocument` 先 `store.Put` 落盘再查 owner：归属不
  存在时文件已写成孤儿。修法：Put 前先做一次归属存在
  检查（事务内复查保留——挡住「检查后归属被删」的窄窗）。
- `AttachmentCoverage` 只查关联行：文件全丢的凭证仍被计
  「有附件」。修法：改为逐 hash `store.Exists` 判定——
  有任一文件实体在才算 withAttach。
- `SaveClaim` 覆盖草稿删明细不删旧 attachment 行（悬空、
  AUTOINCREMENT 复用 id 还会错挂到新明细），重新关联只
  写 hash 丢 file_name/mime/size/kind。修法：删明细前先
  快照旧附件行并按 hash 保留元数据，删旧行，重新关联时
  用 `linkTx` 的元数据版把名字/类型/大小/kind 带回来。

### 附件链路二次复查修复（验收实测 5 项）

- `vacuumInto`/`scrubSecrets` 同样裸拼 `file:`+path+`?`：
  带 `?` 的账套名让备份打开的是另一份库，快照内容来自
  错误的文件。修法：转义逻辑提为 `sqlite.EscapeURIPath`，
  `buildDSN` 与备份两处共用。
- 备份只遍历「磁盘上有的」，不核对「库里引用的」：原件
  丢失的账套生成的是一份「看起来完整」的备份。修法：
  `Create` 在含附件时按表存在性查 `attachment`/`audit_
  evidence` 的引用 hash（`referencedHashes`），任一缺失报
  `ErrMissingFile`；`Backup.FallbackFilesDirs` 让旧布局
  目录参与收集——但**只按引用 hash 定点取件**，不整目录
  遍历：共享 `.files` 是同目录多本账共用的池子，整收会把
  别家账套的附件打进这份归档，引用之外的坏件也不该搅黄
  备份（主目录同名 rel 优先，回退坏件不参与）。
- 只读打开跳过物理迁移 → 旧目录里的完好原件被报缺失；
  迁移按「先读到」选副本 → 坏副本盖过好原件。修法：
  `attachment.Options.FallbackDirs` 给只读路径兜读 —
  `Open/Get/Exists/Size` 经 `resolve` 依次查主目录与回退
  目录，`Get` 逐副本校验 hash 取首个有效；迁移选副本时
  同样按内容 hash 验证，首个通过验证的才复制。
- `AttachDocument` 落盘后 SQLite 关联失败仍留文件。
  修法：Put 前记录 hash 是否已存在，事务失败且该文件
  本来不在、又无任何引用时 `store.Delete` 清理。
- `SaveClaim` 元数据按「整单 hash」合并：两条明细共享同一
  文件但名字不同（line-1/line-2.txt）时后写覆盖先写。
  修法：快照键改 `(line_no, sha256)` 精确匹配 +
  `sha256` 兜底（行序变了也能找回元数据）。
- GUI 冒烟用例随新目录约定同步：`<完整名>.files`。

### 附件链路二次复查收尾（本轮内自查出 +4 项）

- 用户侧「打开原件/下载」路径与实际落点脱节：`MustPath`
  只回主目录规范位置，文件只在回退目录时界面拿到的是
  不存在路径。修法：`Store.ResolvePath` 返回首个真实存在
  的候选（主目录优先），证据链 `linkViews`、`ListAttachments`、
  桌面 `OpenAttachment`/`AttachmentPath` 全部改走它；
  写路径（`Put`/`AttachDocument`/孤儿列表）仍用 `MustPath`。
- `referencedHashes` 查表失败被当「表不存在」跳过：
  引用集静默清空会让缺失核对失效、坏备份照出。修法：
  `sqlite_master` 查询出错直接返回错误，只有 `one==0`
  （老账套真没这张表）才跳过。
- `migrateLegacyFilesDir` 同款吞错：查表失败静默跳过会
  整表漏迁。修法：与既有「清单读取失败」分支同口径 —
  不阻塞开账，但写 stderr 留痕。
- GUI 冒烟账套原放在 `$TMPDIR` 根：该目录的 `.files` 是
  历代测试共用的遗留池，内容寻址下同 hash 即同一份原件 —
  「删实体就报丢失」断言被陈旧副本搅黄（38/39）。修法：
  账套放进 `mkdtemp` 独立子目录，旧布局回退目录随账套
  目录一起隔离 —— 与生产环境「账套住在自己的目录里」同构。
