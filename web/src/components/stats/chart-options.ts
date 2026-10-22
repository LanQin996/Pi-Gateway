import {computed, onBeforeUnmount, onMounted, ref} from 'vue'
import type {EChartsCoreOption} from 'echarts/core'
import type {StatsBucket} from '../../api/types'
import {metric, money, percent} from '../../api/stats-format'
import {formatDateTime, useI18n} from '../../i18n'
import {useUiStore} from '../../stores/ui'

export interface StatsSeries {
    name: string
    color: string
    values: Array<number | null>
    unit?: 'number' | 'duration' | 'throughput' | 'money' | 'percent'
    area?: boolean
    bar?: boolean
}

    function displayValue(value: number | null | undefined, unit: StatsSeries['unit']) {
        if (value == null || !Number.isFinite(value)) { return '—' }
        if (unit === 'duration') { return `${metric(value, 2)} ms` }
        if (unit === 'throughput') { return `${metric(value, 2)} tok/s` }
        if (unit === 'money') { return money(value) }
        if (unit === 'percent') { return percent(value) }
        return metric(value)
    }

    function alpha(color: string, opacity: number) {
        // Resolve the real semantic token through the browser, not a guessed HEX palette.
        const canvas = document.createElement('canvas')
        const context = canvas.getContext('2d')
        if (!context) { return color }
        context.fillStyle = color
        context.fillRect(0, 0, 1, 1)
        const data = context.getImageData(0, 0, 1, 1).data
        return `rgba(${data[0]},${data[1]},${data[2]},${opacity})`
    }

    function escape(value: string) {
        return value.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;').replaceAll("'", '&#39;')
    }

