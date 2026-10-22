<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ChevronLeft, ChevronRight, LockKeyhole, Pencil, Plus, RefreshCw, Save, Search, Trash2, Users, Wifi } from 'lucide-vue-next'
import { api, ApiError } from '../api/client'
import type { Account, SavedProxy, SavedProxyTest } from '../api/types'
import { formatDateTime, useI18n } from '../i18n'
import { useToastStore } from '../stores/ui'
import PageHeader from '../components/PageHeader.vue'
import Modal from '../components/Modal.vue'
import Badge from '../components/Badge.vue'

const { locale } = useI18n()
const toast = useToastStore()
const l = computed(() => locale.value.startsWith('zh') ? {
 title: '代理管理', subtitle: '管理账号使用的代理，测试连接并查看真实连接耗时', search: '搜索代理名称或地址...', add: '新增代理', edit: '编辑代理', name: '代理名称', address: '代理地址', status: '连接状态', latency: '耗时', accounts: '关联账号', tested: '测试时间', actions: '操作', test: '测试连接', testing: '测试中', untested: '未测试', success: '连接成功', failed: '连接失败', empty: '暂无代理，请点击新增代理添加', noMatch: '没有找到匹配的代理，请尝试其他名称', loading: '加载中...', refresh: '刷新', save: '保存代理', saving: '保存中...', saved: '代理已保存', cancel: '取消', remove: '删除代理', deleted: '代理已删除', deleteQuestion: '确定删除此代理吗？', inUse: '代理正在被账号使用，请先重新分配或明确断开关联账号', keep: '留空保留当前连接和认证信息；填写新地址时，请包含所需用户名和密码', protocols: '支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H，可在地址中包含用户名和密码', required: '请填写代理名称和完整连接地址', namePlaceholder: '请输入代理名称', addressPlaceholder: '请输入代理连接地址', updatedAccounts: '保存新地址将更新所有关联账号的出口', probeHint: '仅测试连接，不会携带账号令牌；HTTP 401/403 等响应表示目标可达，不代表账号授权成功。', total: '共', records: '条', perPage: '条/页', previous: '上一页', next: '下一页', page: '页', linkedTitle: '关联账号', accountSearch: '搜索账号名称或邮箱...', assign: '分配账号', linkedOnly: '仅显示已关联账号', account: '账号', plan: '订阅', currentProxy: '当前出口', direct: '直连', selected: '已选择', saveAssignment: '保存关联', assignmentSaved: '代理关联已保存', assignmentWarning: '取消勾选的原关联账号将改为直连；勾选其他代理下的账号将切换至当前代理。', noAccounts: '暂无账号，请先在账号管理中添加', noLinked: '暂无关联账号，点击“分配账号”选择', manual: '手工代理', loadFailed: '加载失败，请重试', authenticated: '已保存代理认证', changed: '待保存', testTarget: '目标 HTTP 状态', confirmDisconnect: '保存后直连的账号数：', assignUnavailable: '账号列表尚未成功加载，无法保存',
} : {
 title: 'Proxy management', subtitle: 'Manage account proxies, test connectivity and inspect measured latency', search: 'Search proxy name or address...', add: 'Add proxy', edit: 'Edit proxy', name: 'Proxy name', address: 'Proxy address', status: 'Connection', latency: 'Latency', accounts: 'Accounts', tested: 'Last tested', actions: 'Actions', test: 'Test connection', testing: 'Testing', untested: 'Not tested', success: 'Connected', failed: 'Connection failed', empty: 'No proxies yet. Add a proxy to get started.', noMatch: 'No matching proxies. Try another name.', loading: 'Loading...', refresh: 'Refresh', save: 'Save proxy', saving: 'Saving...', saved: 'Proxy saved', cancel: 'Cancel', remove: 'Delete proxy', deleted: 'Proxy deleted', deleteQuestion: 'Delete this proxy?', inUse: 'This proxy is assigned to accounts. Reassign or explicitly disconnect them before deleting.', keep: 'Leave blank to keep the saved address and credentials. A new address must include any required username and password.', protocols: 'Supports HTTP, HTTPS, SOCKS5 and SOCKS5H, including username and password in the address.', required: 'Enter a proxy name and complete connection address.', namePlaceholder: 'Enter proxy name', addressPlaceholder: 'Enter proxy connection address', updatedAccounts: 'Saving a new address updates the exit for every linked account.', probeHint: 'Connectivity only; no account token is sent. HTTP 401/403 can confirm reachability, not account authorization.', total: 'Total', records: 'items', perPage: '/ page', previous: 'Previous page', next: 'Next page', page: 'page', linkedTitle: 'Linked accounts', accountSearch: 'Search account name or email...', assign: 'Assign accounts', linkedOnly: 'Show linked accounts only', account: 'Account', plan: 'Plan', currentProxy: 'Current exit', direct: 'Direct', selected: 'selected', saveAssignment: 'Save assignments', assignmentSaved: 'Proxy assignments saved', assignmentWarning: 'Previously linked accounts you uncheck will use direct connections. Accounts selected from other proxies will switch to this proxy.', noAccounts: 'No accounts yet. Add one in Account management.', noLinked: 'No linked accounts. Use Assign accounts to select some.', manual: 'Custom proxy', loadFailed: 'Could not load data. Please retry.', authenticated: 'Proxy authentication saved', changed: 'Unsaved', testTarget: 'Target HTTP status', confirmDisconnect: 'Accounts switching to direct: ', assignUnavailable: 'The account list has not loaded successfully; assignments cannot be saved.',
})
const rows = ref<SavedProxy[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const search = ref('')
const loading = ref(false)
const loadError = ref('')
const testing = ref(new Set<number>())
const pages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))
let requestID = 0
let searchTimer: ReturnType<typeof setTimeout> | undefined
function errorMessage(error: unknown) { return error instanceof Error ? error.message : l.value.loadFailed }
async function load() {
 const version = ++requestID
 loading.value = true
 loadError.value = ''
 try {
  const result = await api.listProxies({ search: search.value.trim(), page: page.value, page_size: pageSize.value })
  if (version !== requestID) { return }
  rows.value = result.items
  total.value = result.total
  if (page.value > pages.value) { page.value = pages.value; await load() }
 } catch (error) { if (version === requestID) { loadError.value = errorMessage(error) } }
 finally { if (version === requestID) { loading.value = false } }
}
watch(search, () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { page.value = 1; void load() }, 300) })
function changePage(value: number) { page.value = value; void load() }
function changeSize() { page.value = 1; void load() }
onMounted(() => void load())
onBeforeUnmount(() => { clearTimeout(searchTimer); ++requestID })

