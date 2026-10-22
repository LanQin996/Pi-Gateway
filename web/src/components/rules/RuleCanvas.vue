<script setup lang="ts">
import {useId, computed, nextTick, ref, onBeforeUnmount, toRaw} from 'vue'
import { Plus, Search, ZoomIn, ZoomOut, Maximize, LayoutGrid, Undo2, Redo2, Copy, Trash2 } from 'lucide-vue-next'
import {VueFlow, useVueFlow, MarkerType, type Connection, type NodeChange, type EdgeChange} from '@vue-flow/core'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import type {Rule, RuleAction, RuleCondition, RuleFieldError, RuleSchema} from '../../api/rules'
import {useI18n} from '../../i18n'
import {clone, RuleInputError} from '../../utils/ruleEditor'
import {editorId} from '../../utils/editorId'
import {localHelp} from '../../utils/ruleSchema'
import {ruleLabel, ruleOptionLabel} from '../../utils/ruleLabels'
import {ruleNodeWidth, visibleWorkflow, removeGraphNodes, duplicateGraphNodes, readGraphInput, writeGraphInput, replaceGraphNode, addGraphModule, connectGraph, graphNodeValue, graphPathMap, type RuleGraphValue, type RuleGraph, type RuleGraphNode, type RuleGraphEdge, type RuleGraphAddOptions, ruleNodeColumnGap, graphValueInputs as importValueInputs, groupGraphNodes, groupedWorkflow, graphGroupPorts, resolveGroupConnection, actionFlowPorts, graphConditionInputs, type RuleGraphGroup} from '../../utils/ruleGraph'
import RuleCanvasNode from './RuleGraphNode.vue'
import RuleGroupNode from './RuleGroupNode.vue'
import RuleNodeSearch from './RuleNodeSearch.vue'

const formId = useId()
const props = defineProps<{modelValue: RuleGraph; schema: RuleSchema; diagnostics?: Record<string, string>; errors?: RuleFieldError[]; sample?: unknown; canUndo?: boolean; canRedo?: boolean}>()
const emit = defineEmits<{
    'update:modelValue': [value: RuleGraph]
    undo: []
    redo: []
    select: [id: string]
    edit: [id: string, value: RuleGraphNode['data']]
    'edit-start': [id: string]
    'edit-end': [id: string]
}>()
const {t, locale} = useI18n()
// Graph mutations replace the whole snapshot; avoid deep reactive traversal of hundreds of operands.
const graphModel=computed(()=>toRaw(props.modelValue))
const flowId = editorId()
const flow = useVueFlow(flowId)
const canvasElement = ref<HTMLElement>()
const search = ref(''), libraryOpen = ref(false), connectionError = ref('')
const activeGroup=ref<string>()
const portManager=ref<string>()