// Coordinates, typography and tooltip spacing follow usage/utils/chart.ts.
// Every value remains an API sample: nulls are gaps, never invented zeroes.
export function useStatsChart() {
    const {locale} = useI18n()
    const ui = useUiStore()
    const themeVersion = ref(0)
    let themeObserver: MutationObserver | undefined
    onMounted(() => {
        themeVersion.value++
        themeObserver = new MutationObserver(() => {
            themeVersion.value++
        })
        themeObserver.observe(document.documentElement, {
            attributes: true,
            attributeFilter: ['data-theme', 'class', 'style']
        })
    })
    onBeforeUnmount(() => themeObserver?.disconnect())
    const themeSnapshot = computed(() => ({theme: ui.theme, revision: themeVersion.value, css: getComputedStyle(document.documentElement)}))
    const palette = computed(() => {
        const css = themeSnapshot.value.css
        const token = (name: string, reference: string, fallback = 'ink') => css.getPropertyValue(`--color-${name}`).trim() || css.getPropertyValue(`--cp-color-${reference}`).trim() || css.getPropertyValue(`--color-${fallback}`).trim()
        return {
            blue: token('chart-blue', 'blue-solid', 'primary-seed'),
            green: token('chart-green', 'green-solid', 'success'),
            orange: token('chart-orange', 'orange-solid', 'warn'),
            cyan: token('chart-cyan', 'cyan-solid', 'info'),
            text: token('ink', 'text'),
            secondary: token('ink-muted', 'text-secondary'),
            muted: token('ink-faint', 'text-quaternary'),
            surface: token('surface-elevated', 'bg-elevated', 'surface'),
            line: token('line', 'split'),
            pointer: token('line-strong', 'border'),
            font: css.getPropertyValue('--font-sans').trim(),
            mono: css.getPropertyValue('--font-mono').trim(),
        }
    })

    function compact(value: number) {
        return new Intl.NumberFormat(locale.value, {notation: 'compact', maximumFractionDigits: 1}).format(value)
    }

    function axisValue(value: number, unit: StatsSeries['unit']) {
        if (unit === 'duration') { return value >= 1000 ? `${metric(value / 1000, 1)}s` : `${metric(value)}ms` }
        if (unit === 'money') { return `$${metric(value, value < 1 ? 3 : 2)}` }
        if (unit === 'percent') { return percent(value) }
        if (unit === 'throughput') { return `${compact(value)}/s` }
        return compact(value)
    }

    function option(buckets: StatsBucket[], series: StatsSeries[]): EChartsCoreOption {
        const theme = palette.value
        const longRange = buckets.length > 1 && buckets.at(-1)!.bucket_start - buckets[0]!.bucket_start > 172800000
        const dateFormat = new Intl.DateTimeFormat(locale.value, longRange ? {
            month: '2-digit',
            day: '2-digit'
        } : {hour: '2-digit', minute: '2-digit', hour12: false})
        return {
            animationDuration: 240,
            animationDurationUpdate: 220,
            animationEasing: 'cubicOut',
            textStyle: {color: theme.secondary, fontFamily: theme.font},
            aria: {enabled: true},
            grid: {left: 0, right: 0, top: 40, bottom: 0, outerBoundsMode: 'same', outerBoundsContain: 'axisLabel'},
            legend: {
                top: 0,
                right: 4,
                itemWidth: 8,
                itemHeight: 8,
                icon: 'circle',
                data: series.map(item => item.name),
                textStyle: {color: theme.secondary, fontSize: 11, fontFamily: theme.font, fontWeight: 650}
            },
            tooltip: {
                trigger: 'axis',
                confine: true,
                renderMode: 'html',
                backgroundColor: theme.surface,
                borderWidth: 0,
                padding: [10, 14],
                extraCssText: 'border-radius:12px;box-shadow:var(--shadow-overlay);',
                textStyle: {color: theme.text, fontSize: 12, fontFamily: theme.font, fontWeight: 650},
                axisPointer: {type: 'line', lineStyle: {color: theme.pointer, type: 'dashed', width: 1}},
                formatter: (params: unknown) => {
                    const first: unknown = Array.isArray(params) ? params[0] : params
                    if (typeof first !== 'object' || first === null || !('dataIndex' in first) || typeof first.dataIndex !== 'number') { return '' }
                    const index = first.dataIndex
                    const bucket = buckets[index]
                    if (!bucket) { return '' }
                    const title = `<div style="margin:0 0 7px;padding:0 0 7px;border-bottom:1px solid ${escape(theme.line)};font-family:var(--font-mono);font-size:11px;font-weight:750;line-height:1.2">${escape(formatDateTime(bucket.bucket_start))}</div>`
                    const lines = series.map(item => `<span style="display:inline-block;width:7px;height:7px;margin-right:6px;border-radius:999px;background:${escape(item.color)}"></span>${escape(item.name)}: ${escape(displayValue(item.values[index], item.unit))}`)
                    return `${title}<div style="line-height:1.55">${lines.join('<br/>')}</div>`
                },
            },
            xAxis: {
                type: 'category',
                boundaryGap: series.some(item => item.bar),
                data: buckets.map(bucket => bucket.bucket_start),
                axisLine: {show: false},
                axisTick: {show: false},
                axisLabel: {
                    color: theme.muted,
                    fontSize: 10,
                    fontFamily: theme.mono,
                    hideOverlap: true,
                    formatter: (value: string) => dateFormat.format(Number(value))
                }
            },
            yAxis: {
                type: 'value',
                min: 0,
                max: series[0]?.unit === 'percent' ? 1 : undefined,
                splitNumber: 3,
                axisLine: {show: false},
                axisTick: {show: false},
                axisLabel: {
                    color: theme.muted,
                    fontSize: 10,
                    fontFamily: theme.mono,
                    formatter: (value: number) => axisValue(value, series[0]?.unit)
                },
                splitLine: {lineStyle: {color: theme.line, width: 1}}
            },
            series: series.map(item => {
                const sampleCount = item.values.filter(value => value != null).length
                return {
                    name: item.name,
                    type: item.bar ? 'bar' : 'line',
                    data: item.values,
                    connectNulls: false,
                    smooth: true,
                    showSymbol: sampleCount > 0 && sampleCount <= 12,
                    showAllSymbol: sampleCount <= 12,
                    symbol: 'circle',
                    symbolSize: sampleCount === 1 ? 7 : 5,
                    barMaxWidth: 24,
                    lineStyle: {color: item.color, width: 2.2},
                    itemStyle: {color: item.color, ...(item.bar ? {opacity: .72} : {})},
                    areaStyle: item.area ? {
                        color: {
                            type: 'linear',
                            x: 0,
                            y: 0,
                            x2: 0,
                            y2: 1,
                            colorStops: [{offset: 0, color: alpha(item.color, .188)}, {
                                offset: 1,
                                color: alpha(item.color, .031)
                            }]
                        }
                    } : undefined
                }
            }),
        }
    }

    return {palette, option}
}
