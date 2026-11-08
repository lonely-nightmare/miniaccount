<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import {
  Plus, Trash2, Save, ShieldCheck, AlertTriangle, Info, Search,
  Paperclip, X, FileText, CheckCircle2,
} from 'lucide-vue-next'
import { api, notify } from '@/lib/api'
import { bookkeeper, loadBookkeeper, rememberBookkeeper } from '@/lib/operator'
import { fmtMoney, parseYuanToCents } from '@/lib/format'
import Modal from '@/components/ui/Modal.vue'
import Button from '@/components/ui/Button.vue'
import Badge from '@/components/ui/Badge.vue'
import Input from '@/components/ui/Input.vue'
import Label from '@/components/ui/Label.vue'
import Spinner from '@/components/ui/Spinner.vue'

const open = defineModel('open', { type: Boolean, default: false })
const props = defineProps({ voucherId: { type: Number, default: 0 } })
const emit = defineEmits(['saved'])

const meta = ref(null)
const loading = ref(false)
const busy = ref(false)
const checkResult = ref(null)
// 附件：草稿保存之后才能挂 —— 附件要挂在凭证 id 上，
// 而新建的凭证在保存前还没有 id。
const attachments = ref([])

// 空行工厂。至少留两行 —— 复式记账最少两条分录，
// 一打开就给一行会让人以为只填一行就行。
function emptyLine() {
  return {
    accountCode: '', summary: '', debitYuan: '', creditYuan: '',
    contactId: null, employeeId: null, deptId: null, projectId: null,
  }
}

// ★ 表单里**没有**记账人。
//
// 记账签章是在过账那一刻盖上去的，而本程序只在账期结算时过账
// （用账期管理里填的操作人）。录入阶段问「谁是记账人」，
// 问的是一件还没发生的事 —— 用户填了也没用，反而以为填完就记上账了。
const form = ref({
  id: 0, word: '记', date: '', remark: '',
  attachCount: 0, createdBy: '',
  lines: [emptyLine(), emptyLine()],
})

async function ensureMeta() {
  if (meta.value) return
  loading.value = true
  const r = await api.voucherMetaInfo()
  loading.value = false
  if (!r.ok) { notify(r.fault.message, 'error', r.fault.detail); return }
  meta.value = r.data
  if (!form.value.date) form.value.date = r.data.today
}

async function loadVoucher() {
  if (!props.voucherId) {
    form.value = {
      id: 0, word: '记',
      date: meta.value?.today ?? '',
      remark: '', attachCount: 0,
      // 制单人默认是本机记住的记账人（见 lib/operator.js）
      createdBy: bookkeeper.value,
      lines: [emptyLine(), emptyLine()],
    }
    checkResult.value = null
    return
  }
  const r = await api.voucherDetail(props.voucherId)
  if (!r.ok) { notify(r.fault.message, 'error', r.fault.detail); return }
  const d = r.data
  form.value = {
    id: d.id, word: d.word, date: d.date, remark: d.remark,
    attachCount: d.attachCount,
    // 乐观锁戳：载入时看到的 updatedAt，保存时原样回传 ——
    // 载入后被别的窗口改过的话后端报冲突，不静默覆盖。
    expectedUpdatedAt: d.updatedAt || '',
    createdBy: d.createdBy || '',
    lines: d.lines.map((l) => ({
      accountCode: l.accountCode, summary: l.summary,
      debitYuan: l.debit ? (l.debit / 100).toFixed(2) : '',
      creditYuan: l.credit ? (l.credit / 100).toFixed(2) : '',
      // ★ 辅助核算的实体 id 必须回填 —— 原来全部置 null，
      // 保存时四维被静默清空，而界面上往来单位名照常在
      // _auxDesc 里显示着，用户完全无感知。
      contactId: l.contactId ?? null, employeeId: l.employeeId ?? null,
      deptId: l.deptId ?? null, projectId: l.projectId ?? null,
      _auxDesc: l.auxDesc,
    })),
  }
  checkResult.value = null
}