const pathMap = computed(() => graphPathMap(graphModel.value))
const root = computed(() => graphModel.value.nodes.find(n => n.kind === 'rule')!)
const selected = computed(() => graphModel.value.nodes.find(n => graphModel.value.selected.includes(n.id)))
const phase = computed(() => (root.value.data as Rule).phase)
const conditions = computed(() => props.schema.conditions.filter(c => (ruleLabel('condition', c.id, locale.value) + c.id + localHelp(c.description, locale.value)).toLowerCase().includes(search.value.toLowerCase())))
const actions = computed(() => props.schema.actions.filter(c => c.id !== 'sequence' && (!c.phases?.length || c.phases.includes(phase.value)) && (ruleLabel('action', c.id, locale.value) + c.id + localHelp(c.description, locale.value)).toLowerCase().includes(search.value.toLowerCase())))
const expandedPredicates = ref<string[]>([])
const expandedValues = ref<string[]>([])
const hiddenValues = computed(() => {
 const visible=new Set(graphModel.value.nodes.filter(node=>node.kind!=='value').map(node=>node.id))
 const open=(id:string)=>{ if(!expandedValues.value.includes(id)){ return; }for(const edge of graphModel.value.edges.filter(edge=>edge.target===id && edge.sourceHandle==='value-out')){visible.add(edge.source);open(edge.source)} }
 graphModel.value.nodes.filter(node=>node.kind!=='value').forEach(node=>open(node.id))
 return new Set(graphModel.value.nodes.filter(node=>node.kind==='value'&&!visible.has(node.id)).map(node=>node.id))
})
const hiddenPredicates = computed(() => {
    const hidden = new Set<string>()
    const collect = (id: string) => { if (hidden.has(id)) { return; } hidden.add(id); graphModel.value.edges.filter(edge => edge.target === id && ['boolean-out','value-out'].includes(edge.sourceHandle)).forEach(edge => collect(edge.source)) }
    graphModel.value.edges.filter(edge => edge.targetHandle.startsWith('predicate:') && (graphModel.value.nodes.find(node=>node.id===edge.target)?.data as RuleAction).type==='array_filter' && !expandedPredicates.value.includes(edge.target)).forEach(edge => collect(edge.source))
    return hidden
})
const completeWorkflow = computed(() => visibleWorkflow(graphModel.value))
const workflow = computed(() => groupedWorkflow(graphModel.value,activeGroup.value))
function nodeLabel(node: RuleGraphNode) {
 if(node.kind==='value') { const value=node.data as RuleGraphValue;return value.mode==='computed'?ruleOptionLabel((value.value as {$expr:{op:string}}).$expr.op,locale.value):t(`rules.canvas.value.${value.mode}`) }
 return (() => {
    if (node.kind==='rule') {
        return t('rules.canvas.rule')
    }
    return ruleLabel(node.kind,node.kind==='action'?(node.data as RuleAction).type:(node.data as RuleCondition).op,locale.value)
})()
}
const moduleNodes = computed(() => workflow.value.nodes.filter(node => !hiddenPredicates.value.has(node.id) && !hiddenValues.value.has(node.id)).map(node => ({
    id: node.id, type: 'rule-module', position: node.position, dragHandle: '.rule-node-drag-handle',
    data: {...node, data: graphNodeValue(graphModel.value, node.id) ?? node.data, path: pathMap.value[node.id] ?? node.path, schema: props.schema, diagnostics: props.diagnostics, errors: props.errors, sample: props.sample, phase: phase.value, valueConnections:Object.fromEntries(graphModel.value.edges.filter(edge=>edge.target===node.id&&edge.sourceHandle==='value-out').map(edge=>[edge.targetHandle.slice(6),edge.source])), conditionConnections:node.kind==='action' && (node.data as RuleAction).type!=='array_filter' ? Object.fromEntries(graphModel.value.edges.filter(edge=>edge.target===node.id&&edge.targetHandle.startsWith('predicate:')).map(edge=>[edge.targetHandle.slice(10),edge.source])) : {}},
    selected: graphModel.value.selected.includes(node.id), deletable: node.kind !== 'rule',
    ariaLabel: nodeLabel(node),
})))
const nodes=computed(()=>[...moduleNodes.value,...workflow.value.groups.map(group=>({id:group.id,type:'rule-group',position:group.position,dragHandle:'.rule-node-drag-handle',data:{group,ports:graphGroupPorts(graphModel.value,group),labels:Object.fromEntries(graphGroupPorts(graphModel.value,group).map(port=>[port.id,portLabel(port)]))},selected:graphModel.value.selected.includes(group.id),deletable:false,ariaLabel:group.name}))])
function portLabel(port:RuleGraphGroup['ports'][number]) {
 const node=graphModel.value.nodes.find(node=>node.id===port.node)
 const handle=(() => {
    if (port.handle==='action-in'||port.handle==='action-out') {
        return t('rules.canvas.actionPort')
    }
    if (port.handle==='boolean-out'||port.handle==='boolean-in') {
        return t('rules.canvas.booleanPort')
    }
    if (port.handle==='value-out'||port.handle.startsWith('value:')) {
        return t('rules.canvas.valuePort')
    }
    if (port.handle.startsWith('flow:')) {
        return t(`rules.canvas.flow.${port.handle.slice(5)}`)
    }
    return port.handle
})()
 return `${node?nodeLabel(node):''} · ${handle}`
}
function updateGroup(id:string,update:(group:RuleGraphGroup)=>void) {const graph=clone(graphModel.value),group=graph.groups?.find(group=>group.id===id);if(group){update(group);publish(graph)}}
async function settleView() {await nextTick();await new Promise<void>(resolve=>requestAnimationFrame(()=>requestAnimationFrame(()=>resolve())));await fit()}
async function enterGroup(id:string) {activeGroup.value=id;await settleView()}
async function leaveGroup() {activeGroup.value=undefined;await settleView()}
async function createGroup() {try{const next=groupGraphNodes(graphModel.value,graphModel.value.selected,locale.value==='zh-CN'?'新组':'New group');next.selected=[];publish(next);await nextTick();await nextTick();await new Promise<void>(resolve=>requestAnimationFrame(()=>requestAnimationFrame(()=>resolve())));separateOverlaps();await fit()}catch(error){reportConnectionError(error)}}
function ungroup(id:string) {publish({...graphModel.value,groups:graphModel.value.groups?.filter(group=>group.id!==id)});if(activeGroup.value===id){ activeGroup.value=undefined }}
const managedGroup=computed(()=>graphModel.value.groups?.find(group=>group.id===portManager.value))
const managedPorts=computed(()=>managedGroup.value?graphGroupPorts(graphModel.value,managedGroup.value):[])
const portCandidate=ref('')
const portCandidates=computed(()=>(() => {
    if (managedGroup.value) {
        return graphModel.value.nodes.filter(node=>managedGroup.value!.nodes.includes(node.id)).flatMap(node=>{
 const candidates:{node:string;handle:string;direction:'input'|'output';name:string}[]=[]
 if(node.kind==='action'){ candidates.push({node:node.id,handle:'action-in',direction:'input',name:nodeLabel(node)},{node:node.id,handle:'action-out',direction:'output',name:nodeLabel(node)}) }
 if(node.kind==='action'){ for(const port of actionFlowPorts(node.data as RuleAction))candidates.push({node:node.id,handle:`flow:${port}`,direction:'output',name:nodeLabel(node)}) }
 if(node.kind==='condition'&&['all','any','not'].includes((node.data as RuleCondition).op)){ candidates.push({node:node.id,handle:'boolean-in',direction:'input',name:nodeLabel(node)}) }
 if(node.kind==='action'){ for(const port of graphConditionInputs(node.data as RuleAction))candidates.push({node:node.id,handle:`predicate:${port}`,direction:'input',name:nodeLabel(node)}) }
 if(node.kind==='condition'){ candidates.push({node:node.id,handle:'boolean-out',direction:'output',name:nodeLabel(node)}) }
 if(node.kind==='value'){ candidates.push({node:node.id,handle:'value-out',direction:'output',name:nodeLabel(node)}) }
 for(const input of importValueInputs(node))candidates.push({node:node.id,handle:`value:${input.path}`,direction:'input',name:`${nodeLabel(node)} · ${input.index===undefined?input.label:input.index+1}`})
 return candidates
}).filter(candidate=>!managedPorts.value.some(port=>port.node===candidate.node&&port.handle===candidate.handle&&port.direction===candidate.direction))
    }
    return []
})())
function registerPort() {const candidate=portCandidates.value[Number(portCandidate.value)];if(portCandidate.value===''||!candidate||!managedGroup.value){ return; }updateGroup(managedGroup.value.id,group=>{if(!group.ports.some(port=>port.node===candidate.node&&port.handle===candidate.handle&&port.direction===candidate.direction)){ group.ports.push({...candidate,id:editorId()}) }});portCandidate.value=''}
function renamePort(id:string,portId:string,name:string) {const group=graphModel.value.groups?.find(group=>group.id===id),port=group&&graphGroupPorts(graphModel.value,group).find(port=>port.id===portId);if(port){ updateGroup(id,group=>{group.ports=group.ports.filter(item=>item.id!==portId);group.ports.push({...port,name})}) }}
const edges = computed(() => workflow.value.edges.filter(edge => !hiddenPredicates.value.has(edge.source) && !hiddenPredicates.value.has(edge.target) && !hiddenValues.value.has(edge.source) && !hiddenValues.value.has(edge.target)).map(edge => ({...edge, markerEnd: MarkerType.ArrowClosed, style: {stroke: (() => {
    if (edge.sourceHandle === 'value-out') {
        return '#22a68c'
    }
    if (edge.sourceHandle === 'boolean-out') {
        return 'var(--color-accent)'
    }
    return '#d97706'
})(), strokeWidth: 2}, label: (() => {
    if (edge.sourceHandle === 'boolean-out') {
        return String(edge.order + 1)
    }
    if (edge.sourceHandle.startsWith('flow:functions/')) {
        return edge.sourceHandle.slice(15).replaceAll('~1', '/').replaceAll('~0', '~')
    }
    if (edge.sourceHandle.startsWith('flow:')) {
        return t(`rules.canvas.flow.${edge.sourceHandle.slice(5)}`)
    }
    return undefined
})()})))
function publish(graph: RuleGraph) {
    if (activeGroup.value) {
        const group=graph.groups?.find(group=>group.id===activeGroup.value)
        const added=graph.nodes.filter(node=>node.kind!=='rule'&&!graphModel.value.nodes.some(old=>old.id===node.id))
        if(group&&added.length){ group.nodes=[...new Set([...group.nodes,...added.map(node=>node.id)])] }
    }
    emit('update:modelValue', graph)
}
function select(id: string, additive=false) { publish({...graphModel.value, selected: additive ? [...new Set([...graphModel.value.selected,id])] : [id]}); emit('select', id) }
function applyGraphChange(graph: RuleGraph, change: NodeChange): RuleGraph {
    if (change.type === 'position' && change.position) {
        const position = change.position
        return {...graph, nodes: graph.nodes.map(node => node.id === change.id ? {...node, position} : node),
            groups: graph.groups?.map(group => group.id === change.id ? {...group, position} : group)}
    }
    if (change.type === 'select') {
        const selected = change.selected ? [...new Set([...graph.selected, change.id])] : graph.selected.filter(id => id !== change.id)
        return {...graph, selected}
    }
    if (change.type === 'remove' && graph.nodes.find(node => node.id === change.id)?.kind !== 'rule') {
        return removeGraphNodes(graph, [change.id])
    }
    return graph
}
function changes(changes: NodeChange[]) {
    flow.applyNodeChanges(changes)
    const graph = changes.reduce(applyGraphChange, graphModel.value)
    if (graph !== graphModel.value) { publish(graph) }
    if (changes.some(change => change.type === 'dimensions')) { scheduleSeparation() }
}
function edgeChanges(changes: EdgeChange[]) {
    flow.applyEdgeChanges(changes)
    const removed = changes.filter(c => c.type === 'remove').map(c => c.id)
    if (removed.length) { publish({...graphModel.value, edges: graphModel.value.edges.filter(e => !removed.includes(e.id))}) }
}
function connection(value: Connection): Omit<RuleGraphEdge, 'id'|'order'> {
    return resolveGroupConnection(graphModel.value,value)
}
function validConnection(value: Connection): boolean {
    if (workflow.value.edges.some(edge => edge.source === value.source && edge.target === value.target && edge.sourceHandle === value.sourceHandle && edge.targetHandle === value.targetHandle)) { return true }
    try {
        const resolved=connection(value)
        if(completeWorkflow.value.edges.some(edge=>edge.source===resolved.source&&edge.target===resolved.target&&edge.sourceHandle===resolved.sourceHandle&&edge.targetHandle===resolved.targetHandle)){ return true }
        connectGraph(graphModel.value, resolved, props.schema); return true
    } catch { return false }
}
function reportConnectionError(error: unknown) {
    const code = error instanceof RuleInputError ? error.errors[0]?.code : undefined
    connectionError.value = t(code?.startsWith('graph.') ? `rules.${code}` : 'rules.graph.invalid')
}
function connect(value: Connection) {
    connectionCompleted = true
    try { publish(connectGraph(graphModel.value, connection(value), props.schema)); connectionError.value = '' }
    catch (error) { reportConnectionError(error) }
}

