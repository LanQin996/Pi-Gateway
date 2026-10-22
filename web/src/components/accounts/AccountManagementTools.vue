<script setup lang="ts">
import { computed, ref } from 'vue'
import { Download, Upload, Users } from 'lucide-vue-next'
import { api, type AccountOperationResult } from '../../api/client'
import type { Account, AccountGroup } from '../../api/types'
import { useAccountManagementLocale } from './accountManagementLocale'
import Modal from '../Modal.vue'
import Badge from '../Badge.vue'
const props = defineProps<{ accounts: Account[]; groups: AccountGroup[]; selectedIds: number[]; groupsLoaded: boolean }>()
const emit = defineEmits<{ changed: []; busy: [value: boolean]; clear: [] }>()
const {m} = useAccountManagementLocale()
const busy = ref(false)
const error = ref('')
const importOpen = ref(false)
const groupsOpen = ref(false)
const resultsOpen = ref(false)
const results = ref<Array<AccountOperationResult & {file?: string}>>([])
const format = ref('auto')
const files = ref<File[]>([])
const pasted = ref('')
const targetId = ref('')
const groupIds = ref<number[]>([])
const summary = computed(() => m('summary', {
    success: results.value.filter(r => ['success', 'created', 'linked'].includes(r.status)).length,
    skipped: results.value.filter(r => r.status === 'skipped').length,
    failed: results.value.filter(r => r.status === 'failed').length,
}))
function setBusy(value: boolean) { busy.value = value; emit('busy', value) }
function showImport() { error.value = ''; files.value = []; pasted.value = ''; targetId.value = ''; importOpen.value = true }
async function batch(action: 'enable' | 'disable' | 'recover' | 'add_groups') {
    if (busy.value || !props.selectedIds.length) { return }
    setBusy(true); error.value = ''
    try {
        results.value = (await api.batchAccounts([...props.selectedIds], action, [...groupIds.value])).results
        groupsOpen.value = false; resultsOpen.value = true; emit('changed')
    } catch (err) { error.value = err instanceof Error ? err.message : m('failed') }
    finally { setBusy(false) }
}
async function exportFile(all: boolean) {
    if (busy.value) { return }
    setBusy(true); error.value = ''
    try {
        const data = await api.exportAccounts(all ? props.accounts.map(a => a.id) : [...props.selectedIds])
        const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], {type: 'application/json'}))
        const anchor = document.createElement('a'); anchor.href = url; anchor.download = 'pi-gateway-accounts.json'
        anchor.click(); URL.revokeObjectURL(url)
    } catch (err) { error.value = err instanceof Error ? err.message : m('failed') }
    finally { setBusy(false) }
}
async function importDocument(doc: {name: string; read: () => Promise<string>}) {
    try {
        const text = await doc.read()
        if (new Blob([text]).size > 16 * 1024 * 1024) { throw new Error(m('tooLarge')) }
        let data: unknown
        try { data = JSON.parse(text) } catch { throw new Error(m('invalidJSON')) }
        const response = await api.importAccounts(format.value, data, targetId.value ? Number(targetId.value) : undefined)
        results.value.push(...response.results.map(result => ({...result, file: doc.name})))
    } catch (err) {
        results.value.push({index: 0, status: 'failed', file: doc.name, error: err instanceof Error ? err.message : m('fileFailed')})
    }
}
async function importData() {
    if (busy.value) { return }
    error.value = ''
    if (!files.value.length && !pasted.value.trim()) { error.value = m('chooseData'); return }
    if (targetId.value && files.value.length + (pasted.value.trim() ? 1 : 0) > 1) { error.value = m('multipleTarget'); return }
    setBusy(true); results.value = []
    try {
        const docs: Array<{name: string; read: () => Promise<string>}> = files.value.map(file => ({name: file.name, read: async () => {
            if (file.size > 16 * 1024 * 1024) { throw new Error(m('tooLarge')) }
            return file.text()
        }}))
        if (pasted.value.trim()) { docs.push({name: m('paste'), read: async () => pasted.value}) }
        // Imports may target the same account, so preserve file order rather than race mutations.
        await docs.reduce((previous, doc) => previous.then(() => importDocument(doc)), Promise.resolve())
        files.value = []; pasted.value = ''; importOpen.value = false; resultsOpen.value = true; emit('changed')
    } finally { setBusy(false) }
}
</script>

