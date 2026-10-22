<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { AlertCircle, ChevronDown, Eye, EyeOff, KeyRound, Languages, LoaderCircle, Mail, Moon, ShieldCheck, Sun } from 'lucide-vue-next'
import LoginBackground from '../components/login/LoginBackground.vue'
import { useAuthStore } from '../stores/auth'
import { useToastStore, useUiStore } from '../stores/ui'
import { useI18n } from '../i18n'
import { LOCALES, type Locale } from '../i18n/messages'

const auth = useAuthStore()
const toast = useToastStore()
const ui = useUiStore()
const { t, locale, setLocale } = useI18n()
const route = useRoute()
const router = useRouter()

const username = ref('admin')
const password = ref('')
const showPassword = ref(false)
const busy = ref(false)
const error = ref('')
const canSubmit = computed(() => Boolean(username.value.trim() && password.value) && !busy.value)

// View-only copy stays with this page; shared labels use the existing catalogue.
const copy = computed(() => locale.value === 'zh-CN' ? {
	title: '控制台登录',
	welcome: '「 欢迎回来，登录以管理您的网关 」',
	admin: '管理员登录',
	usernamePlaceholder: '输入管理员账号',
	passwordPlaceholder: '输入管理员密码',
	showPassword: '显示密码',
	hidePassword: '隐藏密码',
	lightTheme: '切换浅色模式',
	darkTheme: '切换深色模式',
	help: '首次登录？',
	failed: '登录失败，请重试',
} : {
	title: 'Console sign-in',
	welcome: '「 Welcome back to your gateway 」',
	admin: 'Administrator sign-in',
	usernamePlaceholder: 'Enter your administrator username',
	passwordPlaceholder: 'Enter your administrator password',
	showPassword: 'Show password',
	hidePassword: 'Hide password',
	lightTheme: 'Switch to light mode',
	darkTheme: 'Switch to dark mode',
	help: 'First time signing in?',
	failed: 'Sign-in failed, please try again',
})
const themeLabel = computed(() => ui.theme === 'dark' ? copy.value.lightTheme : copy.value.darkTheme)

function changeLocale(event: Event) {
	setLocale((event.target as HTMLSelectElement).value as Locale)
}

async function submit() {
	if (!canSubmit.value) { return }
	busy.value = true
	error.value = ''
	try {
		await auth.signIn(username.value, password.value)
		const requested = route.query.redirect
		const redirect = typeof requested === 'string' && requested.startsWith('/') && !requested.startsWith('//')
			? requested
			: '/dashboard'
		await router.push(redirect)
	} catch (err) {
		error.value = err instanceof Error ? err.message : copy.value.failed
		toast.error(error.value)
	} finally {
		busy.value = false
	}
}
</script>