type QuickRequest = {kind: 'condition' | 'action'; mode?: 'auto' | 'wrap'; sourceHandle?: RuleGraphEdge['sourceHandle']}
type QuickState = {screen: {x: number; y: number}; position?: {x: number; y: number}; targetId?: string; kind?: 'condition' | 'action' | 'value'; mode?: 'auto' | 'wrap'; sourceHandle?: RuleGraphEdge['sourceHandle']; inputPath?:string}
const quick = ref<QuickState | null>(null)
const quickContext = computed(() => {
    if (!quick.value?.targetId) { return t('rules.canvas.addContextDefault') }
    const node = graphModel.value.nodes.find(node => node.id === quick.value!.targetId)
    if (!node) { return t('rules.canvas.addContextDefault') }
    const name = nodeLabel(node)
    if(quick.value.kind==='value'){ return t('rules.canvas.valueHelp') }
    if (!quick.value.kind) { return t((() => {
    if (node.kind === 'action') {
        return 'rules.canvas.addMixedActionContext'
    }
    if (node.kind === 'condition') {
        return 'rules.canvas.addMixedConditionContext'
    }
    return 'rules.canvas.addMixedRootContext'
})(), {name}) }
    return t((() => {
    if (quick.value.mode === 'wrap') {
        return 'rules.canvas.wrapContext'
    }
    if (quick.value.kind === 'action') {
        return 'rules.canvas.addAfterContext'
    }
    return 'rules.canvas.addConditionContext'
})(), {name})
})
function centerPoint() {
    const bounds = canvasElement.value?.getBoundingClientRect()
    return bounds ? {x: bounds.left + bounds.width / 2, y: bounds.top + Math.min(160, bounds.height / 2)} : {x: 40, y: 100}
}
function openSearch(point = centerPoint()) {
    quick.value = {screen: point, position: flow.screenToFlowCoordinate(point), targetId: selected.value?.id}
    connectionError.value = ''
}
function isCanvasBackground(target: EventTarget | null) {
    return target instanceof Element && !!canvasElement.value?.contains(target) && !target.closest('.vue-flow__node, .vue-flow__edge, .vue-flow__handle, button, input, textarea, select')
}
function canvasDoubleClick(event: MouseEvent) {
    if (!isCanvasBackground(event.target)) { return }
    event.preventDefault()
    openSearch({x: event.clientX, y: event.clientY})
}
function canvasContextMenu(event: MouseEvent) {
    event.preventDefault()
    openSearch({x: event.clientX, y: event.clientY})
}
function openNodeQuickAdd(id: string, request: QuickRequest) {
    const element = canvasElement.value?.querySelector(`[data-node-id="${CSS.escape(id)}"]`)
    const bounds = element?.getBoundingClientRect()
    quick.value = {targetId: id, kind: request.kind, mode: request.mode ?? 'auto', sourceHandle: request.sourceHandle, screen: bounds ? {x: bounds.left + 30, y: bounds.bottom - 25} : centerPoint()}
    connectionError.value = ''
}
function closeSearch() { quick.value = null; canvasElement.value?.focus({preventScroll: true}) }
async function createModule(options: RuleGraphAddOptions) {
    try {
        let next: RuleGraph
        let detached = false
        try { next = addGraphModule(graphModel.value, options, props.schema) }
        catch (error) {
            const code = error instanceof RuleInputError ? error.errors[0]?.code : undefined
            if ((options.mode === undefined || options.mode === 'auto') && (code === 'graph.target' || code === 'graph.orphan')) {
                next = addGraphModule(graphModel.value, {...options, targetId: undefined, mode: 'detached'}, props.schema)
                detached = true
            } else { throw error }
        }
        publish(next)
        connectionError.value = detached ? t('rules.graph.orphan') : ''
        quick.value = null
        libraryOpen.value = false
        const id = next.selected[0]
        if (id) {
            emit('select', id)
            await nextTick()
            await revealAdded(id)
        }
    } catch (error) { reportConnectionError(error) }
}
async function insertCombination(type: 'tool' | 'tools' | 'field' | 'model') {
 const actionType = (() => {
    if (type === 'tool') {
        return 'array_filter'
    }
    if (type === 'model') {
        return 'json_set'
    }
    return 'json_remove'
})()
 const target = quick.value?.targetId ? graphModel.value.nodes.find(node => node.id === quick.value?.targetId) : selected.value
 const next = addGraphModule(graphModel.value, {kind: 'action', type: actionType, targetId: target && ['action', 'rule'].includes(target.kind) ? target.id : undefined, position: quick.value?.position, sourceHandle:quick.value?.sourceHandle}, props.schema)
 const id = next.selected[0]!
 const action = graphNodeValue(next, id) as RuleAction
 const params = (() => {
    if (type === 'tool') {
        return {path: '/tools', predicate: {op: 'eq', source: 'item', path: '/name', value: ''}}
    }
    if (type === 'model') {
        return {path: '/model', value: '', create_parents: true}
    }
    return {paths: type === 'tools' ? ['/tools', '/tool_choice'] : ['']}
})()
 const configured = replaceGraphNode(next, id, {...action, params})
 publish(configured)
 libraryOpen.value = false
 quick.value = null
 await nextTick()
 await focus(id)
}
function focusPredicate(id: string) {
 if (expandedPredicates.value.includes(id)) { expandedPredicates.value = expandedPredicates.value.filter(value => value !== id); return }
 expandedPredicates.value.push(id)
 const predicate = graphModel.value.edges.find(edge => edge.target === id && edge.targetHandle === 'predicate:predicate')
 if (predicate) { void nextTick(() => focus(predicate.source)) }
}
function chooseQuick(value: {kind: 'condition' | 'action' | 'value'; type: string}) {
    if (!quick.value) { return }
    const node = graphModel.value.nodes.find(node => node.id === quick.value!.targetId)
    const targetId = value.kind === 'action' && node?.kind === 'condition' || value.kind === 'condition' && node?.kind === 'action' && !quick.value.kind ? undefined : quick.value.targetId
    void createModule({...value, targetId, position: quick.value.position, mode: quick.value.mode ?? 'auto', sourceHandle:quick.value.sourceHandle, inputPath:quick.value.inputPath})
}
function openValueInput(id:string,inputPath:string) {quick.value={screen:centerPoint(),targetId:id,kind:'value',inputPath}}
function moveArgument(id:string,path:string,direction:number) {
 const data=graphNodeValue(graphModel.value,id) as RuleGraphValue
 const listPath=path.slice(0,path.lastIndexOf('/')), index=Number(path.slice(path.lastIndexOf('/')+1))
 const list=[...readGraphInput(data,listPath) as unknown[]]
 if(direction===0){ list.splice(index,1) }
 else if(index+direction>=0&&index+direction<list.length){ [list[index],list[index+direction]]=[list[index+direction],list[index]] }
 writeGraphInput(data,listPath,list)
 emit('edit',id,data)
}
function libraryAdd(kind: 'condition' | 'action', type: string) {
    const wrap = kind === 'condition' && ['all', 'any', 'not'].includes(type)
    const targetId = (() => {
    if (kind === 'action') {
        if (selected.value?.kind === 'action' || selected.value?.kind === 'rule') {
            return selected.value.id
        }
        return undefined
    }
    if (selected.value?.kind === 'condition') {
        return selected.value.id
    }
    return undefined
})()
    void createModule({kind, type, targetId, mode: wrap ? 'wrap' : 'auto'})
}
let connecting: {nodeId: string | null; handleId: string | null; handleType: string | null} | undefined
let connectionCompleted = false
function startConnection(value: {nodeId?: string | null; handleId?: string | null; handleType?: string | null}) {
    connecting = {nodeId: value.nodeId ?? null, handleId: value.handleId ?? null, handleType: value.handleType ?? null}
    connectionCompleted = false
}
function resolvePendingConnection(pending: typeof connecting) {
    if (!pending?.nodeId || !pending.handleId?.startsWith('group:')) { return pending }
    const group = graphModel.value.groups?.find(group => group.id === pending.nodeId)
    const port = group && graphGroupPorts(graphModel.value, group).find(port => `group:${port.id}` === pending.handleId)
    if (!port) { return pending }
    return {...pending, nodeId: port.node, handleId: port.handle}
}
function endConnection(event?: MouseEvent | TouchEvent) {
    const pending = resolvePendingConnection(connecting)
    connecting = undefined
    if (!pending?.nodeId || !event || connectionCompleted || !isCanvasBackground(event.target)) { return }
    const point = 'changedTouches' in event ? event.changedTouches[0] : event
    if (!point) { return }
    if(pending.handleId?.startsWith('value:')) {
        quick.value={kind:'value',targetId:pending.nodeId,inputPath:pending.handleId.slice(6),screen:{x:point.clientX,y:point.clientY},position:flow.screenToFlowCoordinate({x:point.clientX,y:point.clientY})}
        return
    }
    let targetId = pending.nodeId
    let request: QuickRequest
    if (pending.handleId === 'action-out' || pending.handleId?.startsWith('flow:')) { request = {kind: 'action', sourceHandle:pending.handleId as RuleGraphEdge['sourceHandle']} }
    else if (pending.handleId === 'action-in') {
        const prior = graphModel.value.edges.find(edge => edge.target === targetId && edge.targetHandle === 'action-in')
        if (!prior) { return }
        targetId = prior.source
        request = {kind: 'action'}
    } else if (pending.handleId === 'boolean-out') { request = {kind: 'condition', mode: 'wrap'} }
    else if (pending.handleId === 'boolean-in' || pending.handleId === 'predicate:predicate') { request = {kind: 'condition'} }
    else { return }
    const screen = {x: point.clientX, y: point.clientY}
    quick.value = {...request, targetId, screen, position: flow.screenToFlowCoordinate(screen)}
}
function remove() {
    publish(removeGraphNodes(graphModel.value, graphModel.value.selected))
}
function duplicate() {
    publish(duplicateGraphNodes(graphModel.value, graphModel.value.selected))
}
// Programmatic viewport changes do not always emit Vue Flow's move-end event.
async function persistViewport() { await nextTick(); publish({...graphModel.value, layoutRestored: true, viewport: {...flow.getViewport()}}) }
async function fit() { await nextTick(); await flow.fitView({nodes:nodes.value.map(node=>node.id),padding: 0.12, maxZoom: 1}); await persistViewport() }
async function zoom(direction: 'in' | 'out') { if (direction === 'in') { await flow.zoomIn(); } else { await flow.zoomOut(); } await persistViewport() }
async function revealAdded(id: string) {
    const node = flow.findNode(id)
    const bounds = canvasElement.value?.getBoundingClientRect()
    if (!node || !bounds) { return }
    const point = flow.flowToScreenCoordinate(node.position)
    const viewport = flow.getViewport()
    const width = node.dimensions.width || ruleNodeWidth, height = node.dimensions.height || 240
    if (point.x >= bounds.left + 12 && point.y >= bounds.top + 12 && point.x + width * viewport.zoom <= bounds.right - 12 && point.y + height * viewport.zoom <= bounds.bottom - 12) { return }
    await flow.setCenter(node.position.x + width / 2, node.position.y + height / 2, {zoom: viewport.zoom})
    await persistViewport()
}
function layout() {
    const graph = clone(graphModel.value)
    const rendered=workflow.value
    const displayed=new Set(nodes.value.map(node=>node.id))
    const layoutNodes=[...graph.nodes,...(graph.groups??[])].filter(node=>displayed.has(node.id))
    const cache = new Map<string, number>()
    const depth = (id: string, seen = new Set<string>()): number => {
        if (seen.has(id) || seen.size > 128) { return 0 }
        if (cache.has(id)) { return cache.get(id)! }
        seen.add(id)
        const value = Math.max(0, ...rendered.edges.filter(edge => edge.target === id && displayed.has(edge.source)).map(edge => 1 + depth(edge.source, new Set(seen))))
        cache.set(id, value)
        return value
    }
    const widths = new Map<number, number>()
    for (const node of layoutNodes) {
        const column = depth(node.id)
        widths.set(column, Math.max(widths.get(column) ?? ruleNodeWidth, flow.findNode(node.id)?.dimensions.width ?? ruleNodeWidth))
    }
    const columns = new Map<number, number>()
    let offset = 40
    for (let column = 0; column <= Math.max(0, ...widths.keys()); column++) {
        columns.set(column, offset)
        offset += (widths.get(column) ?? ruleNodeWidth) + 100
    }
    const heights = new Map<number, number>()
    for (const node of layoutNodes) {
        const column = depth(node.id), y = heights.get(column) ?? 40
        node.position = {x: columns.get(column)!, y}
        heights.set(column, y + Math.max(200, flow.findNode(node.id)?.dimensions.height ?? 0) + 70)
    }
    publish(graph); void fit()
}
// ResizeObserver updates from Vue Flow include nested editors and sample path widgets.
// Move only overlapping cards, leaving manual placement and cached viewports intact.
let separationFrame = 0
function scheduleSeparation() {
    cancelAnimationFrame(separationFrame)
    separationFrame = requestAnimationFrame(() => { separationFrame = 0; separateOverlaps() })
}
function separateOverlaps() {
    const graph = clone(graphModel.value)
    const placed: {x: number; y: number; width: number; height: number}[] = []
    let changed = false
    for (const node of [...graph.nodes,...(graph.groups??[])].filter(node=>nodes.value.some(rendered=>rendered.id===node.id))) {
        const measured = flow.findNode(node.id)?.dimensions
        if (!measured?.height || !measured.width) { continue }
        let y = node.position.y
        const x = node.position.x, width = measured.width, height = measured.height
        let attemptsRemaining = graph.nodes.length
        while (attemptsRemaining-- > 0) {
            const conflicts = placed.filter(other => x < other.x + other.width + 30 && x + width + 30 > other.x && y < other.y + other.height + 40 && y + height + 40 > other.y)
            if (!conflicts.length) { break }
            y = Math.max(...conflicts.map(other => other.y + other.height + 40))
        }
        if (y !== node.position.y) { node.position.y = y; changed = true }
        placed.push({x, y, width, height})
    }
    if (changed) { publish(graph) }
}
let initialized = false
async function initializeLayout() {
    if (initialized) { return }
    initialized = true
    await nextTick()
    separateOverlaps()
    if (!graphModel.value.layoutRestored) {
        // Long default pipelines need a readable entry point; the fit button
        // still provides an overview of the complete graph.
        if (nodes.value.length > 8) {
            const first = workflow.value.edges.find(edge => edge.source === root.value.id && edge.sourceHandle === 'action-out')?.target
            await flow.fitView({nodes: moduleNodes.value.filter(node => node.id === root.value.id || node.data.path === '/when' || node.id === first).map(node => node.id), padding: 0.15, maxZoom: 1})
            await persistViewport()
        } else { await fit() }
    }
}
onBeforeUnmount(() => cancelAnimationFrame(separationFrame))
async function focus(id: string) {
    const owner=graphModel.value.groups?.find(group=>group.nodes.includes(id))
    if(owner){ activeGroup.value=owner.id }
    else if(activeGroup.value){ activeGroup.value=undefined }
    const before=new Set(nodes.value.map(node=>node.id))
    const expose=(id:string,seen=new Set<string>())=>{
        if(seen.has(id)){ return; }seen.add(id)
        for(const edge of graphModel.value.edges.filter(edge=>edge.source===id&&edge.sourceHandle==='value-out')) {if(!expandedValues.value.includes(edge.target)){ expandedValues.value.push(edge.target); }expose(edge.target,seen)}
    }
    expose(id)
    if(graphModel.value.nodes.find(node=>node.id===id)?.kind==='value'&&!expandedValues.value.includes(id)){ expandedValues.value.push(id) }
    const parent = graphModel.value.edges.find(edge => edge.source === id && edge.targetHandle.startsWith('predicate:'))
    if (parent && !expandedPredicates.value.includes(parent.target)) { expandedPredicates.value.push(parent.target) }
    select(id)
    await nextTick()
    const graph=clone(graphModel.value)
    const arranged=new Set<string>()
    const arrange=(owner:string)=>{
        if(arranged.has(owner)){ return; }arranged.add(owner)
        const node=graph.nodes.find(node=>node.id===owner)!
        let row=node.position.y
        for(const edge of graph.edges.filter(edge=>edge.target===owner&&edge.sourceHandle==='value-out')) {
            const source=graph.nodes.find(node=>node.id===edge.source)!
            if(!hiddenValues.value.has(source.id)&&!before.has(source.id)){ source.position={x:node.position.x-ruleNodeColumnGap,y:row} }
            row+=420
            if(!hiddenValues.value.has(source.id)){ arrange(source.id) }
        }
    }
    graph.nodes.filter(node=>node.kind!=='value').forEach(node=>arrange(node.id))
    publish(graph)
    await nextTick()
    const dependencies=graphModel.value.edges.filter(edge=>edge.target===id&&edge.sourceHandle==='value-out').map(edge=>edge.source)
    await flow.fitView({nodes: [id,...dependencies], padding: 0.15, maxZoom: 1})
    await persistViewport()
}
function keyboard(event: KeyboardEvent) {
    if (event.isComposing || (event.target as HTMLElement).closest?.('input,textarea,select,button,a,[contenteditable=true]')) { return }
    if (event.key === ' ' && !event.ctrlKey && !event.metaKey && !event.altKey) { event.preventDefault(); openSearch(); return }
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'z') { event.preventDefault(); if (event.shiftKey) { emit('redo'); } else { emit('undo') } }
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'd') { event.preventDefault(); duplicate() }
}
defineExpose({focus, fit})
</script>
<template>
    <div class="rule-canvas-shell" @keydown="keyboard">
        <div class="rule-canvas-toolbar">
            <div class="rule-toolbar-group">
                <button type="button" class="btn btn-primary" data-testid="canvas-add-node" @click="openSearch()"><Plus />{{ t('rules.canvas.addNode') }}</button>
                <button type="button" class="btn" data-testid="toggle-module-library" :aria-expanded="libraryOpen" @click="libraryOpen = !libraryOpen"><Search />{{ t('rules.canvas.library') }}</button>
                <button type="button" class="btn" data-testid="canvas-create-group" :disabled="!modelValue.selected.some(id=>modelValue.nodes.some(node=>node.id===id&&node.kind!=='rule')) || !!activeGroup" @click="createGroup">{{locale==='zh-CN'?'收纳为组':'Group selected'}}</button>
                <button v-if="activeGroup" type="button" class="btn" data-testid="canvas-leave-group" @click="leaveGroup">← {{locale==='zh-CN'?'返回工作流':'Back to workflow'}}</button>
            </div>
            <div class="rule-toolbar-group">
                <button type="button" class="btn" data-testid="canvas-zoom-in" :title="t('rules.canvas.zoomIn')" :aria-label="t('rules.canvas.zoomIn')" @click="zoom('in')"><ZoomIn /></button>
                <button type="button" class="btn" data-testid="canvas-zoom-out" :title="t('rules.canvas.zoomOut')" :aria-label="t('rules.canvas.zoomOut')" @click="zoom('out')"><ZoomOut /></button>
                <button type="button" class="btn" data-testid="canvas-fit" @click="fit"><Maximize />{{ t('rules.canvas.fit') }}</button>
                <button type="button" class="btn" @click="layout"><LayoutGrid />{{ t('rules.canvas.layout') }}</button>
                <select class="input rule-node-jump" data-testid="canvas-node-jump" :aria-label="t('rules.canvas.jumpNode')" value="" @change="focus(($event.target as HTMLSelectElement).value); ($event.target as HTMLSelectElement).value = ''">
                    <option value="" disabled>{{ t('rules.canvas.jumpNode') }}</option>
                    <option v-for="node in completeWorkflow.nodes" :key="node.id" :value="node.id">{{ nodeLabel(node) }} · {{ pathMap[node.id] || '/' }}</option>
                </select>
            </div>
            <div class="rule-toolbar-group">
                <button type="button" class="btn" :disabled="!canUndo" @click="emit('undo')"><Undo2 />{{ t('rules.canvas.undo') }}</button>
                <button type="button" class="btn" :disabled="!canRedo" @click="emit('redo')"><Redo2 />{{ t('rules.canvas.redo') }}</button>
                <button type="button" class="btn" :disabled="!selected || selected.kind === 'rule'" @click="duplicate"><Copy />{{ t('rules.copy') }}</button>
                <button type="button" class="btn" :disabled="!selected || selected.kind === 'rule'" @click="remove"><Trash2 />{{ t('rules.remove') }}</button>
            </div>
        </div>
        <p class="rule-canvas-hint">{{ t('rules.canvas.inlineHint') }}</p>
        <p v-if="activeGroup" class="rule-canvas-hint">{{locale==='zh-CN'?'组内编辑':'Editing group'}} · {{modelValue.groups?.find(group=>group.id===activeGroup)?.name}}</p>
        <div class="rule-canvas-body">
            <aside class="rule-module-library" :class="{'is-open': libraryOpen}" data-testid="module-library">
                <button type="button" class="btn library-close" @click="libraryOpen = false">{{ t('common.close') }}</button>
                <input v-model="search" class="input" :placeholder="t('rules.canvas.search')" :aria-label="t('rules.canvas.search')" />
                <h3 :title="t('rules.canvas.combinationHint')">{{ t('rules.canvas.commonCombinations') }}</h3>
                <button v-for="item in ([['tool', 'removeTool'], ['tools', 'removeAllTools'], ['field', 'removeField'], ['model', 'setModel']] as const)" :key="item[0]" type="button" class="module-item" :data-testid="`add-combination-${item[0]}`" :title="t('rules.canvas.combinationHint')" @click="insertCombination(item[0])">{{ t(`rules.canvas.${item[1]}`) }}</button>
                <h3>{{ t('rules.addCondition') }}</h3>
                <button v-for="cap in conditions" :key="cap.id" type="button" class="module-item" :data-testid="`add-condition-${cap.id}`" :title="localHelp(cap.description, locale)" @click="libraryAdd('condition', cap.id)">{{ ruleLabel('condition', cap.id, locale) }}</button>
                <h3>{{ t('rules.addAction') }}</h3>
                <button v-for="cap in actions" :key="cap.id" type="button" class="module-item" :data-testid="`add-action-${cap.id}`" :title="localHelp(cap.description, locale)" @click="libraryAdd('action', cap.id)">{{ ruleLabel('action', cap.id, locale) }}</button>
            </aside>
            <div ref="canvasElement" class="rule-flow" data-testid="rule-canvas" role="application" :aria-label="t('rules.graphical')" tabindex="0" @dblclick="canvasDoubleClick">
                <VueFlow :id="flowId" :nodes="nodes" :edges="edges" :apply-default="false" :default-viewport="modelValue.viewport" :min-zoom="0.15" :max-zoom="2.5" :zoom-on-double-click="false" :is-valid-connection="validConnection" :delete-key-code="['Backspace', 'Delete']" :fit-view-on-init="false" @nodes-initialized="initializeLayout" @nodes-change="changes" @edges-change="edgeChanges" @connect="connect" @connect-start="startConnection" @connect-end="endConnection" @pane-context-menu="canvasContextMenu" @node-click="select($event.node.id,$event.event.shiftKey||$event.event.ctrlKey||$event.event.metaKey)" @move-end="publish({...modelValue, viewport: $event.flowTransform})">
                    <template #node-rule-module="nodeProps">
                        <RuleCanvasNode v-bind="nodeProps" @update:data="emit('edit', nodeProps.id, $event)" @value-input="openValueInput(nodeProps.id,$event)" @focus-value="focus($event)" @move-argument="(path,direction)=>moveArgument(nodeProps.id,path,direction)" @quick-add="openNodeQuickAdd(nodeProps.id, $event)" @focus-predicate="focusPredicate(nodeProps.id)" @edit-start="emit('edit-start', nodeProps.id)" @edit-end="emit('edit-end', nodeProps.id)" />
                    </template>
                    <template #node-rule-group="nodeProps"><RuleGroupNode v-bind="nodeProps" @enter="enterGroup" @ungroup="ungroup" @rename="(id,name)=>updateGroup(id,group=>group.name=name)" @register="portManager=$event" @port="renamePort" /></template>
                </VueFlow>
            </div>
        </div>
        <p v-if="connectionError" role="alert" class="text-xs text-red-500">{{ connectionError }}</p>
        <div v-if="managedGroup" class="group-port-manager">
         <strong>{{managedGroup.name}} · {{locale==='zh-CN'?'输入／输出端口':'Input / output ports'}}</strong>
         <div v-for="port in managedPorts" :key="port.id" class="port-manager-row"><span>{{port.direction==='input'?'↳':'↗'}}</span><label class="sr-only" :for="`${formId}-${port.id}`">{{ portLabel(port) }}</label><input :id="`${formId}-${port.id}`" class="input" :value="port.name||portLabel(port)" @change="renamePort(managedGroup.id,port.id,($event.target as HTMLInputElement).value)" /><button type="button" class="btn" :disabled="!managedGroup.ports.some(item=>item.id===port.id)" @click="updateGroup(managedGroup.id,group=>group.ports=group.ports.filter(item=>item.id!==port.id))">×</button></div>
         <div class="port-manager-row"><select v-model="portCandidate" class="input" :aria-label="locale==='zh-CN'?'注册组端口':'Register group port'"><option value="" disabled>{{locale==='zh-CN'?'选择组内节点端口':'Select internal port'}}</option><option v-for="(port,index) in portCandidates" :key="index" :value="String(index)">{{port.direction==='input'?'↳':'↗'}} {{port.name}} · {{port.handle}}</option></select><button type="button" class="btn" @click="registerPort">+</button><button type="button" class="btn" @click="portManager=undefined">{{t('common.close')}}</button></div>
        </div>
        <RuleNodeSearch :open="!!quick" :position="quick?.screen ?? {x: 0, y: 0}" :schema="schema" :phase="phase" :kind="quick?.kind" :wrap="quick?.mode === 'wrap'" :context="quickContext" @select="chooseQuick" @combination="insertCombination" @close="closeSearch" />
    </div>