<template>
    <section class="management-tools" :aria-label="m('title')" :aria-busy="busy">
        <div class="management-actions">
            <div class="selection-actions" v-if="selectedIds.length">
                <strong>{{ m('selected', {count: selectedIds.length}) }}</strong>
                <button class="btn btn-sm" :disabled="busy" @click="batch('enable')">{{ m('enable') }}</button>
                <button class="btn btn-sm" :disabled="busy" @click="batch('disable')">{{ m('disable') }}</button>
                <button class="btn btn-sm" :disabled="busy" :title="m('recoverHint')" @click="batch('recover')">{{ m('recover') }}</button>
                <button class="btn btn-sm" :disabled="busy || !groupsLoaded" @click="groupIds = []; groupsOpen = true; error = ''"><Users :size="14" />{{ m('groups') }}</button>
                <button class="btn btn-ghost btn-sm" :disabled="busy" @click="emit('clear')">{{ m('clear') }}</button>
            </div>
            <div class="transfer-actions">
                <button class="btn btn-sm" :disabled="busy" @click="showImport"><Upload :size="14" />{{ m('import') }}</button>
                <button class="btn btn-sm" :disabled="busy || !accounts.length" :title="m('exportHint')" @click="exportFile(!selectedIds.length)"><Download :size="14" />{{ m(selectedIds.length ? 'export' : 'exportAll') }}</button>
            </div>
        </div>
        <p v-if="error && !importOpen && !groupsOpen" class="text-danger" role="alert">{{ error }}</p>
        <Modal :open="groupsOpen" :title="m('groups')" @close="!busy && (groupsOpen = false)">
            <p class="management-hint">{{ m('groupHint') }}</p>
            <p v-if="!groups.length" class="management-hint">{{ m('emptyGroups') }}</p>
            <div class="group-options"><label v-for="group in groups" :key="group.id"><input v-model="groupIds" type="checkbox" :value="group.id" :disabled="busy" />{{ group.name }}</label></div>
            <p v-if="error" class="text-danger" role="alert">{{ error }}</p>
            <template #footer><button class="btn btn-primary" :disabled="busy || !groupIds.length" @click="batch('add_groups')">{{ m(busy ? 'processing' : 'apply') }}</button></template>
        </Modal>
        <Modal :open="importOpen" :title="m('import')" width="max-w-2xl" @close="!busy && (importOpen = false)">
            <div class="import-fields">
                <p class="management-hint">{{ m('importHint') }}</p>
                <label>{{ m('format') }}<select v-model="format" class="input" :disabled="busy"><option value="auto">{{ m('auto') }}</option><option value="pi-gateway">{{ m('native') }}</option><option value="sub2api">{{ m('sub2api') }}</option><option value="cliproxyapi">{{ m('cliproxyapi') }}</option></select></label>
                <label>{{ m('files') }}<input class="input import-file" type="file" accept=".json,application/json" multiple :disabled="busy" @change="files = Array.from(($event.target as HTMLInputElement).files ?? [])" /></label>
                <span class="management-hint">{{ m('importSize') }}</span>
                <label>{{ m('paste') }}<textarea v-model="pasted" class="input import-json" :disabled="busy" spellcheck="false" placeholder='{"type":"pi-gateway-accounts","version":1,"accounts":[...]}' /></label>
                <label>{{ m('target') }}<select v-model="targetId" class="input" :disabled="busy || format === 'pi-gateway'"><option value="">{{ m('autoMatch') }}</option><option v-for="account in accounts" :key="account.id" :value="String(account.id)">{{ account.name }} · {{ account.email || account.id }}</option></select></label>
                <p v-if="error" class="text-danger" role="alert">{{ error }}</p>
            </div>
            <template #footer><button class="btn btn-primary" :disabled="busy" @click="importData">{{ m(busy ? 'importing' : 'import') }}</button></template>
        </Modal>
        <Modal :open="resultsOpen" :title="m('results')" width="max-w-2xl" @close="resultsOpen = false">
            <output class="result-summary" >{{ summary }}</output>
            <ul class="operation-results"><li v-for="(result, i) in results" :key="i"><div><strong>{{ result.name || m('row', {index: result.index + 1}) }}</strong><small v-if="result.file">{{ result.file }}</small><p v-if="result.error" class="text-danger">{{ result.error }}</p></div><Badge :tone="(() => { if (result.status === 'failed') { return 'danger'; } if (result.status === 'skipped') { return 'neutral'; } return 'success'; })()">{{ m(result.status) }}</Badge></li></ul>
            <template #footer><button class="btn" @click="resultsOpen = false">{{ m('close') }}</button></template>
        </Modal>
    </section>
</template>

<style scoped>
.management-tools { min-width: 0; }
.management-actions, .selection-actions, .transfer-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.management-actions { justify-content: space-between; }
.transfer-actions { margin-left: auto; }
.selection-actions strong { font-size: 12px; margin-right: 4px; }
.management-hint { color: var(--color-ink-muted); font-size: 12px; line-height: 1.7; }
.import-fields { display: grid; gap: 14px; }
.import-fields label { display: grid; gap: 6px; font-size: 12px; }
.import-fields label > .input { width: 100%; min-width: 0; box-sizing: border-box; }
.import-file { min-height: 40px; padding: 0 12px 0 0; line-height: 38px; font-size: 12px; overflow: hidden; text-overflow: ellipsis; }
.import-file::file-selector-button { height: 38px; margin: 0 12px 0 0; padding: 0 14px; border: 0; border-right: 1px solid var(--color-line); background: var(--color-surface-2); color: var(--color-ink); font: inherit; cursor: pointer; vertical-align: top; }
.import-file:disabled::file-selector-button { cursor: default; }
.import-file::file-selector-button:hover { background: var(--color-row-hover); }
.import-json { min-height: 140px; resize: vertical; font-family: monospace; }
.group-options { display: grid; gap: 12px; margin: 16px 0; }
.group-options label { display: flex; align-items: center; gap: 8px; }
.operation-results { list-style: none; padding: 0; margin: 12px 0; }
.operation-results li { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; padding: 12px 0; border-bottom: 1px solid var(--color-line); font-size: 12px; }
.operation-results li > div { min-width: 0; overflow-wrap: anywhere; }
.operation-results small { display: block; color: var(--color-ink-muted); margin-top: 4px; }
.result-summary { font-size: 13px; }
@media (max-width: 640px) { .transfer-actions { margin-left: 0; } }
</style>