<template>
	<main class="login-page" :data-theme="ui.theme">
		<LoginBackground />

		<div class="login-preferences">
			<label class="login-language">
				<Languages :size="16" aria-hidden="true" />
				<select :value="locale" :aria-label="t('common.language')" @change="changeLocale">
					<option v-for="option in LOCALES" :key="option.value" :value="option.value">{{ option.label }}</option>
				</select>
				<ChevronDown :size="12" aria-hidden="true" />
			</label>
			<button type="button" class="login-theme-toggle" :aria-label="themeLabel" :title="themeLabel" @click="ui.toggleTheme()">
				<span class="login-theme-knob" aria-hidden="true" />
				<Sun :size="16" class="login-theme-sun" aria-hidden="true" />
				<Moon :size="16" class="login-theme-moon" aria-hidden="true" />
			</button>
		</div>

		<section class="login-stage" aria-labelledby="login-title">
			<div class="login-panel">
				<form class="login-form" :aria-busy="busy" @submit.prevent="submit">
					<div class="login-form-line" aria-hidden="true" />

					<header class="login-brand-row">
						<div class="login-brand">
							<span class="login-brand-mark" aria-hidden="true">π</span>
							<span class="login-brand-copy">
								<strong>Pi Gateway</strong>
								<span>ADMIN REALM</span>
							</span>
						</div>
						<span class="login-realm" :title="copy.admin">
							<ShieldCheck :size="18" role="img" :aria-label="copy.admin" />
						</span>
					</header>

					<div class="login-heading">
						<h1 id="login-title">{{ copy.title }}</h1>
						<p>{{ copy.welcome }}</p>
					</div>

					<div class="login-fields">
						<div class="login-field">
							<label for="username">{{ t('login.username') }}</label>
							<div class="login-input-wrap" :class="{ 'has-error': error }">
								<Mail :size="17" class="login-input-icon" aria-hidden="true" />
								<input
									id="username"
									v-model="username"
									name="username"
									type="text"
									autocomplete="username"
									autocapitalize="none"
									:spellcheck="false"
									:placeholder="copy.usernamePlaceholder"
									:disabled="busy"
									:aria-invalid="Boolean(error)"
									:aria-describedby="error ? 'login-error' : undefined"
									required
								/>
							</div>
						</div>

						<div class="login-field">
							<label for="password">{{ t('login.password') }}</label>
							<div class="login-input-wrap" :class="{ 'has-error': error }">
								<KeyRound :size="17" class="login-input-icon" aria-hidden="true" />
								<input
									id="password"
									v-model="password"
									name="password"
									:type="showPassword ? 'text' : 'password'"
									autocomplete="current-password"
									:placeholder="copy.passwordPlaceholder"
									:disabled="busy"
									:aria-invalid="Boolean(error)"
									:aria-describedby="error ? 'login-error' : undefined"
									required
								/>
								<button
									type="button"
									class="login-password-toggle"
									:aria-label="showPassword ? copy.hidePassword : copy.showPassword"
									:title="showPassword ? copy.hidePassword : copy.showPassword"
									:aria-pressed="showPassword"
									aria-controls="password"
									@mousedown.prevent
									@click="showPassword = !showPassword"
								>
									<EyeOff v-if="showPassword" :size="16" aria-hidden="true" />
									<Eye v-else :size="16" aria-hidden="true" />
								</button>
							</div>
						</div>

						<p v-if="error" id="login-error" class="login-error" role="alert">
							<AlertCircle :size="16" aria-hidden="true" />
							<span>{{ error }}</span>
						</p>

						<button type="submit" class="login-submit" :disabled="!canSubmit">
							<LoaderCircle v-if="busy" :size="16" class="login-spinner" aria-hidden="true" />
							<span aria-live="polite">{{ busy ? t('login.submitting') : t('login.submit') }}</span>
						</button>
					</div>
				</form>

				<details class="login-help">
					<summary>{{ copy.help }}</summary>
					<p>{{ t('login.hint') }}</p>
				</details>
			</div>
		</section>
	</main>
</template>

<style scoped>
.login-page {
	--login-canvas-start: var(--color-surface);
	--login-canvas-middle: color-mix(in srgb, var(--color-canvas) 70%, var(--color-surface));
	--login-canvas-end: color-mix(in srgb, var(--color-surface-2) 78%, var(--color-accent-soft));
	--login-edge-middle: color-mix(in srgb, var(--color-line) 55%, transparent);
	--login-edge-end: color-mix(in srgb, var(--color-ink-muted) 20%, transparent);
	--login-grid: color-mix(in srgb, var(--color-ink-muted) 5%, transparent);
	--login-striation: color-mix(in srgb, var(--color-ink-muted) 12%, transparent);
	--login-grain: color-mix(in srgb, var(--color-ink-muted) 13%, transparent);
	--login-watermark: color-mix(in srgb, var(--color-ink) 55%, transparent);
	--login-stack-text: color-mix(in srgb, var(--color-ink) 84%, transparent);
	--login-stack-bg: color-mix(in srgb, var(--color-surface) 72%, transparent);
	--login-stack-dot: color-mix(in srgb, var(--color-accent) 62%, transparent);
	--login-stack-opacity: 0.55;
	--login-route-bundle: color-mix(in srgb, var(--color-ink-muted) 20%, transparent);
	--login-route-stream: color-mix(in srgb, var(--color-accent) 17%, transparent);
	--login-route-audit: color-mix(in srgb, var(--color-ink-faint) 18%, transparent);
	--login-semantic: color-mix(in srgb, var(--color-accent) 26%, transparent);
	--login-particle-glow: color-mix(in srgb, var(--color-accent) 62%, transparent);
	--login-cluster-text: color-mix(in srgb, var(--color-ink) 78%, transparent);
	--login-panel-start: color-mix(in srgb, var(--color-surface) 95%, transparent);
	--login-panel-middle: color-mix(in srgb, var(--color-surface) 82%, var(--color-accent-soft));
	--login-panel-end: color-mix(in srgb, var(--color-surface-2) 78%, var(--color-accent-soft));
	--login-panel-shadow: color-mix(in srgb, var(--color-ink) 20%, transparent);
	--login-panel-line: color-mix(in srgb, var(--color-accent) 38%, transparent);
	--login-input-bg: color-mix(in srgb, var(--color-surface-2) 82%, var(--color-surface));
	--login-input-hover: var(--color-surface-3);
	--login-input-active: var(--color-surface);
	--login-disabled-bg: color-mix(in srgb, var(--color-surface-3) 60%, var(--color-surface-2));
	--login-disabled-text: var(--color-ink-faint);

	position: relative;
	isolation: isolate;
	min-height: 100vh;
	min-height: 100dvh;
	overflow: hidden;
	background: var(--color-canvas);
	color: var(--color-ink);
	font-family: var(--font-sans);
}

