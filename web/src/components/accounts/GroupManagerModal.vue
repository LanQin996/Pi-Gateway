<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Pencil, Plus, Search, Trash2, Users } from 'lucide-vue-next'
import { useI18n } from '../../i18n'
import { ApiError, api } from '../../api/client'
import type { Account, AccountGroup } from '../../api/types'
import { useToastStore } from '../../stores/ui'
import Modal from '../Modal.vue'
import Badge from '../Badge.vue'
import Toggle from '../Toggle.vue'
import ModelRestrictionSelector from './ModelRestrictionSelector.vue'
import { useAccountControls } from './accountControlsLocale'

const props = defineProps<{ open: boolean; accounts: Account[]; groups: AccountGroup[]; loaded: boolean; error: string }>()
const emit = defineEmits<{ close: []; changed: [] }>()
const { c } = useAccountControls()
const { t } = useI18n()
const switchOn429 = ref('inherit')
const toast = useToastStore()
const localGroups = ref<AccountGroup[]>([])
const editing = ref(false)
const editingId = ref<number | null>(null)
const name = ref('')
const notes = ref('')
const enabled = ref(true)
const members = ref<number[]>([])
const disabledModels = ref<string[]>([])
const search = ref('')
const formError = ref('')
const saving = ref(false)
const deleting = ref<AccountGroup | null>(null)
const deleteBusy = ref(false)
const accountName = (account: Account) => account.name || account.email || c('unnamedAccount')
const visibleAccounts = computed(() => {
	const query = search.value.trim().toLowerCase()
	return props.accounts.filter(account => !query || `${account.name} ${account.email}`.toLowerCase().includes(query))
})
const sources = computed(() => props.accounts.filter(account => {
	if (members.value.includes(account.id)) { return true }
	// Preview membership after saving: removing the last group makes an account public.
	return !localGroups.value.some(group => group.id !== editingId.value && group.account_ids?.includes(account.id))
		&& !(account.group_ids ?? []).some(id => id !== editingId.value && !localGroups.value.some(group => group.id === id))
}))
watch(() => props.groups, value => { localGroups.value = [...value] }, { immediate: true })
watch(() => props.open, open => { if (open) { editing.value = false; deleting.value = null; formError.value = ''; search.value = '' } })
function startEdit(group?: AccountGroup) {
	editingId.value = group?.id ?? null
	name.value = group?.name ?? ''
	notes.value = group?.notes ?? ''
	switchOn429.value = group?.switch_on_429 ?? 'inherit'
 enabled.value = group?.enabled ?? true
	members.value = [...(group?.account_ids ?? [])]
	disabledModels.value = [...(group?.disabled_models ?? [])]
	search.value = ''
	formError.value = ''
	deleting.value = null
	editing.value = true
}
function setMember(id: number, checked: boolean) {
	members.value = checked ? [...new Set([...members.value, id])] : members.value.filter(value => value !== id)
}
async function save() {
	if (saving.value) { return }
	const trimmed = name.value.trim()
	if (!trimmed) { formError.value = c('requiredName'); return }
	if (localGroups.value.some(group => group.id !== editingId.value && group.name.trim().toLocaleLowerCase() === trimmed.toLocaleLowerCase())) { formError.value = c('duplicateName'); return }
	saving.value = true
	formError.value = ''
	try {
		const payload = { switch_on_429: switchOn429.value, name: trimmed, notes: notes.value.trim(), enabled: enabled.value, account_ids: [...members.value], disabled_models: [...disabledModels.value] }
		const { group } = editingId.value === null ? await api.createAccountGroup(payload) : await api.updateAccountGroup(editingId.value, payload)
		localGroups.value = [...localGroups.value.filter(item => item.id !== group.id), group]
		editing.value = false
		toast.success(c('groupSaved'))
		emit('changed')
	} catch (err) {
		formError.value = (() => {
    if (err instanceof ApiError && (err.status === 409 || /already exists|unique|duplicate/i.test(err.message))) {
        return c('duplicateName')
    }
    if (err instanceof Error) {
        return err.message
    }
    return c('requestFailed')
})()
	} finally { saving.value = false }
}
async function remove() {
	if (!deleting.value || deleteBusy.value) { return }
	deleteBusy.value = true
	formError.value = ''
	try {
		await api.deleteAccountGroup(deleting.value.id)
		localGroups.value = localGroups.value.filter(group => group.id !== deleting.value!.id)
		deleting.value = null
		toast.success(c('groupDeleted'))
		emit('changed')
	} catch (err) { formError.value = err instanceof Error ? err.message : c('requestFailed') }
	finally { deleteBusy.value = false }
}
function close() { if (!saving.value && !deleteBusy.value) { emit('close') } }
</script>