const formOpen = ref(false)
const editing = ref<SavedProxy | null>(null)
const name = ref('')
const address = ref('')
const saving = ref(false)
const testingForm = ref(false)
const formTest = ref<SavedProxyTest | null>(null)
const formError = ref('')
const formBusy = computed(() => saving.value || testingForm.value)
function openForm(row: SavedProxy | null = null) {
 editing.value = row
 name.value = row?.name ?? ''
 address.value = ''
 formTest.value = null
 formError.value = ''
 formOpen.value = true
}
function closeForm() { if (!formBusy.value) { formOpen.value = false; address.value = '' } }
watch(address, () => { formTest.value = null })
async function save() {
 if (formBusy.value) { return }
 if (!name.value.trim() || (!editing.value && !address.value.trim())) { formError.value = l.value.required; return }
 saving.value = true
 formError.value = ''
 try {
  const payload = { name: name.value.trim(), url: address.value.trim() }
  if (editing.value) { await api.updateProxy(editing.value.id, payload) }
  else { await api.createProxy(payload) }
  formOpen.value = false
  address.value = ''
  toast.success(l.value.saved)
  await load()
 } catch (error) { formError.value = errorMessage(error) }
 finally { saving.value = false }
}
async function testForm() {
 if (formBusy.value) { return }
 if (!address.value.trim() && !editing.value) { formError.value = l.value.required; return }
 testingForm.value = true
 formError.value = ''
 formTest.value = null
 try {
  const result = address.value.trim() ? await api.probeProxy(address.value.trim()) : await api.testSavedProxy(editing.value!.id)
  formTest.value = result.test
  if (!address.value.trim()) { await load() }
 } catch (error) { formError.value = errorMessage(error) }
 finally { testingForm.value = false }
}
async function testRow(row: SavedProxy) {
 if (testing.value.has(row.id)) { return }
 testing.value.add(row.id)
 try {
  const result = await api.testSavedProxy(row.id)
  if (result.test.success) { toast.success(`${row.name}: ${l.value.success} · ${result.test.latency_ms ?? 0} ms`) }
  else { toast.error(`${row.name}: ${result.test.error || l.value.failed}`) }
  await load()
 } catch (error) { toast.error(errorMessage(error)) }
 finally { testing.value.delete(row.id) }
}
const deleting = ref(false)
const deleteTarget = ref<SavedProxy | null>(null)
const deleteError = ref('')
function askDelete(row: SavedProxy) { deleteError.value = ''; deleteTarget.value = row }
async function remove() {
 if (!deleteTarget.value || deleting.value) { return }
 deleting.value = true
 try { await api.deleteProxy(deleteTarget.value.id); deleteTarget.value = null; toast.success(l.value.deleted); await load() }
 catch (error) { deleteError.value = error instanceof ApiError && error.status === 409 ? l.value.inUse : errorMessage(error) }
 finally { deleting.value = false }
}