.login-page[data-theme='dark'] {
	--login-canvas-start: color-mix(in srgb, var(--color-surface) 82%, var(--color-surface-2));
	--login-canvas-middle: color-mix(in srgb, var(--color-canvas) 88%, var(--color-surface));
	--login-canvas-end: color-mix(in srgb, var(--color-canvas) 92%, var(--color-surface-3));
	--login-edge-middle: color-mix(in srgb, var(--color-canvas) 32%, transparent);
	--login-edge-end: color-mix(in srgb, var(--color-canvas) 66%, transparent);
	--login-grid: color-mix(in srgb, var(--color-accent) 5%, transparent);
	--login-striation: color-mix(in srgb, var(--color-accent) 5%, transparent);
	--login-grain: color-mix(in srgb, var(--color-ink-muted) 9%, transparent);
	--login-watermark: color-mix(in srgb, var(--color-accent) 40%, transparent);
	--login-stack-text: color-mix(in srgb, var(--color-ink) 80%, transparent);
	--login-stack-bg: color-mix(in srgb, var(--color-surface) 54%, transparent);
	--login-stack-dot: color-mix(in srgb, var(--color-accent) 72%, transparent);
	--login-stack-opacity: 0.76;
	--login-route-bundle: color-mix(in srgb, var(--color-accent) 17%, transparent);
	--login-route-stream: color-mix(in srgb, var(--color-accent) 14%, transparent);
	--login-route-audit: color-mix(in srgb, var(--color-ink-muted) 12%, transparent);
	--login-semantic: color-mix(in srgb, var(--color-accent) 27%, transparent);
	--login-cluster-text: color-mix(in srgb, var(--color-ink) 66%, transparent);
	--login-panel-start: color-mix(in srgb, var(--color-surface-2) 82%, transparent);
	--login-panel-middle: color-mix(in srgb, var(--color-surface) 86%, transparent);
	--login-panel-end: color-mix(in srgb, var(--color-canvas) 90%, transparent);
	--login-panel-shadow: color-mix(in srgb, var(--color-canvas) 70%, transparent);
	--login-panel-line: color-mix(in srgb, var(--color-accent) 30%, transparent);
	--login-input-bg: color-mix(in srgb, var(--color-canvas) 58%, transparent);
	--login-input-hover: color-mix(in srgb, var(--color-surface-2) 62%, transparent);
	--login-input-active: color-mix(in srgb, var(--color-surface-2) 72%, transparent);
	--login-disabled-bg: var(--color-surface-2);
	--login-disabled-text: color-mix(in srgb, var(--color-ink-faint) 75%, var(--color-surface-2));
}

.login-preferences {
	position: absolute;
	top: 20px;
	right: 20px;
	z-index: 2;
	display: flex;
	align-items: center;
	gap: 12px;
}

.login-language {
	position: relative;
	display: flex;
	height: 36px;
	align-items: center;
	gap: 7px;
	border-radius: 7px;
	padding: 0 9px;
	color: var(--color-ink-muted);
	background: color-mix(in srgb, var(--color-surface-2) 82%, transparent);
}

.login-language select {
	min-width: 80px;
	height: 100%;
	appearance: none;
	border: 0;
	padding: 0 16px 0 0;
	background: transparent;
	color: inherit;
	font-size: 12px;
	outline: none;
	cursor: pointer;
}

.login-language > svg:last-child {
	position: absolute;
	right: 9px;
	pointer-events: none;
}

.login-language:focus-within,
.login-theme-toggle:focus-visible,
.login-password-toggle:focus-visible,
.login-submit:focus-visible,
.login-help summary:focus-visible {
	outline: 2px solid var(--color-accent);
	outline-offset: 3px;
}