<template>
	<Modal :open="open" :title="c('manageGroups')" width="max-w-3xl" @close="close">
		<div class="group-manager">
			<p class="group-rule">{{ c('groupsHint') }}</p>
			<p class="group-rule">{{ c('rules') }}</p>
			<p v-if="props.error" class="group-error" role="alert">{{ props.error }}</p>
			<p v-if="formError" class="group-error" role="alert">{{ formError }}</p>
			<template v-if="!editing">
				<button type="button" class="btn btn-primary new-group" :disabled="!loaded || deleteBusy" @click="startEdit()"><Plus :size="15" />{{ c('newGroup') }}</button>
				<p v-if="loaded && !localGroups.length" class="group-hint">{{ c('noGroups') }}</p>
				<article v-for="group in localGroups" :key="group.id" class="group-row">
					<div class="group-copy"><strong>{{ group.name }}</strong><span v-if="group.notes">{{ group.notes }}</span><small>{{ c('groupCounts', { members: group.account_ids?.length ?? 0, models: group.disabled_models?.length ?? 0 }) }}</small><Badge :tone="group.enabled ? 'success' : 'neutral'">{{ c(group.enabled ? 'enabled' : 'disabled') }}</Badge></div>
					<div class="group-actions"><button type="button" class="btn btn-ghost" :disabled="deleteBusy" :aria-label="`${c('edit')} ${group.name}`" @click="startEdit(group)"><Pencil :size="14" />{{ c('edit') }}</button><button type="button" class="btn btn-ghost delete-group" :disabled="deleteBusy" :aria-label="`${c('deleteGroup')} ${group.name}`" @click="deleting = group; formError = ''"><Trash2 :size="14" /></button></div>
				</article>
				<section v-if="deleting" class="delete-confirm" role="alert"><p>{{ c('deleteImpact', { name: deleting.name }) }}</p><div><button type="button" class="btn" :disabled="deleteBusy" @click="deleting = null">{{ c('cancel') }}</button><button type="button" class="btn btn-danger" :disabled="deleteBusy" @click="remove">{{ c('deleteGroup') }}</button></div></section>
			</template>
			<form v-else class="group-editor" @submit.prevent="save">
				<div><label class="label" for="group-name">{{ c('groupName') }}</label><input id="group-name" v-model="name" class="input" maxlength="200" :disabled="saving" required /></div>
				<div><label class="label" for="group-notes">{{ c('groupNotes') }}</label><textarea id="group-notes" v-model="notes" class="input" rows="2" maxlength="2000" :disabled="saving" /></div>
				<div><label class="label" for="group-switch-429">{{ t('retry429.switch') }}</label><select id="group-switch-429" v-model="switchOn429" class="input" :disabled="saving"><option value="inherit">{{ t('retry429.inherit') }}</option><option value="enabled">{{ c('enabled') }}</option><option value="disabled">{{ c('disabled') }}</option></select><p class="group-hint">{{ t('retry429.groupHint') }}</p></div>
 <Toggle v-model="enabled" :label="c('enabled')" :disabled="saving" />
				<fieldset class="members-field" :disabled="saving"><legend><Users :size="15" />{{ c('members') }} · {{ members.length }}</legend><label class="search-field"><Search :size="16" aria-hidden="true" /><input v-model="search" class="input" :placeholder="c('memberSearch')" :aria-label="c('memberSearch')" /></label><div class="members-list"><label v-for="account in visibleAccounts" :key="account.id" class="member-choice"><input type="checkbox" :checked="members.includes(account.id)" @change="setMember(account.id, ($event.target as HTMLInputElement).checked)" /><span><strong>{{ accountName(account) }}</strong><small v-if="account.email && account.email !== account.name">{{ account.email }}</small><small v-if="!account.enabled">{{ c('disabled') }}</small></span></label><p v-if="!visibleAccounts.length" class="group-hint">{{ c('noMatches') }}</p></div><p v-if="!members.length" class="group-hint">{{ c('noMembers') }}</p></fieldset>
				<ModelRestrictionSelector :key="editingId ?? 'new'" v-model="disabledModels" :sources="sources" group-mode :disabled="saving" />
				<p class="group-hint">{{ c('saveFirst') }}</p>
				<div class="editor-actions"><button type="button" class="btn" :disabled="saving" @click="editing = false; formError = ''">{{ c('cancel') }}</button><button type="submit" class="btn btn-primary" :disabled="saving">{{ saving ? c('saving') : c('save') }}</button></div>
			</form>
		</div>
		<template v-if="!editing" #footer><button type="button" class="btn" :disabled="deleteBusy" @click="close">{{ c('close') }}</button></template>
	</Modal>