const accountsProxy = ref<SavedProxy | null>(null)
const accountsLoading = ref(false)
const accountsLoaded = ref(false)
const accountsError = ref('')
const accountsSaving = ref(false)
const allAccounts = ref<Account[]>([])
const selectedIDs = ref<number[]>([])
const originalIDs = ref<number[]>([])
const accountSearch = ref('')
const linkedOnly = ref(true)
const accountPage = ref(1)
const filteredAccounts = computed(() => {
 const query = accountSearch.value.trim().toLowerCase()
 return allAccounts.value.filter(account => (!linkedOnly.value || selectedIDs.value.includes(account.id)) && (!query || `${account.name} ${account.email}`.toLowerCase().includes(query)))
})
const accountPages = computed(() => Math.max(1, Math.ceil(filteredAccounts.value.length / 10)))
const visibleAccounts = computed(() => filteredAccounts.value.slice((accountPage.value - 1) * 10, accountPage.value * 10))
const disconnectedCount = computed(() => originalIDs.value.filter(id => !selectedIDs.value.includes(id)).length)
const assignmentChanged = computed(() => selectedIDs.value.length !== originalIDs.value.length || selectedIDs.value.some(id => !originalIDs.value.includes(id)))
watch([accountSearch, linkedOnly], () => { accountPage.value = 1 })
watch(accountPages, value => { accountPage.value = Math.min(accountPage.value, value) })
async function openAccounts(row: SavedProxy) {
 accountsProxy.value = row
 accountsLoaded.value = false
 accountsError.value = ''
 accountsLoading.value = true
 allAccounts.value = []
 selectedIDs.value = []
 originalIDs.value = []
 accountSearch.value = ''
 linkedOnly.value = true
 accountPage.value = 1
 try {
  const result = await api.listAccounts()
  if (accountsProxy.value?.id !== row.id) { return }
  allAccounts.value = result.accounts
  selectedIDs.value = result.accounts.filter(account => account.proxy_id === row.id).map(account => account.id)
  originalIDs.value = [...selectedIDs.value]
  accountsLoaded.value = true
 } catch (error) { accountsError.value = errorMessage(error) }
 finally { accountsLoading.value = false }
}
function closeAccounts() { if (!accountsSaving.value) { accountsProxy.value = null } }
async function saveAccounts() {
 if (!accountsProxy.value || accountsSaving.value) { return }
 if (!accountsLoaded.value) { accountsError.value = l.value.assignUnavailable; return }
 accountsSaving.value = true
 accountsError.value = ''
 try {
  await api.assignProxyAccounts(accountsProxy.value.id, selectedIDs.value)
  accountsProxy.value = null
  toast.success(l.value.assignmentSaved)
  await load()
 } catch (error) { accountsError.value = errorMessage(error) }
 finally { accountsSaving.value = false }
}
</script>