.login-theme-toggle {
	position: relative;
	display: inline-grid;
	width: 80px;
	height: 36px;
	flex-shrink: 0;
	grid-template-columns: repeat(2, 1fr);
	place-items: center;
	border: 0;
	border-radius: 20px;
	padding: 0;
	background: var(--color-surface-3);
	color: var(--color-ink-muted);
	cursor: pointer;
}

.login-theme-toggle svg {
	position: relative;
	z-index: 1;
}

.login-theme-knob {
	position: absolute;
	top: 4px;
	left: 6px;
	width: 28px;
	height: 28px;
	border-radius: 50%;
	background: var(--color-surface);
	box-shadow: 0 1px 4px color-mix(in srgb, var(--color-ink) 8%, transparent);
	transition: transform 200ms ease;
}

.login-page[data-theme='dark'] .login-theme-knob {
	transform: translateX(40px);
}

.login-theme-sun,
.login-page[data-theme='dark'] .login-theme-moon {
	color: var(--color-ink);
}

.login-page[data-theme='dark'] .login-theme-sun {
	color: var(--color-ink-muted);
}

.login-stage {
	display: grid;
	min-height: 100vh;
	min-height: 100dvh;
	align-items: center;
	justify-items: center;
	padding: max(76px, 5dvh) 20px max(32px, 5dvh);
}

.login-panel {
	width: min(440px, 100%);
	min-width: 0;
}

.login-form {
	position: relative;
	display: grid;
	gap: 32px;
	width: 100%;
	border: 0;
	border-radius: 8px;
	padding: 40px 30px 24px;
	background:
		linear-gradient(118deg, var(--login-panel-start), var(--login-panel-middle) 56%, var(--login-panel-end)),
		var(--login-panel-middle);
	box-shadow: 0 18px 38px -20px var(--login-panel-shadow);
	backdrop-filter: blur(18px) saturate(1.08);
	-webkit-backdrop-filter: blur(18px) saturate(1.08);
}

.login-form-line {
	position: absolute;
	top: 0;
	left: 22px;
	width: calc(100% - 44px);
	height: 2px;
	background: linear-gradient(90deg, transparent, var(--login-panel-line), transparent);
	opacity: 0.42;
	pointer-events: none;
}

.login-brand-row,
.login-brand {
	display: flex;
	min-width: 0;
	align-items: center;
}

.login-brand-row {
	justify-content: space-between;
	gap: 18px;
}

.login-brand {
	gap: 8px;
}

.login-brand-mark {
	display: inline-flex;
	width: 42px;
	height: 42px;
	flex-shrink: 0;
	align-items: center;
	justify-content: center;
	border-radius: 9px;
	background: var(--color-brand);
	color: #fff;
	font-size: 30px;
	font-weight: 600;
	line-height: 1;
}

.login-brand-copy {
	display: grid;
	min-width: 0;
	gap: 4px;
}

.login-brand-copy strong {
	font-size: 17px;
	font-weight: 600;
	line-height: 1.3;
}

.login-brand-copy > span {
	margin-left: 2px;
	color: var(--color-ink-muted);
	font-family: var(--font-mono);
	font-size: 10px;
	font-weight: 400;
	line-height: 1.2;
}

.login-realm {
	display: grid;
	width: 36px;
	height: 34px;
	flex-shrink: 0;
	place-items: center;
	border-radius: 6px;
	background: var(--login-input-bg);
	color: var(--color-accent);
}

.login-heading {
	display: grid;
	min-height: 72px;
	gap: 16px;
	align-content: start;
}

.login-heading h1 {
	margin: 0;
	font-size: 34px;
	font-weight: 600;
	letter-spacing: -0.8px;
	line-height: 1.02;
}

.login-heading p {
	margin: 0 0 0 -8px;
	color: var(--color-ink-muted);
	font-size: 14px;
	font-weight: 400;
	line-height: 1.45;
}

.login-fields {
	display: grid;
	min-width: 0;
	gap: 12px;
}

.login-field {
	display: grid;
	min-width: 0;
	gap: 8px;
}

.login-field > label {
	color: var(--color-ink);
	font-size: 14px;
	font-weight: 600;
	line-height: 1.1;
}

.login-input-wrap {
	display: flex;
	min-width: 0;
	height: 43px;
	align-items: center;
	gap: 9px;
	border-radius: 6px;
	padding: 0 10px;
	background: var(--login-input-bg);
	transition: background 150ms ease, box-shadow 150ms ease;
}