watch(open, async (v) => {
  if (!v) return
  await ensureMeta()
  await loadVoucher()
  await loadAttachments()
})

onMounted(async () => {
  // 先拿到本机记账人，再建表单：不然 prefill 出来是空的
  await loadBookkeeper()
  if (open.value) { await ensureMeta(); await loadVoucher() }
})

// ---------------------------------------------------------------- 计算

const toCents = (s) => {
  const c = parseYuanToCents(s)
  return c === null ? 0 : c
}

const totals = computed(() => {
  let d = 0, c = 0
  for (const l of form.value.lines) {
    d += toCents(l.debitYuan)
    c += toCents(l.creditYuan)
  }
  return { debit: d, credit: c, diff: d - c, balanced: d === c && d > 0 }
})

// 非空行数：判断「至少两条分录」用的是它，而不是数组长度
const filledLines = computed(() =>
  form.value.lines.filter((l) =>
    l.accountCode || l.summary || toCents(l.debitYuan) || toCents(l.creditYuan)))

const canSubmit = computed(() => filledLines.value.length >= 2 && totals.value.balanced)

// ---------------------------------------------------------------- 科目选择

const activeLine = ref(-1)
const accountQuery = ref('')
const showAccountPicker = ref(false)

const filteredAccounts = computed(() => {
  const q = accountQuery.value.trim().toLowerCase()
  const all = meta.value?.accounts ?? []
  if (!q) return all.slice(0, 60)
  return all.filter((a) => a.searchText.includes(q)).slice(0, 60)
})

function openAccountPicker(idx) {
  activeLine.value = idx
  accountQuery.value = ''
  showAccountPicker.value = true
  nextTick(() => {
    document.getElementById('acct-search')?.focus()
  })
}

function pickAccount(a) {
  const l = form.value.lines[activeLine.value]
  l.accountCode = a.code
  l._auxDesc = ''
  // 换科目时清掉旧的辅助核算：旧科目的维度在新科目上可能不合法，
  // 留着会被服务端拒绝，而用户看不出为什么
  l.contactId = l.employeeId = l.deptId = l.projectId = null
  showAccountPicker.value = false
}

function accountOf(code) {
  return (meta.value?.accounts ?? []).find((a) => a.code === code)
}

// 当前行需要的辅助核算维度
function needAux(line) {
  const a = accountOf(line.accountCode)
  return a?.auxTypes ?? []
}
// 维度 key（customer|supplier|employee|dept|project）：选档案用它，
// 中文名只负责显示 —— 拿「客户」两个字反推不出该开哪个选择器。
function needKeys(line) {
  const a = accountOf(line.accountCode)
  return a?.auxKeys ?? []
}

const AUX_LABEL = {
  customer: '客户', supplier: '供应商', shareholder: '股东',
  other: '其他往来', employee: '员工', dept: '部门', project: '项目',
}
// 后端七维：customer/supplier/shareholder/other 都落到 contactId
const AUX_FIELD = {
  customer: 'contactId', supplier: 'contactId', shareholder: 'contactId',
  other: 'contactId', employee: 'employeeId', dept: 'deptId', project: 'projectId',
}
// 选择器里能放哪些 kind —— 与后端 auxMatchesContact 一一对应：
// customer 收 customer|both，supplier 收 supplier|both，
// shareholder 只收股东档案，other 不限类型（null = 不过滤）。
const CONTACT_KINDS = {
  customer: ['customer', 'both'],
  supplier: ['supplier', 'both'],
  shareholder: ['shareholder'],
  other: null,
}

