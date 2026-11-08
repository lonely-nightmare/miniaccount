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

**写（全部进操作日志）**

| 工具 | 说明 | 约束 |
|---|---|---|
| `create_draft_voucher` | 建凭证草稿 | `SaveVoucher` 全量校验；`source=mcp`；可选幂等键 |
| `update_draft_voucher` | 改草稿 | 仅 draft；`expectedUpdatedAt` 必填；来源字段保留 |
| `delete_draft_voucher` | 删草稿 | 仅 draft；`expectedUpdatedAt` 必填 |
| `upsert_contact` | 建/改往来单位 | 部分更新（2.9） |
| `save_department` | 建/改部门 | 部分更新（2.9） |
| `update_employee` | **仅修改**员工档案 | 部分更新 + 字段白名单（2.9） |

**不开放**：过账、结账/反结账、备份/恢复、附件、审计读写、
密钥配置、任意 SQL、文件系统。

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
（恰 20 工具）/resources（3 个）/create_draft_voucher
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