</template>

<style scoped>
.group-manager, .group-editor { display: grid; min-width: 0; gap: 14px; }
.group-rule, .group-hint, .group-error { margin: 0; font-size: 11px; line-height: 1.65; overflow-wrap: anywhere; }.group-rule { padding: 10px; border-radius: 8px; background: var(--color-surface-2); color: var(--color-ink-muted); }.group-hint { color: var(--color-ink-faint); }.group-error { color: var(--color-danger); }
.new-group { justify-self: start; }
.group-row { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; min-width: 0; padding: 12px; border: 1px solid var(--color-line); border-radius: 10px; }
.group-copy { display: flex; flex: 1 1 180px; min-width: 0; flex-direction: column; align-items: flex-start; gap: 6px; overflow-wrap: anywhere; }.group-copy strong { max-width: 100%; font-size: 13px; }.group-copy span, .group-copy small { color: var(--color-ink-faint); font-size: 11px; }
.group-actions, .editor-actions, .delete-confirm > div { display: flex; gap: 8px; flex-wrap: wrap; justify-content: flex-end; }.group-actions .btn { min-height: 32px; padding: 5px 9px; font-size: 11px; }.delete-group { color: var(--color-danger); }
.delete-confirm { padding: 12px; border: 1px solid var(--color-danger); border-radius: 10px; color: var(--color-danger); font-size: 12px; line-height: 1.6; }.delete-confirm p { margin: 0 0 12px; overflow-wrap: anywhere; }
.members-field { display: grid; gap: 10px; min-width: 0; padding: 12px; border: 1px solid var(--color-line); border-radius: 10px; }.members-field legend { display: flex; align-items: center; gap: 6px; padding: 0 5px; font-size: 12px; font-weight: 600; }.members-field .search-field { min-width: 0; width: 100%; }.members-list { max-height: 210px; overflow-y: auto; overscroll-behavior: contain; }
.member-choice { display: flex; align-items: flex-start; gap: 9px; padding: 8px; border-radius: 6px; cursor: pointer; }.member-choice:hover { background: var(--color-surface-2); }.member-choice input { margin-top: 3px; flex-shrink: 0; accent-color: var(--color-accent); }.member-choice > span { display: grid; gap: 4px; min-width: 0; overflow-wrap: anywhere; }.member-choice strong { font-size: 12px; font-weight: 600; }.member-choice small { color: var(--color-ink-faint); font-size: 10px; }
@media (max-width: 390px) { .group-row, .members-field { padding: 9px; }.group-actions { width: 100%; }.editor-actions .btn { flex: 1; } }
</style>