// 一维一钮：每维独立选择，各自的档案互不串门
// （应收科目只能选客户，不该把供应商也列出来让人挑错）。
const showAuxPicker = ref(false)
const auxKey = ref('')
const auxQuery = ref('')
const auxPickerItems = computed(() => {
  const q = auxQuery.value.trim().toLowerCase()
  const hit = (s) => !q || (s ?? '').toLowerCase().includes(q)
  const key = auxKey.value
  let list = []
  if (key in CONTACT_KINDS) {
    const kinds = CONTACT_KINDS[key]
    list = (meta.value?.contacts ?? [])
      .filter((c) => kinds === null || kinds.includes(c.kind))
      .map((c) => ({ id: c.id, name: c.name, tag: c.kindLabel }))
  } else if (key === 'employee') {
    list = (meta.value?.employees ?? []).map((e) => ({ id: e.id, name: e.name, tag: e.kindLabel }))
  } else if (key === 'dept') {
    list = (meta.value?.departments ?? [])
      .filter((d) => d.enabled).map((d) => ({ id: d.id, name: d.fullName, tag: '部门' }))
  } else if (key === 'project') {
    list = []
  }
  return list.filter((x) => hit(x.name)).slice(0, 60)
})

function openAuxPicker(idx, key) {
  activeLine.value = idx
  auxKey.value = key
  auxQuery.value = ''
  showAuxPicker.value = true
}

function pickAux(item) {
  const l = form.value.lines[activeLine.value]
  const key = auxKey.value
  const field = AUX_FIELD[key]
  if (field) {
    l[field] = item.id
    // 各维各记一份名：档案若不在选择器列表里（如已停用），
    // 按 id 查不到时还能显示对的名字而不是别的维度的名。
    l._auxNames = { ...(l._auxNames || {}), [key]: item.name }
    l._auxDesc = item.name // 单维场景的后端描述回落
  }
  showAuxPicker.value = false
}

function clearAux(l, key) {
  const field = AUX_FIELD[key]
  if (field) l[field] = null
  if (l._auxNames) delete l._auxNames[key]
  l._auxDesc = ''
}

// 某一维已填值的显示名。
//
// ★ 各维各查各的档案：原来所有已填维度都返回同一个 _auxDesc，
// 先选部门再选客户，两个按钮会一起显示客户名。
// 按 id 实时查档案列表才是准的 —— 档案改了名也同步。
function lookupAuxName(key, id) {
  const m = meta.value
  if (!m) return null
  if (key in CONTACT_KINDS) {
    return (m.contacts ?? []).find((c) => c.id === id)?.name ?? null
  }
  if (key === 'employee') return (m.employees ?? []).find((e) => e.id === id)?.name ?? null
  if (key === 'dept') return (m.departments ?? []).find((d) => d.id === id)?.fullName ?? null
  return null
}

// 该行已填辅助维度的个数：_auxDesc 是载入草稿时后端拼的总描述，
// 只有一个维度已填时它才无歧义地属于那一维。
function filledAuxCount(l) {
  return ['contactId', 'employeeId', 'deptId', 'projectId']
    .filter((f) => l[f] !== null && l[f] !== undefined).length
}

function auxChipLabel(l, key) {
  const field = AUX_FIELD[key]
  const id = l[field]
  if (!id) return AUX_LABEL[key] ?? key
  return lookupAuxName(key, id)
    ?? l._auxNames?.[key]
    ?? (filledAuxCount(l) === 1 ? l._auxDesc : null)
    ?? `#${id}`
}
function auxFilled(l, key) {
  const field = AUX_FIELD[key]
  return l[field] !== null && l[field] !== undefined
}

// ---------------------------------------------------------------- 操作

function addLine() {
  form.value.lines.push(emptyLine())
}
function removeLine(i) {
  if (form.value.lines.length <= 2) {
    notify('复式记账至少需要两条分录', 'warn')
    return
  }
  form.value.lines.splice(i, 1)
}

// 一键把差额填到当前行 —— 这是录入时最常用的动作
function fillDiff(idx) {
  const l = form.value.lines[idx]
  const others = form.value.lines.reduce((s, x, i) => {
    if (i === idx) return s
    return s + toCents(x.debitYuan) - toCents(x.creditYuan)
  }, 0)
  if (others === 0) return
  const target = -others
  if (target > 0) { l.debitYuan = (target / 100).toFixed(2); l.creditYuan = '' }
  else { l.creditYuan = (-target / 100).toFixed(2); l.debitYuan = '' }
}

