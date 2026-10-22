<script setup lang="ts">
import { Plus, RefreshCw, Search } from 'lucide-vue-next'
import type { AccountGroup } from '../../api/types'
import { useI18n } from '../../i18n'
import { accountStatusLabel } from '../../utils/uiOptions'

defineProps<{ groups: AccountGroup[]; statuses: string[]; loading: boolean; groupsLoaded: boolean }>()
const search = defineModel<string>('search', { required: true })
const status = defineModel<string>('status', { required: true })
const group = defineModel<string>('group', { required: true })
const emit = defineEmits<{ refresh: []; create: [] }>()
const { t } = useI18n()
</script>

<template>
	<fieldset class="account-filters"  :aria-label="t('accounts.title')">
		<div class="account-filter-fields">
			<label class="search-field"><Search :size="18" aria-hidden="true" /><input v-model="search" class="input" :placeholder="t('accounts.search')" :aria-label="t('accounts.search')" /></label>
			<select v-model="status" class="input" :aria-label="t('accounts.col.status')"><option value="">{{ t('common.allStatuses') }}</option><option value="disabled">{{ t('common.disabled') }}</option><option v-for="item in statuses.filter(item => item !== 'disabled')" :key="item" :value="item">{{ accountStatusLabel(item) }}</option></select>
			<select v-model="group" class="input" :disabled="!groupsLoaded" :aria-label="t('accounts.groups')"><option value="">{{ t('accounts.allGroups') }}</option><option value="ungrouped">{{ t('accounts.ungrouped') }}</option><option v-for="item in groups" :key="item.id" :value="String(item.id)">{{ item.name }}{{ item.enabled ? '' : ` (${t('common.disabled')})` }}</option></select>
		</div>
		<div class="account-filter-actions"><button type="button" class="btn btn-ghost refresh-button" :title="t('common.refresh')" :aria-label="t('common.refresh')" :disabled="loading" @click="emit('refresh')"><RefreshCw :size="18" :class="{ 'animate-spin': loading }" /></button><button type="button" class="btn btn-primary" @click="emit('create')"><Plus :size="16" />{{ t('accounts.add') }}</button></div>
	</fieldset>
</template>

<style scoped>
.account-filters { display: flex; width: 100%; align-items: center; flex-wrap: wrap; gap: 12px; padding: 20px 24px; }
.account-filter-fields { display: flex; min-width: 0; flex-wrap: wrap; gap: 12px; }
.search-field { width: 280px; }
.account-filter-fields > select { width: 144px; min-width: 0; max-width: 100%; text-overflow: ellipsis; }
.account-filter-actions { display: flex; align-items: center; justify-content: flex-end; gap: 8px; margin-left: auto; }
.refresh-button { width: 36px; padding: 0; }
@media (max-width: 767px) { .account-filters { padding: 16px; } .account-filter-fields { width: 100%; display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; } .search-field { width: 100%; grid-column: 1 / -1; } .account-filter-fields > select { width: 100%; } }
</style>
