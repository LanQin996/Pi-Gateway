import type {CaptureRuleTrace} from '../api/types'
import {ruleLabel} from './ruleLabels'

/** Translate implementation-owned names, never replace a user-defined rule name. */
export function traceRuleName(trace: CaptureRuleTrace, locale: string): string {
    const zh = locale === 'zh-CN'
    if (trace.source_kind === 'gateway') {
        const prefix = zh ? '网关内置处理' : 'Gateway processing'
        const name = gatewayTraceName(trace, zh)
        return name ? `${prefix}: ${name}` : prefix
    }
    if (trace.source === 'legacy' && trace.rule_name) return ruleLabel('action', trace.rule_name, locale)
    return trace.rule_name || (zh ? '未命名规则' : 'Unnamed rule')
}

function gatewayTraceName(trace: CaptureRuleTrace, zh: boolean): string | undefined {
    if (trace.rule_id === 'gateway:normalize') return zh ? '请求规范化' : 'Request normalization'
    if (trace.rule_id === 'gateway:pi_shape') return zh ? '协议结构约束' : 'Protocol invariants'
    return trace.rule_name
}