<template>
 <div class="page-view">
  <PageHeader :title="l.title" :subtitle="l.subtitle" />
  <div class="page-content">
   <section class="card table-panel proxy-card" :aria-busy="loading">
    <div class="filter-toolbar">
     <label class="search-field proxy-search"><Search aria-hidden="true" /><input v-model="search" class="input" :placeholder="l.search" :aria-label="l.search" /></label>
     <div class="flex items-center gap-2">
      <button class="btn btn-ghost btn-icon" :title="l.refresh" :aria-label="l.refresh" :disabled="loading" @click="load"><RefreshCw :class="{ 'animate-spin': loading }" /></button>
      <button class="btn btn-primary" @click="openForm()"><Plus class="h-4 w-4" />{{ l.add }}</button>
     </div>
    </div>
    <div v-if="loadError" class="proxy-alert" role="alert">{{ loadError }} <button class="btn btn-ghost" @click="load">{{ l.refresh }}</button></div>
    <div class="table-scroll proxy-table-scroll">
     <table class="w-full proxy-table">
      <thead><tr><th class="th">{{ l.name }}</th><th class="th">{{ l.address }}</th><th class="th">{{ l.status }}</th><th class="th">{{ l.latency }}</th><th class="th">{{ l.accounts }}</th><th class="th">{{ l.tested }}</th><th class="th">{{ l.actions }}</th></tr></thead>
      <tbody>
       <tr v-for="row in rows" :key="row.id" class="row-hover">
        <td class="td"><span class="proxy-name" :title="row.name">{{ row.name }}</span></td>
        <td class="td"><div class="flex min-w-0 items-center gap-1 text-[color:var(--color-ink-faint)]"><LockKeyhole v-if="row.url.includes('@')" class="h-3 w-3 shrink-0" :aria-label="l.authenticated" /><code class="proxy-address" :title="row.url">{{ row.url }}</code></div></td>
        <td class="td"><Badge v-if="row.last_test" :tone="row.last_test.success ? 'success' : 'danger'" :title="row.last_test.error || `${l.testTarget}: ${row.last_test.status}`">{{ row.last_test.success ? l.success : l.failed }}</Badge><span v-else class="proxy-muted">{{ l.untested }}</span></td>
        <td class="td"><span v-if="testing.has(row.id)" class="proxy-muted">{{ l.testing }}</span><span v-else-if="row.last_test" class="font-mono tabular-nums" :class="row.last_test.success ? 'proxy-success' : 'proxy-danger'">{{ row.last_test.latency_ms ?? 0 }} ms</span><span v-else class="proxy-muted">—</span></td>
        <td class="td"><button class="btn btn-ghost !px-1" :aria-label="`${row.name}: ${l.accounts} ${row.account_count}`" @click="openAccounts(row)"><Users class="h-3.5 w-3.5" /><span class="font-mono">{{ row.account_count }}</span></button></td>
        <td class="td proxy-muted text-xs">{{ row.last_test ? formatDateTime(row.last_test.tested_at) : '—' }}</td>
        <td class="td"><div class="flex items-center gap-1">
         <button class="btn btn-ghost btn-icon proxy-action" :disabled="testing.has(row.id)" :title="l.test" :aria-label="`${l.test}: ${row.name}`" @click="testRow(row)"><Wifi :class="{ 'animate-pulse': testing.has(row.id) }" /></button>
         <button class="btn btn-ghost btn-icon proxy-action" :disabled="testing.has(row.id)" :title="l.edit" :aria-label="`${l.edit}: ${row.name}`" @click="openForm(row)"><Pencil /></button>
         <button class="btn btn-ghost btn-icon proxy-danger" :disabled="row.account_count > 0 || testing.has(row.id)" :title="row.account_count ? l.inUse : l.remove" :aria-label="row.account_count ? l.inUse : `${l.remove}: ${row.name}`" @click="askDelete(row)"><Trash2 /></button>
        </div></td>
       </tr>
       <tr v-if="!rows.length"><td colspan="7" class="proxy-empty">{{ (() => { if (loading) { return l.loading; } if (loadError) { return l.loadFailed; } if (search) { return l.noMatch; } return l.empty; })() }}</td></tr>
      </tbody>
     </table>
    </div>
    <footer class="proxy-pagination"><span>{{ l.total }} {{ total }} {{ l.records }}</span><div class="flex items-center gap-2"><select v-model.number="pageSize" class="input !w-auto" :aria-label="l.perPage" @change="changeSize"><option v-for="size in [10, 20, 50, 100]" :key="size" :value="size">{{ size }} {{ l.perPage }}</option></select><button class="btn btn-ghost btn-icon" :disabled="loading || page <= 1" :aria-label="l.previous" @click="changePage(page - 1)"><ChevronLeft /></button><span class="tabular-nums">{{ page }} / {{ pages }}</span><button class="btn btn-ghost btn-icon" :disabled="loading || page >= pages" :aria-label="l.next" @click="changePage(page + 1)"><ChevronRight /></button></div></footer>
   </section>
  </div>

  <Modal :open="formOpen" :title="editing ? l.edit : l.add" width="max-w-lg" @close="closeForm">
   <form id="proxy-form" class="grid gap-5" @submit.prevent="save">
    <label class="grid gap-2"><span class="text-sm font-medium">{{ l.name }} <span class="proxy-danger">*</span></span><input v-model="name" class="input" maxlength="100" required :disabled="formBusy" :placeholder="l.namePlaceholder" :aria-label="l.name" /></label>
    <label class="grid gap-2">
     <span class="text-sm font-medium">{{ l.address }} <span v-if="!editing" class="proxy-danger">*</span></span>
     <input v-model="address" class="input" type="text" autocomplete="off" spellcheck="false" :required="!editing" :disabled="formBusy" :placeholder="l.addressPlaceholder" :aria-label="l.address" />
     <span class="proxy-muted text-xs leading-relaxed">{{ editing ? l.keep : l.protocols }}</span>
    </label>
    <p v-if="editing" class="proxy-muted break-all font-mono text-xs">{{ editing.url }}</p>
    <p v-if="editing?.account_count && address.trim()" class="proxy-warning text-xs">{{ l.updatedAccounts }} ({{ editing.account_count }})</p>
    <p v-if="formError" class="proxy-danger text-sm" role="alert">{{ formError }}</p>
    <output v-if="formTest" class="text-sm" :class="formTest.success ? 'proxy-success' : 'proxy-danger'" >{{ formTest.success ? l.success : l.failed }} · {{ formTest.latency_ms ?? 0 }} ms<span v-if="formTest.status"> · HTTP {{ formTest.status }}</span><span style="display: block" v-if="formTest.error" class="mt-1">{{ formTest.error }}</span></output>
    <p class="proxy-muted text-xs leading-relaxed">{{ l.probeHint }}</p>
   </form>
   <template #footer><button class="btn" :disabled="formBusy" @click="closeForm">{{ l.cancel }}</button><button class="btn" :disabled="formBusy" @click="testForm"><Wifi class="h-4 w-4" />{{ testingForm ? l.testing : l.test }}</button><button class="btn btn-primary" form="proxy-form" type="submit" :disabled="formBusy"><Save class="h-4 w-4" />{{ saving ? l.saving : l.save }}</button></template>
  </Modal>

  <Modal :open="!!deleteTarget" :title="l.remove" @close="!deleting && (deleteTarget = null)"><p>{{ l.deleteQuestion }}</p><p class="mt-2 font-medium">{{ deleteTarget?.name }}</p><p v-if="deleteError" class="proxy-danger mt-3" role="alert">{{ deleteError }}</p><template #footer><button class="btn" :disabled="deleting" @click="deleteTarget = null">{{ l.cancel }}</button><button class="btn btn-danger" :disabled="deleting" @click="remove">{{ l.remove }}</button></template></Modal>

  <Modal :open="!!accountsProxy" :title="l.linkedTitle" width="max-w-3xl" @close="closeAccounts">
   <p class="proxy-muted mb-4 text-sm">{{ accountsProxy?.name }}</p>
   <div class="flex flex-wrap items-center justify-between gap-3 mb-4"><label class="search-field proxy-search"><Search aria-hidden="true" /><input v-model="accountSearch" class="input" :placeholder="l.accountSearch" :aria-label="l.accountSearch" /></label><button class="btn" :disabled="accountsLoading || accountsSaving" @click="linkedOnly = !linkedOnly">{{ linkedOnly ? l.assign : l.linkedOnly }}</button></div>
   <p v-if="accountsError" class="proxy-danger mb-3 text-sm" role="alert">{{ accountsError }}</p>
   <div class="table-scroll proxy-accounts-scroll" :aria-busy="accountsLoading"><table class="w-full"><thead><tr><th class="th w-10"></th><th class="th">{{ l.account }}</th><th class="th">{{ l.plan }}</th><th class="th">{{ l.currentProxy }}</th></tr></thead><tbody>
    <tr v-for="account in visibleAccounts" :key="account.id" class="row-hover"><td class="td"><input v-model="selectedIDs" type="checkbox" :value="account.id" :aria-label="`${l.assign}: ${account.name}`" :disabled="accountsSaving" /></td><td class="td"><div class="font-medium">{{ account.name }}</div><div class="proxy-muted text-xs font-mono mt-1">{{ account.email || '—' }}</div></td><td class="td"><Badge tone="neutral">{{ account.plan_type || '—' }}</Badge></td><td class="td proxy-muted"><span v-if="account.proxy_id === accountsProxy?.id">{{ accountsProxy?.name }}</span><code v-else class="text-xs break-all">{{ account.proxy_display === 'direct' ? l.direct : account.proxy_display }}</code></td></tr>
    <tr v-if="!visibleAccounts.length"><td colspan="4" class="proxy-empty">{{ (() => { if (accountsLoading) { return l.loading; } if (!allAccounts.length) { return l.noAccounts; } return l.noLinked; })() }}</td></tr>
   </tbody></table></div>
   <div class="proxy-pagination !px-0"><span>{{ selectedIDs.length }} {{ l.selected }}</span><div class="flex items-center gap-2"><button class="btn btn-ghost btn-icon" :disabled="accountPage <= 1" :aria-label="l.previous" @click="accountPage--"><ChevronLeft /></button><span>{{ accountPage }} / {{ accountPages }}</span><button class="btn btn-ghost btn-icon" :disabled="accountPage >= accountPages" :aria-label="l.next" @click="accountPage++"><ChevronRight /></button></div></div>
   <p class="proxy-warning text-xs leading-relaxed">{{ l.assignmentWarning }}</p><p v-if="disconnectedCount" class="proxy-danger mt-2 text-sm" role="alert">{{ l.confirmDisconnect }}{{ disconnectedCount }}</p>
   <template #footer><button class="btn" :disabled="accountsSaving" @click="closeAccounts">{{ l.cancel }}</button><button class="btn btn-primary" :disabled="accountsSaving || !accountsLoaded || !assignmentChanged" @click="saveAccounts">{{ accountsSaving ? l.saving : l.saveAssignment }}</button></template>
  </Modal>
 </div>
