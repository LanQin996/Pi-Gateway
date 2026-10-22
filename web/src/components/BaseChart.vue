<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { init, use, type EChartsCoreOption, type EChartsType } from 'echarts/core'
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent, AriaComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

use([BarChart, LineChart, PieChart, GridComponent, LegendComponent, TooltipComponent, AriaComponent, CanvasRenderer])
const props = withDefaults(defineProps<{ option: EChartsCoreOption; label: string; height?: number }>(), { height: 220 })
const container = ref<HTMLDivElement>()
let chart: EChartsType | undefined
let observer: ResizeObserver | undefined
let resizeFrame = 0
let disposed = false
const motion = window.matchMedia('(prefers-reduced-motion: reduce)')
function render() {
	chart?.setOption({ ...props.option, ...(motion.matches ? { animation: false } : {}) }, { notMerge: true })
}
function resize() {
	cancelAnimationFrame(resizeFrame)
	resizeFrame = requestAnimationFrame(() => chart?.resize())
}
onMounted(() => {
	if (!container.value) { return }
	chart = init(container.value, undefined, { renderer: 'canvas' })
	render()
	observer = new ResizeObserver(resize)
	observer.observe(container.value)
	motion.addEventListener('change', render)
	void document.fonts?.ready.then(() => { if (!disposed) { render(); resize() } })
})
watch(() => props.option, render, { deep: true })
watch(() => props.height, resize, { flush: 'post' })
onBeforeUnmount(() => { disposed = true; cancelAnimationFrame(resizeFrame); observer?.disconnect(); motion.removeEventListener('change', render); chart?.dispose(); chart = undefined })
</script>

<template>
	<figure class="m-0 min-w-0"><div ref="container" aria-hidden="true" class="base-chart" :style="{ height: `${height}px` }" /><figcaption class="sr-only">{{ label }}</figcaption></figure>
</template>

<style scoped>
.base-chart { position: relative; min-width: 0; width: 100%; overflow: hidden; }
</style>