.login-input-wrap:hover {
	background: var(--login-input-hover);
}

.login-input-wrap:focus-within {
	background: var(--login-input-active);
	box-shadow: 0 0 0 2px color-mix(in srgb, var(--color-accent) 60%, transparent);
}

.login-input-icon {
	flex-shrink: 0;
	color: var(--color-accent);
}

.login-input-wrap input {
	width: 100%;
	min-width: 0;
	height: 100%;
	flex: 1;
	border: 0;
	border-radius: 0;
	padding: 0;
	outline: 0;
	background: transparent;
	color: var(--color-ink);
	font-size: 14px;
	line-height: 1.4;
}

.login-input-wrap input::placeholder {
	color: var(--color-ink-faint);
	opacity: 1;
}

.login-input-wrap input:disabled {
	cursor: wait;
}

.login-password-toggle {
	display: grid;
	width: 30px;
	height: 30px;
	flex-shrink: 0;
	place-items: center;
	border: 0;
	border-radius: 6px;
	padding: 0;
	background: transparent;
	color: var(--color-ink-faint);
	cursor: pointer;
	transition: color 150ms ease, background 150ms ease;
}

.login-password-toggle:hover {
	background: var(--color-surface-3);
	color: var(--color-ink);
}

.login-input-wrap.has-error {
	box-shadow: 0 0 0 1px var(--color-danger);
}

.login-input-wrap.has-error:focus-within {
	box-shadow: 0 0 0 2px color-mix(in srgb, var(--color-danger) 70%, transparent);
}

.login-error {
	display: flex;
	align-items: flex-start;
	gap: 8px;
	margin: 0;
	border-radius: 6px;
	padding: 10px;
	background: var(--color-danger-soft);
	color: var(--color-danger);
	font-size: 12px;
	line-height: 1.5;
	overflow-wrap: anywhere;
}

.login-error svg {
	flex-shrink: 0;
	margin-top: 1px;
}

.login-submit {
	display: inline-flex;
	width: 100%;
	height: 44px;
	align-items: center;
	justify-content: center;
	gap: 8px;
	border: 0;
	border-radius: 6px;
	padding: 0 14px;
	background: var(--color-accent-dim);
	box-shadow: 0 14px 24px -18px color-mix(in srgb, var(--color-accent-dim) 30%, transparent);
	color: #fff;
	font-size: 14px;
	font-weight: 500;
	cursor: pointer;
	transition: background 150ms ease, box-shadow 150ms ease;
}

.login-submit:hover:not(:disabled) {
	background: var(--color-button-hover);
}

.login-submit:active:not(:disabled) {
	background: color-mix(in srgb, var(--color-accent-dim) 90%, var(--color-ink));
}

.login-submit:disabled {
	background: var(--login-disabled-bg);
	box-shadow: none;
	color: var(--login-disabled-text);
	cursor: not-allowed;
}

.login-spinner {
	animation: login-spin 900ms linear infinite;
}

.login-help {
	margin: 16px 4px 0;
	color: var(--color-ink-muted);
	font-size: 12px;
	line-height: 1.65;
}

.login-help summary {
	width: fit-content;
	border-radius: 3px;
	color: var(--color-ink-faint);
	cursor: pointer;
}

.login-help summary:hover {
	color: var(--color-ink);
}

.login-help p {
	margin: 8px 0 0;
}

@keyframes login-spin {
	to { transform: rotate(360deg); }
}

@media (min-width: 980px) {
	.login-stage {
		justify-items: end;
		padding-right: clamp(48px, 17.3vw, 332px);
	}
}

@media (max-width: 560px) {
	.login-preferences {
		top: 16px;
		right: 16px;
		gap: 8px;
	}

	.login-stage {
		padding: 80px 18px 28px;
	}

	.login-form {
		gap: 24px;
		padding: 26px 22px 22px;
	}

	.login-brand-row {
		gap: 14px;
	}

	.login-heading h1 {
		font-size: 30px;
	}

	.login-heading p {
		font-size: 13px;
	}
}

@media (max-width: 380px) {
	.login-form {
		padding-right: 18px;
		padding-left: 18px;
	}

	.login-heading h1 {
		font-size: 28px;
	}
}

@media (prefers-reduced-motion: reduce) {
	.login-theme-knob,
	.login-input-wrap,
	.login-password-toggle,
	.login-submit {
		transition: none;
	}

	.login-spinner {
		animation: none;
	}
}
</style>