</template>

<style scoped>
.proxy-card { display: flex; flex-direction: column; min-height: 500px; height: calc(100dvh - 142px); overflow: hidden; }
.proxy-search { width: 320px; max-width: 100%; }
.proxy-table-scroll { flex: 1; min-height: 0; }
.proxy-table { min-width: 960px; table-layout: fixed; }
.proxy-table th:nth-child(1) { width: 17%; }
.proxy-table th:nth-child(2) { width: 25%; }
.proxy-table th:nth-child(3) { width: 11%; }
.proxy-table th:nth-child(4) { width: 9%; }
.proxy-table th:nth-child(5) { width: 9%; }
.proxy-table th:nth-child(6) { width: 17%; }
.proxy-table th:nth-child(7) { width: 124px; }
.proxy-table tbody tr { height: 60px; }
.proxy-name, .proxy-address { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.proxy-address { font-size: 12px; }
.proxy-muted { color: var(--color-ink-faint); }
.proxy-success { color: var(--color-success); }
.proxy-danger { color: var(--color-danger); }
.proxy-warning { color: var(--color-warn); }
.proxy-action { color: var(--color-accent); }
.proxy-empty { height: 320px; text-align: center; padding: 32px; color: var(--color-ink-faint); font-size: 13px; }
.proxy-alert { padding: 12px 20px; color: var(--color-danger); }
.proxy-pagination { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; padding: 14px 20px; border-top: 1px solid var(--color-line); font-size: 12px; color: var(--color-ink-faint); }
.proxy-pagination .input { min-height: 32px; height: 32px; font-size: 12px; }
.proxy-accounts-scroll { min-height: 320px; max-height: 55dvh; }
.proxy-accounts-scroll tbody tr { height: 64px; }
@media (max-width: 640px) { .proxy-search { width: 100%; } .proxy-card { height: auto; min-height: 500px; } .proxy-pagination { padding: 12px; } }
</style>