// ---------------------------------------------------------------- 附件

async function loadAttachments() {
  if (!form.value.id) { attachments.value = []; return }
  const r = await api.attachments({ ownerType: 'voucher', ownerId: form.value.id })
  attachments.value = r.ok ? (r.data ?? []) : []
}

function pickAttachment() {
  if (!form.value.id) {
    notify('请先保存草稿，再上传附件 —— 附件要挂在凭证上，新建的凭证还没有编号', 'warn')
    return
  }
  const el = document.createElement('input')
  el.type = 'file'
  el.multiple = true
  el.accept = '.pdf,.jpg,.jpeg,.png,.ofd,.xlsx,.docx'
  el.onchange = async () => {
    for (const file of el.files ?? []) {
      const buf = await file.arrayBuffer()
      const r = await api.uploadAttachment({
        voucherId: form.value.id,
        fileName: file.name,
        dataBase64: b64(buf),
      })
      if (!r.ok) { notify(`「${file.name}」上传失败：${r.fault.message}`, 'error', r.fault.detail); continue }
      notify(`已附上「${file.name}」`, 'success')
    }
    await loadAttachments()
  }
  el.click()
}

// 文件内容以 base64 传给 Go：WebView 里拿不到本地文件路径
// （浏览器的 File 对象出于安全不暴露路径），而账套可能在任何位置。
// 多占 33% 内存，但对一张几 MB 的发票 PDF 完全可以接受 ——
// 换来的是「选个文件就能存」的体验。
function b64(buf) {
  const bytes = new Uint8Array(buf)
  let bin = ''
  const chunk = 0x8000
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk))
  }
  return btoa(bin)
}

async function removeAttachment(a) {
  const r = await api.removeAttachment({
    ownerType: 'voucher', ownerId: form.value.id, hash: a.hash,
  })
  if (!r.ok) { notify(r.fault.message, 'error', r.fault.detail); return }
  notify('已移除附件（磁盘上的文件会保留，可在设置里清理孤儿附件）', 'success')
  await loadAttachments()
}