</template>
<style scoped>
.rule-canvas-shell { min-width: 0; border: 1px solid var(--color-line); border-radius: 12px; background: var(--color-surface); overflow: hidden; }
.group-port-manager {display:grid;gap:8px;padding:14px;border-top:1px solid var(--color-line);font-size:12px}
.port-manager-row {display:flex;align-items:center;gap:6px;min-width:0}.port-manager-row .input {flex:1;min-width:0}
.rule-canvas-toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 16px; padding: 10px 12px; border-bottom: 1px solid var(--color-line); }
.rule-toolbar-group { display: flex; align-items: center; flex-wrap: wrap; gap: 4px; }
.rule-toolbar-group + .rule-toolbar-group { padding-left: 16px; border-left: 1px solid var(--color-line); }
.rule-canvas-toolbar .btn { height: 32px; padding: 5px 10px; border-radius: 6px; font-size: 11px; }
.rule-canvas-toolbar svg { width: 14px; height: 14px; flex-shrink: 0; }
.rule-node-jump { width:160px; max-width:100%; min-height:32px; padding:5px 8px; font-size:11px; }
.rule-canvas-hint { margin: 0; padding: 8px 12px; font-size: 11px; color: var(--color-ink-muted); }
.rule-canvas-body { display: flex; min-width: 0; position: relative; border-top: 1px solid var(--color-line); }
.rule-module-library { display: none; width: 220px; flex-shrink: 0; max-height: 760px; overflow-y: auto; padding: 12px; border-right: 1px solid var(--color-line); background: var(--color-surface-elevated); }
.rule-module-library.is-open { display: block; }
.rule-module-library h3 { font-size: 11px; font-weight: 650; color: var(--color-ink-muted); margin: 16px 0 6px; }
.module-item { width: 100%; text-align: left; font-size: 12px; padding: 8px; border-radius: 6px; }
.module-item:hover { background: var(--color-surface-2); color: var(--color-accent); }
.rule-flow { flex: 1; min-width: 0; height: 760px; background-color: var(--color-canvas); background-image: radial-gradient(var(--color-line-strong) 1px, transparent 1px); background-size: 24px 24px; }
.library-close { display: inline-flex; margin-bottom: 8px; }
:deep(.vue-flow__node.selected .rule-graph-node) { outline: 2px solid var(--color-accent); outline-offset: 2px; }
:deep(.vue-flow__handle) { background: #818cf8; width: 10px; height: 10px; border: 2px solid var(--color-surface-elevated); }
:deep(.vue-flow__handle[data-handleid^='action']) { background: #d29958; border-radius: 50%; }
:deep(.vue-flow__edge-text) { fill: var(--color-ink-muted); font-size: 10px; }
:deep(.vue-flow__edge-textbg) { fill: var(--color-surface-elevated); }
@media(max-width:1100px) { .rule-module-library { position: absolute; left: 0; top: 0; bottom: 0; width: min(260px, 80vw); z-index: 20; box-shadow: 5px 0 20px rgb(0 0 0 / 15%); } }
@media(max-width:640px) { .rule-flow { height: 620px; } .rule-canvas-toolbar { gap: 6px; padding: 8px; } .rule-toolbar-group + .rule-toolbar-group { padding-left: 0; border-left: 0; } .rule-canvas-toolbar .btn { padding: 5px 7px; } }
</style>