function fmtSize(n) {
  if (!n) return ''
  const u = ['B', 'KB', 'MB']
  let i = 0, v = n
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${u[i]}`
}

async function doCheck() {
  const r = await api.checkVoucher(payload())
  checkResult.value = r.ok ? r.data : { ok: false, message: r.fault.message }
}

function payload() {
  return {
    id: form.value.id,
    word: form.value.word,
    date: form.value.date,
    remark: form.value.remark,
    attachCount: Number(form.value.attachCount) || 0,
    expectedUpdatedAt: form.value.expectedUpdatedAt || '',
    createdBy: form.value.createdBy,
    lines: form.value.lines.map((l) => ({
      accountCode: l.accountCode,
      summary: l.summary,
      debitYuan: l.debitYuan,
      creditYuan: l.creditYuan,
      contactId: l.contactId,
      employeeId: l.employeeId,
      deptId: l.deptId,
      projectId: l.projectId,
    })),
  }
}

async function save() {
  if (filledLines.value.length < 2) { notify('复式记账至少需要两条分录', 'warn'); return }
  busy.value = true
  const r = await api.saveVoucher(payload())
  busy.value = false
  if (!r.ok) { notify(r.fault.message, 'error', r.fault.detail); return }
  // 记住这次用的制单人：下次默认就是他（空则不记）
  if (form.value.createdBy?.trim()) await rememberBookkeeper(form.value.createdBy.trim())
  // 新建的凭证此时才拿到 id —— 附件列表要跟着刷新。
  // 锁戳也要换成服务端写的新值：接着再改再存才不误判冲突。
  form.value.id = r.data.id
  form.value.expectedUpdatedAt = r.data.updatedAt || ''
  await loadAttachments()
  emit('saved', r.data)
}

</script>

<template>
  <Modal
    v-model:open="open"
    :title="form.id ? '编辑凭证' : '录入凭证'"
    description="借贷必须相等才能保存。差额不会被自动抹平 —— 那意味着记错了，应该由人来判断。草稿到账期结算时才过账。"
    width="max-w-5xl"
  >
    <Spinner v-if="loading" />
    <div v-else-if="meta" class="flex flex-col gap-4">
      <!-- 凭证头 -->
      <div class="flex flex-wrap items-end gap-3">
        <div class="w-24">
          <Label>凭证字</Label>
          <select
            v-model="form.word"
            class="mt-1.5 h-9 w-full rounded-md border border-input bg-transparent px-2 text-sm"
          >
            <option v-for="w in meta.words" :key="w" :value="w">{{ w }}</option>
          </select>
        </div>
        <div class="w-40">
          <Label>记账日期</Label>
          <Input v-model="form.date" class="mt-1.5" placeholder="2025-03-11" />
        </div>
        <div class="w-40">
          <Label>附单据数</Label>
          <Input v-model="form.attachCount" type="number" class="mt-1.5" placeholder="0" />
        </div>
        <div class="w-36">
          <Label>制单人</Label>
          <Input v-model="form.createdBy" class="mt-1.5" placeholder="李会计" />
        </div>
        <div class="min-w-[16rem] flex-1">
          <Label>凭证摘要</Label>
          <Input v-model="form.remark" class="mt-1.5" placeholder="一句话说明这笔业务" />
        </div>
      </div>

      <!-- 分录表 -->
      <div class="rounded-lg border">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b bg-muted/40">
              <th class="h-9 w-10 px-2 text-left text-xs font-medium text-muted-foreground">#</th>
              <th class="h-9 px-2 text-left text-xs font-medium text-muted-foreground">摘要</th>
              <th class="h-9 px-2 text-left text-xs font-medium text-muted-foreground">科目</th>
              <th class="h-9 w-32 px-2 text-left text-xs font-medium text-muted-foreground">辅助核算</th>
              <th class="h-9 w-32 px-2 text-right text-xs font-medium text-muted-foreground">借方（元）</th>
              <th class="h-9 w-32 px-2 text-right text-xs font-medium text-muted-foreground">贷方（元）</th>
              <th class="h-9 w-10 px-2" />
            </tr>
          </thead>
          <tbody>
            <tr v-for="(l, i) in form.lines" :key="i" class="border-b last:border-0">
              <td class="px-2 py-1 text-xs text-muted-foreground">{{ i + 1 }}</td>
              <td class="px-2 py-1">
                <input
                  v-model="l.summary"
                  class="w-full rounded border-0 bg-transparent px-1.5 py-1 text-sm focus:bg-accent/40 focus:outline-none"
                  placeholder="本行摘要"
                />
              </td>
              <td class="px-2 py-1">
                <button
                  class="flex w-full items-center gap-1.5 rounded px-1.5 py-1 text-left hover:bg-accent/60"
                  @click="openAccountPicker(i)"
                >
                  <template v-if="l.accountCode">
                    <span class="font-mono text-xs text-muted-foreground">{{ l.accountCode }}</span>
                    <span class="truncate">{{ accountOf(l.accountCode)?.fullName ?? l.accountCode }}</span>
                  </template>
                  <span v-else class="text-muted-foreground">选择科目…</span>
                </button>
              </td>
              <td class="px-2 py-1">
                <div v-if="needKeys(l).length" class="flex flex-wrap items-center gap-1">
                  <button
                    v-for="key in needKeys(l)" :key="key"
                    class="truncate rounded px-1.5 py-0.5 text-xs"
                    :class="auxFilled(l, key)
                      ? 'bg-[var(--profit)]/12 text-[var(--profit)]'
                      : 'bg-[var(--warn)]/18 text-[var(--warn)]'"
                    :title="`选择${AUX_LABEL[key] ?? key}`"
                    @click="openAuxPicker(i, key)"
                  >
                    {{ auxChipLabel(l, key) }}
                  </button>
                </div>
                <span v-else class="text-xs text-muted-foreground">—</span>
              </td>
              <td class="px-2 py-1">
                <input
                  v-model="l.debitYuan"
                  class="num w-full rounded border-0 bg-transparent px-1.5 py-1 text-sm focus:bg-accent/40 focus:outline-none"
                  placeholder="0.00"
                  @dblclick="fillDiff(i)"
                  title="双击自动填入差额"
                />
              </td>
              <td class="px-2 py-1">
                <input
                  v-model="l.creditYuan"
                  class="num w-full rounded border-0 bg-transparent px-1.5 py-1 text-sm focus:bg-accent/40 focus:outline-none"
                  placeholder="0.00"
                  @dblclick="fillDiff(i)"
                  title="双击自动填入差额"
                />
              </td>
              <td class="px-2 py-1">
                <button
                  class="rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
                  title="删除本行"
                  @click="removeLine(i)"
                >
                  <Trash2 class="size-3.5" />
                </button>
              </td>
            </tr>
          </tbody>
          <tfoot>
            <tr class="border-t bg-muted/40 font-medium">
              <td colspan="4" class="px-2 py-2">
                <button class="flex items-center gap-1 text-xs hover:underline" @click="addLine">
                  <Plus class="size-3.5" /> 增加一行
                </button>
              </td>
              <td class="num px-2 py-2">{{ fmtMoney(totals.debit) }}</td>
              <td class="num px-2 py-2">{{ fmtMoney(totals.credit) }}</td>
              <td />
            </tr>
          </tfoot>
        </table>
      </div>

      <!-- 附件：中国实务里凭证上要写「附单据 N 张」，
           而 PDF 发票、报销单扫描件都挂在这里 -->
      <div class="rounded-lg border p-3">
        <div class="flex items-center gap-2">
          <Paperclip class="size-3.5 text-muted-foreground" />
          <span class="text-sm font-medium">附单据 {{ attachments.length }} 张</span>
          <span class="text-xs text-muted-foreground">（发票 PDF、报销单扫描件…）</span>
          <Button variant="outline" size="sm" class="ml-auto" @click="pickAttachment">
            <Plus /> 添加附件
          </Button>
        </div>
        <div v-if="attachments.length" class="mt-2 flex flex-wrap gap-2">
          <div
            v-for="a in attachments" :key="a.hash"
            class="flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs"
            :class="a.missing ? 'border-destructive/50 bg-destructive/10' : 'bg-muted/40'"
          >
            <FileText class="size-3.5 shrink-0 text-muted-foreground" />
            <span class="max-w-[14rem] truncate" :title="a.name">{{ a.name }}</span>
            <span class="text-muted-foreground">{{ fmtSize(a.size) }}</span>
            <span v-if="a.missing" class="text-[var(--loss)]">原件丢失</span>
            <button class="text-muted-foreground hover:text-foreground" @click="removeAttachment(a)">
              <X class="size-3" />
            </button>
          </div>
        </div>
        <p v-else-if="!form.id" class="mt-2 text-xs text-muted-foreground">
          先保存草稿，再上传附件。
        </p>
        <p v-else class="mt-2 text-xs text-muted-foreground">
          还没有附件。附件按内容寻址存放，同一份文件挂到多张凭证上只占一份空间。
        </p>
      </div>

      <!-- 平衡提示：录入过程中最需要盯的一行 -->
      <div class="flex items-center gap-3">
        <Badge :variant="totals.balanced ? 'profit' : 'loss'">
          <CheckCircle2 v-if="totals.balanced" class="mr-1 size-3" />
          <AlertTriangle v-else class="mr-1 size-3" />
          {{ totals.balanced ? '借贷平衡' : `差额 ${fmtMoney(totals.diff)}` }}
        </Badge>
        <span class="text-xs text-muted-foreground">
          {{ filledLines.length }} 条分录 · 双击金额格可自动填入差额
        </span>
        <Button variant="ghost" size="sm" class="ml-auto" @click="doCheck">
          <ShieldCheck /> 检查一下
        </Button>
      </div>

      <div
        v-if="checkResult"
        class="flex items-start gap-2 rounded-md border p-2.5 text-sm"
        :class="checkResult.ok
          ? 'border-[var(--profit)]/40 bg-[var(--profit)]/10'
          : 'border-destructive/40 bg-destructive/10'"
      >
        <component :is="checkResult.ok ? ShieldCheck : AlertTriangle" class="mt-0.5 size-4 shrink-0" />
        <span>{{ checkResult.message }}</span>
      </div>
    </div>

    <template #footer>
      <div class="mr-auto flex items-center gap-1.5 text-xs text-muted-foreground">
        <Info class="size-3" />
        保存的是<b class="font-medium">草稿</b>：不占凭证号，也不进总账 ——
        到账期结算时统一过账
      </div>
      <Button variant="ghost" :disabled="busy" @click="open = false">取消</Button>
      <Button :disabled="busy || !canSubmit" @click="save">
        <Save /> 保存草稿
      </Button>
    </template>
  </Modal>

  <!-- 科目选择器 -->
  <Modal
    v-model:open="showAccountPicker"
    title="选择科目"
    description="只列出可记账的明细科目。汇总科目不能直接记账。"
    width="max-w-2xl"
  >
    <div class="flex flex-col gap-3">
      <div class="relative">
        <Search class="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
        <Input
          id="acct-search"
          v-model="accountQuery"
          class="pl-8"
          placeholder="输入编码或名称，如 1002 / 银行存款 / 办公费"
        />
      </div>
      <div class="max-h-[26rem] overflow-auto rounded-lg border">
        <button
          v-for="a in filteredAccounts"
          :key="a.code"
          class="flex w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-0 hover:bg-accent/50"
          @click="pickAccount(a)"
        >
          <span class="w-16 shrink-0 font-mono text-xs text-muted-foreground">{{ a.code }}</span>
          <span class="flex-1 truncate">{{ a.fullName }}</span>
          <Badge variant="muted">{{ a.direction }}</Badge>
          <Badge v-if="a.auxTypes?.length" variant="warn">{{ a.auxTypes.join('、') }}</Badge>
        </button>
        <p v-if="filteredAccounts.length === 0" class="p-6 text-center text-sm text-muted-foreground">
          没有匹配的科目
        </p>
      </div>
    </div>
  </Modal>

  <!-- 辅助核算选择器：客户/供应商/员工/部门/项目 共用一个，
       列表按当前维度切换 —— 应收列客户、应付列供应商，不会选错档案。 -->
  <Modal v-model:open="showAuxPicker" :title="`选择${AUX_LABEL[auxKey] ?? '辅助核算'}`" width="max-w-xl">
    <div class="flex flex-col gap-3">
      <Input v-model="auxQuery" placeholder="输入名称" />
      <div class="max-h-[24rem] overflow-auto rounded-lg border">
        <button
          v-for="c in auxPickerItems"
          :key="c.id"
          class="flex w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-0 hover:bg-accent/50"
          @click="pickAux(c)"
        >
          <span class="flex-1 truncate">{{ c.name }}</span>
          <Badge v-if="c.tag" variant="muted">{{ c.tag }}</Badge>
        </button>
        <p v-if="auxPickerItems.length === 0" class="p-6 text-center text-sm text-muted-foreground">
          没有可选的档案 —— 先到「辅助核算」页建{{ AUX_LABEL[auxKey] ?? '' }}档案
        </p>
      </div>
    </div>
  </Modal>
</template>
