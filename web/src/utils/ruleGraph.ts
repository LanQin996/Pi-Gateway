import {cloneJson} from './cloneJson'
import type {Rule, RuleAction, RuleCondition, RuleFieldError, RuleSchema, JsonValue} from '../api/rules'
import {editorId} from './editorId'
import {newAction, ruleSchema} from './ruleSchema'
import {defaultCondition, RuleInputError, validateRule} from './ruleEditor'

export type RuleGraphNode = {
    id: string; kind: 'rule' | 'condition' | 'action' | 'value'; path: string;
    position: {x: number; y: number}; data: Rule | RuleCondition | RuleAction | RuleGraphValue;
}

export interface RuleGraphValue {
    mode: 'computed' | 'reference' | 'literal' | 'values';
    value: JsonValue
}

export interface GraphValueInput {
    path: string;
    label: string;
    value: unknown;
    index?: number
}

const escapePart = (key: string) => key.replaceAll('~', '~0').replaceAll('/', '~1')
const pointerParts = (path: string) => path.slice(1).split('/').map(key => key.replaceAll('~1', '/').replaceAll('~0', '~'))

export function readGraphInput(data: RuleGraphNode['data'], path: string): unknown {
    return pointerParts(path).reduce<unknown>((value, key) => value && typeof value === 'object' ? (value as Record<string, unknown>)[key] : undefined, data)
}

export function writeGraphInput(data: RuleGraphNode['data'], path: string, value: unknown): void {
    const parts = pointerParts(path), key = parts.pop()!
    const parent = parts.reduce<unknown>((value, key) => (value as Record<string, unknown>)[key], data) as Record<string, unknown>
    if (value === undefined) delete parent[key]
    else parent[key] = value
}

export function graphValueMode(value: unknown): RuleGraphValue['mode'] | undefined {
    if (value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === 1) {
        if ('$expr' in value) return 'computed'
        if ('$ref' in value) return 'reference'
    }
}

export function graphValueInputs(node: Pick<RuleGraphNode, 'kind' | 'data'>): GraphValueInput[] {
    if (node.kind === 'value') {
        const data = node.data as RuleGraphValue
        let args: unknown
        if (data.mode === 'computed') args = (data.value as {$expr: {args: JsonValue[]}}).$expr.args
        else if (data.mode === 'values') args = data.value
        return Array.isArray(args) ? args.map((value, index) => ({
            path: data.mode === 'computed' ? `/value/$expr/args/${index}` : `/value/${index}`,
            label: 'args',
            value,
            index
        })) : []
    }
    if (node.kind === 'condition') {
        const condition = node.data as RuleCondition
        return Object.hasOwn(condition, 'value') && graphValueMode(condition.value) ? [{
            path: '/value',
            label: 'value',
            value: condition.value
        }] : []
    }
    if (node.kind !== 'action') return []
    const action = node.data as RuleAction
    return ['value', 'values'].filter(key => Object.hasOwn(action.params, key)).map(key => ({
        path: `/params/${key}`,
        label: key,
        value: action.params[key]
    }))
}

export const complexGraphCondition = (value: unknown): boolean => !!value && typeof value === 'object' && (['all', 'any', 'not', 'test'].includes((value as RuleCondition).op) || !!graphValueMode((value as RuleCondition).value))

export function graphConditionInputs(action: RuleAction): string[] {
    switch (action.type) {
        case 'array_filter':
        case 'if': return ['predicate']
        case 'walk': return ['predicate', 'keep']
        case 'for_each': return ['keep']
        default: return []
    }
}

export interface RuleGraphEdge {
    id: string;
    source: string;
    target: string;
    sourceHandle: 'boolean-out' | 'action-out' | 'value-out' | `flow:${string}`;
    targetHandle: string;
    order: number;
}

export interface RuleGraph {
    nodes: RuleGraphNode[];
    edges: RuleGraphEdge[];
    viewport: {x: number; y: number; zoom: number};
    selected: string[];
    layoutRestored?: boolean;
    groups?: RuleGraphGroup[];
}

export interface RuleGraphGroup {
    id: string;
    name: string;
    nodes: string[];
    position: {x: number; y: number};
    ports: {id: string; node: string; handle: string; direction: 'input' | 'output'; name: string}[];
}

export function groupGraphNodes(graph: RuleGraph, ids: string[], name: string): RuleGraph {
    const next = copy(graph), members = graphNodeClosure(next, ids)
    for (const id of members) if (next.groups?.some(group => group.nodes.includes(id))) members.delete(id)
    if (!members.size) throw new RuleInputError([{path: '', code: 'graph.target', message: 'target'}])
    const nodes = next.nodes.filter(node => members.has(node.id))
    const group: RuleGraphGroup = {
        id: editorId(),
        name,
        nodes: [...members],
        position: {
            x: Math.min(...nodes.map(node => node.position.x)),
            y: Math.min(...nodes.map(node => node.position.y))
        },
        ports: []
    }
    group.ports = graphGroupPorts(next, group)
    next.groups = [...(next.groups ?? []), group]
    return next
}

export function graphGroupPorts(graph: RuleGraph, group: RuleGraphGroup) {
    const members = new Set(group.nodes),
        ports = group.ports.filter(port => members.has(port.node) && graph.nodes.some(node => node.id === port.node)).map(port => ({...port}))
    for (const edge of visibleWorkflow(graph).edges) {
        if (members.has(edge.source) === members.has(edge.target)) continue
        const direction = members.has(edge.source) ? 'output' : 'input',
            node = direction === 'output' ? edge.source : edge.target,
            handle = direction === 'output' ? edge.sourceHandle : edge.targetHandle
        if (!ports.some(port => port.node === node && port.handle === handle && port.direction === direction)) ports.push({
            id: `${direction}:${node}:${handle}`,
            node,
            handle,
            direction,
            name: ''
        })
    }
    return ports
}

export function resolveGroupConnection(graph: RuleGraph, connection: {
    source: string;
    target: string;
    sourceHandle?: string | null;
    targetHandle?: string | null
}): Omit<RuleGraphEdge, 'id' | 'order'> {
    const resolve = (id: string, handle: string | null | undefined, direction: 'input' | 'output') => {
        const owner = graph.groups?.find(group => group.id === id)
        if (!owner) return {node: id, handle: handle ?? ''}
        const port = graphGroupPorts(graph, owner).find(port => `group:${port.id}` === handle && port.direction === direction)
        if (!port) throw new RuleInputError([{path: '', code: 'graph.port', message: 'port'}])
        return {node: port.node, handle: port.handle}
    }
    const source = resolve(connection.source, connection.sourceHandle, 'output'),
        target = resolve(connection.target, connection.targetHandle, 'input')
    return {
        source: source.node,
        target: target.node,
        sourceHandle: source.handle as RuleGraphEdge['sourceHandle'],
        targetHandle: target.handle
    }
}

export function groupedWorkflow(graph: RuleGraph, active?: string) {
    const workflow = visibleWorkflow(graph),
        groups = (graph.groups ?? []).filter(group => group.nodes.some(id => workflow.nodes.some(node => node.id === id)))
    const current = groups.find(group => group.id === active)
    if (current) return {
        nodes: workflow.nodes.filter(node => current.nodes.includes(node.id)),
        groups: [],
        edges: workflow.edges.filter(edge => current.nodes.includes(edge.source) && current.nodes.includes(edge.target))
    }
    const owners = new Map<string, RuleGraphGroup>()
    for (const group of groups) for (const id of group.nodes) owners.set(id, group)
    return {
        nodes: workflow.nodes.filter(node => !owners.has(node.id)), groups,
        edges: workflow.edges.flatMap(edge => {
            const from = owners.get(edge.source), to = owners.get(edge.target)
            if (from && from.id === to?.id) return []
            const sourcePort = from ? graphGroupPorts(graph, from).find(port => port.node === edge.source && port.handle === edge.sourceHandle && port.direction === 'output') : undefined
            const targetPort = to ? graphGroupPorts(graph, to).find(port => port.node === edge.target && port.handle === edge.targetHandle && port.direction === 'input') : undefined
            return [{
                ...edge,
                source: from?.id ?? edge.source,
                target: to?.id ?? edge.target,
                sourceHandle: sourcePort ? `group:${sourcePort.id}` : edge.sourceHandle,
                targetHandle: targetPort ? `group:${targetPort.id}` : edge.targetHandle
            }]
        })
    }
}

export interface RuleGraphAddOptions {
    kind: 'condition' | 'action' | 'value'
    type: string
    targetId?: string
    position?: {x: number; y: number}
    mode?: 'auto' | 'detached' | 'wrap'
    sourceHandle?: RuleGraphEdge['sourceHandle']
    inputPath?: string
}

export interface RuleGraphLayout {
    signature: string
    nodes: Record<string, {x: number; y: number}>
    viewport: {x: number; y: number; zoom: number}
    groups?: {
        id: string;
        name: string;
        nodes: string[];
        position: {x: number; y: number};
        ports: RuleGraphGroup['ports']
    }[]
}

const copy = cloneJson
const layoutStoragePrefix = 'pi-rule-layout:'
// Leave room for inline value/reference editors; saved or explicitly placed positions stay exact.
const defaultRowSpacing = 680
export const ruleNodeWidth = 400
export const ruleNodeColumnGap = 500
const memoryLayouts = new Map<string, RuleGraphLayout>()
const validCoordinate = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value) && Math.abs(value) <= 10_000_000
const validViewport = (value: RuleGraph['viewport'] | undefined) => !!value && validCoordinate(value.x) && validCoordinate(value.y) && Number.isFinite(value.zoom) && value.zoom >= 0.15 && value.zoom <= 2.5

export function graphLayoutNodeKey(node: RuleGraphNode, graph: RuleGraph, paths = graphPathMap(graph)): string {
    if (node.kind === 'rule') return 'rule'
    const actionKey = (action: RuleGraphNode, seen = new Set<string>()): string => {
        if (seen.has(action.id)) throw new Error('Invalid action scope')
        seen.add(action.id)
        let incoming = graph.edges.find(edge => edge.target === action.id && edge.targetHandle === 'action-in')
        while (incoming?.sourceHandle === 'action-out') {
            const previous = graph.nodes.find(node => node.id === incoming!.source)
            if (!previous || previous.kind === 'rule') break
            if (seen.has(previous.id)) throw new Error('Invalid action chain')
            seen.add(previous.id)
            incoming = graph.edges.find(edge => edge.target === previous.id && edge.targetHandle === 'action-in')
        }
        const scope = incoming?.sourceHandle.startsWith('flow:') ? graph.nodes.find(node => node.id === incoming!.source) : undefined
        const prefix = scope ? `${actionKey(scope, seen)}/${incoming!.sourceHandle}/` : 'action:'
        return prefix + (action.data as RuleAction).id
    }
    if (node.kind === 'action') return actionKey(node)
    const path = paths[node.id]
    if (path === undefined) throw new Error('Only connected nodes have stable layout identities')
    const action = graph.nodes.filter(n => n.kind === 'action' && path.startsWith(`${paths[n.id]}/params/`)).sort((a, b) => (paths[b.id]?.length ?? 0) - (paths[a.id]?.length ?? 0))[0]
    return action ? `${actionKey(action)}${path.slice(paths[action.id]!.length)}` : `condition:${path}`
}

function graphNodeType(node: RuleGraphNode): string {
    switch (node.kind) {
        case 'condition': return (node.data as RuleCondition).op
        case 'action': return (node.data as RuleAction).type
        case 'value': return (node.data as RuleGraphValue).mode
        default: return 'rule'
    }
}

export function graphLayoutSignature(graph: RuleGraph): string {
    if (graph.nodes.length > 2048) throw new Error('Layout node budget exceeded')
    const paths = graphPathMap(graph)
    const keys = new Map(graph.nodes.map(node => [node.id, graphLayoutNodeKey(node, graph, paths)]))
    const nodes = graph.nodes.map(node => ({
        key: keys.get(node.id)!,
        kind: node.kind,
        type: graphNodeType(node)
    })).sort((a, b) => a.key.localeCompare(b.key))
    const edges = graph.edges.map(edge => ({
        source: keys.get(edge.source),
        target: keys.get(edge.target),
        sourceHandle: edge.sourceHandle,
        targetHandle: edge.targetHandle,
        order: edge.targetHandle === 'boolean-in' ? edge.order : 0
    })).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)))
    // Hash topology only: no name, parameter value, payload field path or sample is stored.
    let hash = 2166136261
    for (const char of JSON.stringify({nodes, edges})) hash = Math.imul(hash ^ char.codePointAt(0)!, 16777619)
    return `v1-${(hash >>> 0).toString(16)}`
}

export function graphLayoutSnapshot(graph: RuleGraph): RuleGraphLayout {
    if (graph.nodes.length > 2048) throw new Error('Layout node budget exceeded')
    const paths = graphPathMap(graph)
    if (Object.keys(paths).length !== graph.nodes.length) throw new Error('Invalid layout topology')
    const nodes: RuleGraphLayout['nodes'] = Object.create(null)
    for (const node of graph.nodes) {
        if (!validCoordinate(node.position.x) || !validCoordinate(node.position.y)) throw new Error('Invalid layout coordinate')
        nodes[graphLayoutNodeKey(node, graph, paths)] = {x: node.position.x, y: node.position.y}
    }
    const viewport = validViewport(graph.viewport) ? {
        x: graph.viewport.x,
        y: graph.viewport.y,
        zoom: graph.viewport.zoom
    } : {x: 0, y: 0, zoom: 1}
    const groups = graph.groups?.map(group => ({
        ...copy(group),
        nodes: group.nodes.map(id => graphLayoutNodeKey(graph.nodes.find(node => node.id === id)!, graph, paths)),
        ports: group.ports.map(port => ({
            ...port,
            node: graphLayoutNodeKey(graph.nodes.find(node => node.id === port.node)!, graph, paths)
        }))
    }))
    return {signature: graphLayoutSignature(graph), nodes, viewport, ...(groups?.length ? {groups} : {})}
}

export function applyGraphLayout(graph: RuleGraph, layout: RuleGraphLayout | null | undefined): RuleGraph {
    if (!layout?.nodes || layout.signature !== graphLayoutSignature(graph) || !validViewport(layout.viewport) || Object.keys(layout.nodes).length !== graph.nodes.length) return graph
    const paths = graphPathMap(graph)
    const nodes = graph.nodes.map(node => {
        const position = layout.nodes[graphLayoutNodeKey(node, graph, paths)]
        return position && validCoordinate(position.x) && validCoordinate(position.y) ? {
            ...node,
            position: {x: position.x, y: position.y}
        } : null
    })
    if (nodes.includes(null)) return graph
    const identities = new Map(graph.nodes.map(node => [graphLayoutNodeKey(node, graph, paths), node.id]))
    const groups = Array.isArray(layout.groups) ? layout.groups.slice(0, 100).filter(group => typeof group.name === 'string' && group.name.length <= 100 && validCoordinate(group.position?.x) && validCoordinate(group.position?.y) && Array.isArray(group.nodes) && group.nodes.every(key => identities.has(key)) && Array.isArray(group.ports)).map(group => ({
        ...group,
        nodes: group.nodes.map(key => identities.get(key)!),
        ports: group.ports.filter(port => identities.has(port.node) && ['input', 'output'].includes(port.direction)).map(port => ({
            ...port,
            node: identities.get(port.node)!
        }))
    })) : undefined
    return {
        ...graph,
        nodes: nodes as RuleGraphNode[],
        viewport: {x: layout.viewport.x, y: layout.viewport.y, zoom: layout.viewport.zoom},
        layoutRestored: true
        , groups
    }
}

function layoutStorage(): Storage | undefined {
    try {
        return globalThis.localStorage
    } catch {
        return undefined
    }
}

export function restoreGraphLayout(graph: RuleGraph): RuleGraph {
    const ruleId = (graph.nodes.find(node => node.kind === 'rule')?.data as Rule | undefined)?.id
    if (!ruleId) return graph
    const key = `${layoutStoragePrefix}${encodeURIComponent(ruleId)}`
    let layout = memoryLayouts.get(key)
    try {
        const raw = layoutStorage()?.getItem(key)
        if (raw && raw.length <= 524288) layout = JSON.parse(raw) as RuleGraphLayout
    } catch { /* unavailable or malformed storage: use the bounded memory fallback */
    }
    try {
        return applyGraphLayout(graph, layout)
    } catch {
        return graph
    }
}

export function persistGraphLayout(graph: RuleGraph): void {
    const ruleId = (graph.nodes.find(node => node.kind === 'rule')?.data as Rule | undefined)?.id
    if (!ruleId) return
    try {
        const key = `${layoutStoragePrefix}${encodeURIComponent(ruleId)}`, layout = graphLayoutSnapshot(graph)
        memoryLayouts.delete(key);
        memoryLayouts.set(key, layout)
        if (memoryLayouts.size > 50) memoryLayouts.delete(memoryLayouts.keys().next().value!)
        layoutStorage()?.setItem(key, JSON.stringify(layout))
    } catch { /* invalid graph, private mode or quota: memory draft remains usable */
    }
}

const group = (node: RuleGraphNode) => node.kind === 'condition' && ['all', 'any', 'not'].includes((node.data as RuleCondition).op)
// Flow lists are connected scopes. Sequence is retained only as a transparent
// compatibility boundary so opening an old rule never rewrites its stored AST.
export function actionFlowPorts(action: RuleAction): string[] {
    if (action.type === 'if') return ['then', 'else']
    if (['sequence', 'for_each', 'walk'].includes(action.type)) return ['steps']
    if (action.type === 'scope') return ['steps', ...Object.keys((action.params.functions ?? {}) as object).map(key => `functions/${key.replaceAll('~', '~0').replaceAll('/', '~1')}`)]
    return []
}

function flowList(action: RuleAction, port: string): RuleAction[] | undefined {
    if (port.startsWith('functions/')) return (action.params.functions as Record<string, RuleAction[]> | undefined)?.[port.slice(10).replaceAll('~1', '/').replaceAll('~0', '~')]
    return action.params[port] as RuleAction[] | undefined
}

function setFlowList(action: RuleAction, port: string, value: RuleAction[]) {
    if (port.startsWith('functions/')) (action.params.functions as Record<string, RuleAction[]>)[port.slice(10).replaceAll('~1', '/').replaceAll('~0', '~')] = value
    else action.params[port] = value
}

export function visibleWorkflow(graph: RuleGraph): {nodes: RuleGraphNode[]; edges: RuleGraphEdge[]} {
    const hidden = new Set(graph.nodes.filter(node => node.kind === 'action' && (node.data as RuleAction).type === 'sequence').map(node => node.id))
    const nodes = graph.nodes.filter(node => !hidden.has(node.id))
    const incoming = (id: string) => graph.edges.find(edge => edge.target === id && edge.targetHandle === 'action-in')

    function successor(id: string, port: RuleGraphEdge['sourceHandle'], seen = new Set<string>()): RuleGraphEdge | undefined {
        if (seen.has(`${id}:${port}`)) return
        seen.add(`${id}:${port}`)
        const edge = graph.edges.find(edge => edge.source === id && edge.sourceHandle === port)
        if (edge) {
            if (!hidden.has(edge.target)) return edge
            return successor(edge.target, 'flow:steps', seen) ?? successor(edge.target, 'action-out', seen)
        }
        // Finishing a legacy sequence returns to its enclosing linear chain.
        if (port === 'action-out') {
            let parent = incoming(id)
            while (parent?.sourceHandle === 'action-out' && !seen.has(parent.source)) {
                seen.add(parent.source);
                parent = incoming(parent.source)
            }
            if (parent?.sourceHandle === 'flow:steps' && hidden.has(parent.source)) return successor(parent.source, 'action-out', seen)
        }
    }

    const edges = graph.edges.filter(edge => ['boolean-out', 'value-out'].includes(edge.sourceHandle) && !hidden.has(edge.target) && !hidden.has(edge.source))
    for (const node of nodes.filter(node => ['rule', 'action'].includes(node.kind))) {
        const ports: RuleGraphEdge['sourceHandle'][] = ['action-out', ...(node.kind === 'action' ? actionFlowPorts(node.data as RuleAction).map(port => `flow:${port}` as const) : [])]
        for (const port of ports) {
            const edge = successor(node.id, port)
            if (edge) edges.push({...edge, source: node.id, sourceHandle: port})
        }
    }
    return {nodes, edges}
}
export const graphEdge = (source: string, target: string, targetHandle: string, order = 0): RuleGraphEdge => ({
    id: editorId(),
    source,
    target,
    sourceHandle: targetHandle === 'action-in' ? 'action-out' : 'boolean-out',
    targetHandle,
    order
})

// A selected flow owner owns its branch bodies and predicates, never its next sibling.
export function graphNodeClosure(graph: RuleGraph, ids: string[]): Set<string> {
    const roots = ids.filter(id => graph.nodes.some(node => node.id === id && node.kind !== 'rule'))
    const included = new Set(roots)
    const visited = new Set<string>()
    const collect = (id: string, body = false) => {
        const key = `${id}:${body}`
        if (visited.has(key)) return
        visited.add(key)
        for (const edge of graph.edges) {
            const predicate = edge.target === id && ['boolean-out', 'value-out'].includes(edge.sourceHandle)
            const childFlow = edge.source === id && (edge.sourceHandle.startsWith('flow:') || body && edge.sourceHandle === 'action-out')
            if (!predicate && !childFlow) continue
            const child = predicate ? edge.source : edge.target
            included.add(child)
            collect(child, childFlow)
        }
    }
    // Descendants collected in body mode must not be revisited as roots.
    for (const id of roots) collect(id)
    return included
}

export function removeGraphNodes(graph: RuleGraph, ids: string[]): RuleGraph {
    const next = copy(graph), removed = graphNodeClosure(next, ids)
    // Removing an action closes the gap in its own chain, including a branch head.
    const bridges: RuleGraphEdge[] = []
    for (const edge of next.edges.filter(edge => !removed.has(edge.source) && removed.has(edge.target) && edge.targetHandle === 'action-in')) {
        let target = edge.target
        const seen = new Set<string>()
        while (removed.has(target) && !seen.has(target)) {
            seen.add(target)
            const successor = next.edges.find(edge => edge.source === target && edge.sourceHandle === 'action-out')
            if (!successor) break
            target = successor.target
        }
        if (!removed.has(target)) bridges.push({...edge, target})
    }
    next.nodes = next.nodes.filter(node => !removed.has(node.id))
    next.edges = [...next.edges.filter(edge => !removed.has(edge.source) && !removed.has(edge.target)), ...bridges]
    next.groups = next.groups?.map(group => ({
        ...group,
        nodes: group.nodes.filter(id => !removed.has(id)),
        ports: group.ports.filter(port => !removed.has(port.node))
    })).filter(group => group.nodes.length)
    next.selected = next.selected.filter(id => !removed.has(id) && (next.nodes.some(node => node.id === id) || next.groups?.some(group => group.id === id)))
    const paths = graphPathMap(next)
    for (const node of next.nodes) if (paths[node.id] !== undefined) node.path = paths[node.id]!
    return next
}

export function duplicateGraphNodes(graph: RuleGraph, ids: string[]): RuleGraph {
    const next = copy(graph), included = graphNodeClosure(next, ids)
    const originals = next.nodes.filter(node => included.has(node.id))
    const remap = new Map(originals.map(node => [node.id, editorId()]))
    for (const original of originals) {
        const node = copy(original)
        node.id = remap.get(original.id)!
        node.position = {x: node.position.x + 60, y: node.position.y + 320}
        if (node.kind === 'action') (node.data as RuleAction).id = editorId()
        next.nodes.push(node)
    }
    next.edges.push(...graph.edges.filter(edge => included.has(edge.source) && included.has(edge.target)).map(edge => ({
        ...edge,
        id: editorId(),
        source: remap.get(edge.source)!,
        target: remap.get(edge.target)!
    })))
    next.selected = [...remap.values()]
    return next
}

export function ruleToGraph(rule: Rule, previous?: RuleGraph): RuleGraph {
    const graph: RuleGraph = {
        nodes: [],
        edges: [],
        viewport: previous?.viewport ?? {x: 0, y: 0, zoom: 1},
        selected: previous?.selected ?? [],
        layoutRestored: previous?.layoutRestored ?? false
        , groups: copy(previous?.groups ?? [])
    }
    const used = new Set<string>()

    function node(kind: RuleGraphNode['kind'], data: RuleGraphNode['data'], path: string, x: number, y: number) {
        const old = previous?.nodes.find(n => !used.has(n.id) && n.kind === kind && (kind === 'action' ? (n.data as RuleAction).id === (data as RuleAction).id && (n.path === path || !previous.nodes.some(other => other !== n && other.kind === 'action' && (other.data as RuleAction).id === (data as RuleAction).id)) : n.path === path))
        const value: RuleGraphNode = {
            id: old?.id ?? editorId(),
            kind,
            data: copy(data),
            path,
            position: old?.position ?? {x, y}
        }
        used.add(value.id);
        graph.nodes.push(value);
        return value
    }

    function condition(value: RuleCondition, path: string, x: number, y: number): {
        node: RuleGraphNode;
        bottom: number
    } {
        const current = node('condition', value, path, x, y)
        let row = y
        if (Array.isArray(value.conditions) && ['all', 'any', 'not'].includes(value.op)) {
            delete (current.data as RuleCondition).conditions
            value.conditions.forEach((child, index) => {
                const next = condition(child, `${path}/conditions/${index}`, x - ruleNodeColumnGap, row)
                graph.edges.push(graphEdge(next.node.id, current.id, 'boolean-in', index))
                row = next.bottom
            })
        }
        return {node: current, bottom: Math.max(y + defaultRowSpacing, row)}
    }

    const root = node('rule', rule, '', 560, 80)
    // Rule metadata is retained exactly; AST fields are rebuilt solely from validated edges.
    const when = condition(rule.when, '/when', 60, 80)
    graph.edges.push(graphEdge(when.node.id, root.id, 'boolean-in'))
    let predicateRow = when.bottom

    function actions(list: RuleAction[], owner: RuleGraphNode, port: RuleGraphEdge['sourceHandle'], base: string, x: number, y: number): number {
        let prior = owner, bottom = y + defaultRowSpacing
        list.forEach((action, index) => {
            const path = `${base}/${index}`
            const current = node('action', action, path, x + index * ruleNodeColumnGap, y)
            graph.edges.push({
                ...graphEdge(prior.id, current.id, 'action-in', index),
                sourceHandle: index === 0 ? port : 'action-out'
            });
            prior = current
            for (const [key, value] of Object.entries(action.params)) {
                if (value && typeof value === 'object' && typeof (value as RuleCondition).op === 'string' && graphConditionInputs(action).includes(key) && (action.type === 'array_filter' || complexGraphCondition(value))) {
                    const child = condition(value as RuleCondition, `${path}/params/${key}`, current.position.x - ruleNodeColumnGap, predicateRow)
                    graph.edges.push(graphEdge(child.node.id, current.id, `predicate:${key}`))
                    predicateRow = child.bottom
                    delete (current.data as RuleAction).params[key]
                }
            }
            let row = bottom
            for (const field of actionFlowPorts(action)) {
                const body = flowList(action, field)
                if (!Array.isArray(body)) continue
                setFlowList(current.data as RuleAction, field, [])
                if (body.length) row = actions(body, current, `flow:${field}`, `${path}/params/${field}`, current.position.x + ruleNodeColumnGap, row)
            }
            bottom = Math.max(bottom, row)
        })
        return bottom
    }

    actions(rule.actions, root, 'action-out', '/actions', 1060, 80)
    if (!previous) {
        const visible = visibleWorkflow(graph)
        let lane = defaultRowSpacing + 80
        const arrange = (owner: RuleGraphNode, port: RuleGraphEdge['sourceHandle'], x: number, y: number) => {
            let id = owner.id, handle = port, column = x
            for (; ;) {
                const edge = visible.edges.find(edge => edge.source === id && edge.sourceHandle === handle)
                if (!edge) break
                const child = graph.nodes.find(node => node.id === edge.target)!
                child.position = {x: column, y}
                for (const flow of actionFlowPorts(child.data as RuleAction)) {
                    const row = lane;
                    lane += defaultRowSpacing
                    arrange(child, `flow:${flow}`, column + ruleNodeColumnGap, row)
                }
                id = child.id;
                handle = 'action-out';
                column += ruleNodeColumnGap
            }
        }
        arrange(root, 'action-out', 1060, 80)
    }
    // Data dependencies use their own nodes and ports. Every operand occupies one
    // ordered input; expressions never turn into an editor recursively inside an action.
    function valueNode(value: unknown, mode: RuleGraphValue['mode'], path: string, owner: RuleGraphNode, input: string, y: number): number {
        const current = node('value', {
            mode,
            value: copy(value) as JsonValue
        }, path, owner.position.x - ruleNodeColumnGap, y)
        graph.edges.push({
            id: editorId(),
            source: current.id,
            target: owner.id,
            sourceHandle: 'value-out',
            targetHandle: `value:${input}`,
            order: 0
        })
        let row = y
        for (const operand of graphValueInputs(current)) row = valueNode(operand.value, graphValueMode(operand.value) ?? 'literal', `${path}${operand.path.slice(6)}`, current, operand.path, row)
        return Math.max(y + defaultRowSpacing, row)
    }

    addValueDependencies()
    function addValueDependencies() {
        // Reserve separate rows for dependency trees, so fresh graphs never overlap.
        let valueRow = Math.max(defaultRowSpacing, ...graph.nodes.map(node => node.position.y + defaultRowSpacing))
        // Value dependencies append nodes. Only initial owners may create dependencies.
        const owners = graph.nodes.filter(node => graphValueInputs(node).length > 0)
        for (const owner of owners) for (const input of graphValueInputs(owner)) {
            let mode = graphValueMode(input.value)
            if (!mode && owner.kind === 'action') mode = 'literal'
            if (!graphValueMode(input.value) && input.label === 'values') mode = 'values'
            if (mode) valueRow = valueNode(input.value, mode, `${owner.path}${input.path}`, owner, input.path, valueRow)
        }
    }
    const placed: RuleGraphNode[] = []
    for (const current of graph.nodes) {
        if (current.kind === 'action' && (current.data as RuleAction).type === 'sequence') continue
        if (!previous?.nodes.some(old => old.id === current.id)) {
            while (placed.some(other => Math.abs(current.position.x - other.position.x) < ruleNodeWidth + 30 && Math.abs(current.position.y - other.position.y) < defaultRowSpacing)) current.position.y += defaultRowSpacing
        }
        placed.push(current)
    }
    graph.groups = graph.groups?.map(group => ({
        ...group,
        nodes: group.nodes.filter(id => used.has(id)),
        ports: group.ports.filter(port => used.has(port.node))
    })).filter(group => group.nodes.length)
    return graph
}

export function graphStructureErrors(graph: RuleGraph, schema: RuleSchema = ruleSchema): RuleFieldError[] {
    const errors: RuleFieldError[] = []
    const nodes = new Map(graph.nodes.map(n => [n.id, n]))
    const error = (id: string, code: string) => errors.push({
        path: nodes.get(id)?.path ?? '',
        code: `graph.${code}`,
        message: code
    })
    if (nodes.size !== graph.nodes.length) error('', 'duplicate')
    if (graph.nodes.filter(n => n.kind === 'rule').length !== 1) error('', 'root')
    const pairs = new Set<string>()
    graph.edges.forEach(validateEdge)
    function validateEdge(edge: RuleGraphEdge) {
        const from = nodes.get(edge.source), to = nodes.get(edge.target)
        if (!from || !to) {
            error(edge.target, 'endpoint');
            return
        }
        if (from.id === to.id) error(from.id, 'cycle')
        const booleanPort = edge.sourceHandle === 'boolean-out' && from.kind === 'condition' &&
            (edge.targetHandle === 'boolean-in' && (to.kind === 'rule' || group(to)) || edge.targetHandle.startsWith('predicate:') && to.kind === 'action' && graphConditionInputs(to.data as RuleAction).includes(edge.targetHandle.slice(10)))
        const actionPort = (edge.sourceHandle === 'action-out' && ['rule', 'action'].includes(from.kind) || edge.sourceHandle.startsWith('flow:') && from.kind === 'action' && actionFlowPorts(from.data as RuleAction).includes(edge.sourceHandle.slice(5))) && edge.targetHandle === 'action-in' && to.kind === 'action'
        const valuePort = edge.sourceHandle === 'value-out' && from.kind === 'value' && edge.targetHandle.startsWith('value:') && graphValueInputs(to).some(input => `value:${input.path}` === edge.targetHandle)
        if (!booleanPort && !actionPort && !valuePort) error(to.id, 'port')
        const pair = `${edge.source}:${edge.target}:${edge.targetHandle}`
        if (pairs.has(pair)) error(to.id, 'duplicate');
        pairs.add(pair)
    }
    graph.nodes.forEach(validateNode)
    function validateNode(node: RuleGraphNode) {
        const incoming = graph.edges.filter(e => e.target === node.id)
        const outgoing = graph.edges.filter(e => e.source === node.id)
        if (new Set(outgoing.filter(edge => edge.sourceHandle !== 'value-out').map(edge => edge.sourceHandle)).size !== outgoing.filter(edge => edge.sourceHandle !== 'value-out').length) error(node.id, 'successor')
        const valueInputs = incoming.filter(edge => edge.sourceHandle === 'value-out')
        if (new Set(valueInputs.map(edge => edge.targetHandle)).size !== valueInputs.length) error(node.id, 'inputCount')
        if (graphValueInputs(node).some(input => !valueInputs.some(edge => edge.targetHandle === `value:${input.path}`))) error(node.id, 'missingValue')
        if (node.kind === 'action' && incoming.filter(e => e.targetHandle === 'action-in').length > 1) error(node.id, 'predecessor')
        const predicates = incoming.filter(edge => edge.targetHandle.startsWith('predicate:'))
        if (node.kind === 'rule' && incoming.length > 1 || node.kind === 'condition' && (node.data as RuleCondition).op === 'not' && incoming.filter(edge => edge.targetHandle === 'boolean-in').length > 1 || new Set(predicates.map(edge => edge.targetHandle)).size !== predicates.length) error(node.id, 'inputCount')
        if (node.kind === 'action') {
            const phase = (graph.nodes.find(n => n.kind === 'rule')?.data as Rule | undefined)?.phase
            const cap = schema.actions.find(c => c.id === (node.data as RuleAction).type)
            if (!cap || phase && cap.phases?.length && !cap.phases.includes(phase)) error(node.id, 'phase')
        }
    }
    const visiting = new Set<string>(), visited = new Set<string>()

    function visit(id: string, depth: number) {
        if (depth > 128) {
            error(id, 'budget');
            return
        }
        if (visiting.has(id)) {
            error(id, 'cycle');
            return
        }
        if (visited.has(id)) return
        visiting.add(id)
        for (const edge of graph.edges.filter(e => e.source === id)) visit(edge.target, depth + 1)
        visiting.delete(id);
        visited.add(id)
    }

    if (graph.nodes.length > 2048) error('', 'budget')
    else graph.nodes.forEach(n => visit(n.id, 0))
    return errors
}

export function graphToRule(graph: RuleGraph, schema: RuleSchema = ruleSchema, validate = true): Rule {
    // Stage compatibility is semantic: retain the user's new phase in the editable
    // AST while strict conversion still prevents publishing incompatible actions.
    const errors = graphStructureErrors(graph, schema).filter(error => validate || error.code !== 'graph.phase')
    if (errors.length) throw new RuleInputError(errors)
    const root = graph.nodes.find(n => n.kind === 'rule')!
    const nodes = new Map(graph.nodes.map(n => [n.id, n]))
    const visited = new Set([root.id])
    const incoming = (id: string, port: string) => graph.edges.filter(e => e.target === id && e.targetHandle === port).sort((a, b) => a.order - b.order)
    const visitValues = (id: string) => {
        for (const edge of graph.edges.filter(edge => edge.target === id && edge.sourceHandle === 'value-out')) {
            visited.add(edge.source)
            visitValues(edge.source)
        }
    }

    function condition(node: RuleGraphNode, path: string): RuleCondition {
        visited.add(node.id)
        visitValues(node.id)
        const data = graphNodeValue(graph, node.id) as RuleCondition
        if (group(node)) data.conditions = incoming(node.id, 'boolean-in').map((edge, i) => condition(nodes.get(edge.source)!, `${path}/conditions/${i}`))
        return data
    }

    const when = incoming(root.id, 'boolean-in')[0]
    if (!when) throw new RuleInputError([{path: '/when', code: 'graph.missingCondition', message: 'missingCondition'}])
    const rule = copy(root.data) as Rule
    rule.when = condition(nodes.get(when.source)!, '/when')

    function actions(owner: RuleGraphNode, handle: RuleGraphEdge['sourceHandle'], path: string): RuleAction[] {
        const list: RuleAction[] = []
        let current = owner, port = handle
        for (; ;) {
            const edge = graph.edges.find(e => e.source === current.id && e.sourceHandle === port)
            if (!edge) break
            current = nodes.get(edge.target)!;
            port = 'action-out'
            if (visited.has(current.id)) throw new RuleInputError([{path, code: 'graph.cycle', message: 'cycle'}])
            visited.add(current.id)
            visitValues(current.id)
            const action = graphNodeValue(graph, current.id) as RuleAction
            for (const child of graph.edges.filter(e => e.target === current.id && e.targetHandle.startsWith('predicate:'))) action.params[child.targetHandle.slice(10)] = condition(nodes.get(child.source)!, `${path}/${list.length}/params/${child.targetHandle.slice(10)}`)
            for (const field of actionFlowPorts(action)) if (flowList(action, field) !== undefined || graph.edges.some(e => e.source === current.id && e.sourceHandle === `flow:${field}`)) setFlowList(action, field, actions(current, `flow:${field}`, `${path}/${list.length}/params/${field}`))
            list.push(action)
        }
        return list
    }

    rule.actions = actions(root, 'action-out', '/actions')
    for (const node of graph.nodes) if (!visited.has(node.id)) errors.push({
        path: node.path,
        code: 'graph.orphan',
        message: 'orphan'
    })
    if (validate) errors.push(...validateRule(rule, schema))
    if (errors.length) throw new RuleInputError(errors)
    return rule
}

export function graphErrors(graph: RuleGraph, schema: RuleSchema = ruleSchema): RuleFieldError[] {
    try {
        graphToRule(graph, schema);
        return []
    } catch (error) {
        return error instanceof RuleInputError ? error.errors : [{
            path: '',
            code: 'graph.invalid',
            message: String(error)
        }]
    }
}

export function connectGraph(graph: RuleGraph, edge: Omit<RuleGraphEdge, 'id' | 'order'>, schema: RuleSchema = ruleSchema): RuleGraph {
    const next = copy(graph)
    next.edges.push({
        ...edge,
        id: editorId(),
        order: Math.max(-1, ...next.edges.filter(e => e.target === edge.target && e.targetHandle === edge.targetHandle).map(e => e.order)) + 1
    })
    const errors = graphStructureErrors(next, schema)
    if (errors.length) throw new RuleInputError(errors)
    return next
}

// Work on graph fragments, never rebuild the existing tree/chain from the root's stale AST.
export function addGraphModule(graph: RuleGraph, options: RuleGraphAddOptions, schema: RuleSchema = ruleSchema): RuleGraph {
    const errors = graphStructureErrors(graph, schema)
    if (errors.length) throw new RuleInputError(errors)
    const next = copy(graph), root = next.nodes.find(node => node.kind === 'rule')!
    const rule = root.data as Rule, mode = options.mode ?? 'auto'
    if (options.kind === 'value') return addValueModule(next, options)
    const fail = (code: string, node = root): never => {
        throw new RuleInputError([{path: node.path, code: `graph.${code}`, message: code}])
    }
    validateOptions()
    const paths = graphPathMap(next)
    const explicitTarget = options.targetId === undefined ? undefined : next.nodes.find(node => node.id === options.targetId)
    if (options.targetId !== undefined && !explicitTarget) fail('target')
    // Detached creation has no connection target; auto/wrap may never attach to an orphan.
    if (mode !== 'detached' && explicitTarget && paths[explicitTarget.id] === undefined) fail('orphan', explicitTarget)
    function validateOptions() {
        if (!['auto', 'detached', 'wrap'].includes(mode)) fail('mode')
        if (!['condition', 'action'].includes(options.kind)) fail('capability')
        const capability = (options.kind === 'action' ? schema.actions : schema.conditions).find(cap => cap.id === options.type)
        if (!capability) fail('capability')
        if (capability?.phases?.length && !capability.phases.includes(rule.phase)) fail('phase')
        if (options.position && (!validCoordinate(options.position.x) || !validCoordinate(options.position.y))) fail('position')
        if (mode === 'wrap' && (options.kind !== 'condition' || !['all', 'any', 'not'].includes(options.type))) fail('mode')
    }
    const children = (node: RuleGraphNode, port = 'boolean-in') => next.edges.filter(edge => edge.target === node.id && edge.targetHandle === port).sort((a, b) => a.order - b.order)
    const parentEdge = (node: RuleGraphNode) => {
        const edges = next.edges.filter(edge => edge.source === node.id && edge.sourceHandle === 'boolean-out')
        if (edges.length !== 1) fail('target', node)
        return edges[0]!
    }
    const freePosition = (position: RuleGraphNode['position']) => {
        const result = {...position}
        while (next.nodes.some(node => Math.abs(node.position.x - result.x) < ruleNodeColumnGap && Math.abs(node.position.y - result.y) < defaultRowSpacing)) result.y += defaultRowSpacing
        return result
    }
    const positionFor = (position: RuleGraphNode['position']) => options.position ? {...options.position} : freePosition(position)
    const addFragment = (kind: RuleGraphAddOptions['kind'], value: RuleCondition | RuleAction, position: RuleGraphNode['position']) => {
        const fragment = ruleToGraph(kind === 'condition' ? {
            ...rule,
            when: value as RuleCondition,
            actions: []
        } : {...rule, when: {op: 'always'}, actions: [value as RuleAction]})
        const base = kind === 'condition' ? '/when' : '/actions/0'
        const included = fragment.nodes.filter(node => node.path === base || node.path.startsWith(`${base}/`))
        const ids = new Set(included.map(node => node.id)), main = included.find(node => node.path === base)!
        const edges = fragment.edges.filter(edge => ids.has(edge.source) && ids.has(edge.target))
        const arrange = (node: RuleGraphNode, x: number, y: number): number => {
            // Keep an explicit main position exact; generated descendants avoid existing cards too.
            node.position = node === main ? {x, y} : freePosition({x, y})
            let row = node.position.y + (node.kind === 'action' ? defaultRowSpacing : 0)
            for (const edge of edges.filter(edge => edge.target === node.id).sort((a, b) => a.order - b.order)) row = arrange(included.find(child => child.id === edge.source)!, x - ruleNodeColumnGap, row)
            return Math.max(node.position.y + defaultRowSpacing, row)
        }
        arrange(main, position.x, position.y)
        next.nodes.push(...included);
        next.edges.push(...edges)
        return main
    }
    const value = options.kind === 'action' ? newAction(options.type, schema) : defaultCondition(options.type, schema)
    const addCondition = (position: RuleGraphNode['position']) => addFragment('condition', value, positionFor(position))
    const finish = (node: RuleGraphNode) => {
        const errors = graphStructureErrors(next, schema)
        if (errors.length) throw new RuleInputError(errors)
        const paths = graphPathMap(next)
        for (const node of next.nodes) if (paths[node.id] !== undefined) node.path = paths[node.id]!
        next.selected = [node.id]
        return next
    }
    if (mode === 'detached') return finish(addFragment(options.kind, value, positionFor({
        x: root.position.x + (options.kind === 'action' ? ruleNodeColumnGap : -ruleNodeColumnGap),
        y: root.position.y + defaultRowSpacing
    })))
    if (options.kind === 'action') return addActionModule()
    return addConditionModule()

    function addActionModule(): RuleGraph {
        let target = explicitTarget ?? root
        if (target.kind !== 'rule' && target.kind !== 'action') fail('target', target)
        if (!explicitTarget) {
            // Multiple disconnected action chains have no unambiguous append destination.
            if (next.nodes.some(node => node.kind === 'action' && paths[node.id] === undefined)) fail('orphan')
            for (; ;) {
                const edge = next.edges.find(edge => edge.source === target.id && edge.sourceHandle === 'action-out')
                if (!edge) break
                target = next.nodes.find(node => node.id === edge.target)!
            }
        }
        const sourceHandle = options.sourceHandle ?? 'action-out'
        const successor = next.edges.find(edge => edge.source === target.id && edge.sourceHandle === sourceHandle)
        const node = addFragment('action', value, positionFor({
            x: target.position.x + ruleNodeColumnGap,
            y: target.position.y
        }))
        const order = successor?.order ?? (children(target, 'action-in')[0]?.order ?? -1) + 1
        if (successor) {
            successor.source = node.id;
            successor.sourceHandle = 'action-out'
        }
        next.edges.push({...graphEdge(target.id, node.id, 'action-in', order), sourceHandle})
        return finish(node)
    }

    function wrapCondition(target: RuleGraphNode): RuleGraph {
        if (target.kind === 'rule') {
            const edge = children(target)[0]
            if (!edge) fail('missingCondition', target)
            target = next.nodes.find(node => node.id === edge.source)!
        }
        if (target.kind !== 'condition') fail('target', target)
        const parent = parentEdge(target)
        // A wrapper takes the existing subtree, not defaultCondition's placeholder child.
        const wrapper = addFragment('condition', {
            ...value,
            conditions: []
        } as RuleCondition, positionFor({x: target.position.x + ruleNodeColumnGap, y: target.position.y}))
        parent.source = wrapper.id
        next.edges.push(graphEdge(target.id, wrapper.id, 'boolean-in'))
        return finish(wrapper)
    }

    function preserveNodeIdentity(node: RuleGraphNode, id: string) {
        const generatedId = node.id
        node.id = id
        for (const edge of next.edges) {
            if (edge.source === generatedId) edge.source = id
            if (edge.target === generatedId) edge.target = id
        }
    }

    function resolveConditionTarget(target: RuleGraphNode): {target: RuleGraphNode; done?: RuleGraph} {
        let port = 'boolean-in'
        if (target.kind !== 'rule') {
            if ((target.data as RuleAction).type !== 'array_filter') fail('target', target)
            port = 'predicate:predicate'
        }
        const existing = children(target, port)[0]
        if (!existing) {
            const node = addCondition({
                x: target.position.x - ruleNodeColumnGap,
                y: target.position.y + (target.kind === 'action' ? defaultRowSpacing : 0)
            })
            next.edges.push(graphEdge(node.id, target.id, port))
            return {target, done: finish(node)}
        }
        target = next.nodes.find(node => node.id === existing.source)!
        if ((target.data as RuleCondition).op === 'always') {
            const node = addFragment('condition', value, options.position ? {...options.position} : {...target.position})
            preserveNodeIdentity(node, target.id)
            next.nodes = next.nodes.filter(old => old !== target)
            return {target, done: finish(node)}
        }
        return {target}
    }

    function addConditionModule(): RuleGraph {
        let target = explicitTarget ?? root
        if (mode === 'wrap') return wrapCondition(target)
        if (target.kind === 'condition' && (['all', 'any'].includes((target.data as RuleCondition).op) || (target.data as RuleCondition).op === 'not' && !children(target).length)) {
            const incoming = children(target)
            const node = addCondition({
                x: target.position.x - ruleNodeColumnGap,
                y: target.position.y + incoming.length * defaultRowSpacing
            })
            next.edges.push(graphEdge(node.id, target.id, 'boolean-in', Math.max(-1, ...incoming.map(edge => edge.order)) + 1))
            return finish(node)
        }
        if (target.kind !== 'condition') {
            const resolved = resolveConditionTarget(target)
            if (resolved.done) return resolved.done
            target = resolved.target
        }
        // Adding beside a leaf/not must not change its subtree or turn NOT(A) into NOT(A AND B).
        const parent = parentEdge(target)
        if (!schema.conditions.some(cap => cap.id === 'all')) fail('capability')
        const node = addCondition({x: target.position.x, y: target.position.y + defaultRowSpacing})
        const wrapper = addFragment('condition', {
            ...defaultCondition('all', schema),
            conditions: []
        }, freePosition({x: target.position.x + ruleNodeColumnGap, y: target.position.y}))
        parent.source = wrapper.id
        next.edges.push(graphEdge(target.id, wrapper.id, 'boolean-in', 0), graphEdge(node.id, wrapper.id, 'boolean-in', 1))
        return finish(node)
    }
}

function initialGraphValue(type: string): JsonValue {
    if (type === 'reference') return {$ref: {source: 'current', path: ''}}
    if (type === 'literal') return null
    return {$expr: {op: type, args: []}}
}

function addValueModule(next: RuleGraph, options: RuleGraphAddOptions): RuleGraph {
    const owner = next.nodes.find(node => node.id === options.targetId)
    if (!owner || !options.inputPath) throw new RuleInputError([{
        path: '',
        code: 'graph.target',
        message: 'target'
    }])
    const data = graphNodeValue(next, owner.id)
    const value = initialGraphValue(options.type)
    writeGraphInput(data, options.inputPath, value)
    const result = replaceGraphNode(next, owner.id, data)
    const edge = result.edges.find(edge => edge.target === owner.id && edge.targetHandle === `value:${options.inputPath}`)
    if (edge) {
        result.selected = [edge.source]
        if (options.position) result.nodes.find(node => node.id === edge.source)!.position = {...options.position}
    }
    return result
}

// Coordinates, viewport, editor IDs and selection never affect wire identity or dirty state.
export function graphSignature(graph: RuleGraph): string {
    try {
        return JSON.stringify(graphToRule(graph, ruleSchema, false))
    } catch { /* retain invalid drafts, including all orphan nodes */
    }
    return JSON.stringify({
        nodes: graph.nodes.map(n => ({
            id: n.id,
            kind: n.kind,
            data: n.data
        })).sort((a, b) => a.id.localeCompare(b.id)),
        edges: graph.edges.map(({source, target, sourceHandle, targetHandle, order}) => ({
            source,
            target,
            sourceHandle,
            targetHandle,
            order
        })).sort((a, b) => JSON.stringify(a).localeCompare(JSON.stringify(b)))
    })
}

export function graphNodeValue(graph: RuleGraph, id: string, seen = new Set<string>()): RuleGraphNode['data'] {
    const node = graph.nodes.find(n => n.id === id)!
    const data = copy(node.data)
    if (seen.has(id)) return data
    seen.add(id)
    for (const edge of graph.edges.filter(edge => edge.target === id && edge.sourceHandle === 'value-out')) {
        writeGraphInput(data, edge.targetHandle.slice(6), (graphNodeValue(graph, edge.source, new Set(seen)) as RuleGraphValue).value)
    }
    if (group(node)) (data as RuleCondition).conditions = graph.edges.filter(e => e.target === id && e.targetHandle === 'boolean-in').sort((a, b) => a.order - b.order).map(e => graphNodeValue(graph, e.source, new Set(seen)) as RuleCondition)
    if (node.kind === 'action') {
        for (const edge of graph.edges.filter(e => e.target === id && e.targetHandle.startsWith('predicate:'))) (data as RuleAction).params[edge.targetHandle.slice(10)] = graphNodeValue(graph, edge.source, new Set(seen))
        restoreActionFlows(graph, id, data as RuleAction, seen)
    }
    return data
}

function restoreActionFlows(graph: RuleGraph, id: string, data: RuleAction, seen: Set<string>): void {
    for (const field of actionFlowPorts(data as RuleAction)) {
        const list: RuleAction[] = []
        let current = id, handle: RuleGraphEdge['sourceHandle'] = `flow:${field}`
        const scopeSeen = new Set(seen)
        for (; ;) {
            const edge = graph.edges.find(e => e.source === current && e.sourceHandle === handle)
            if (!edge || scopeSeen.has(edge.target)) break
            list.push(graphNodeValue(graph, edge.target, scopeSeen) as RuleAction)
            current = edge.target;
            handle = 'action-out';
            scopeSeen.add(current)
        }
        if (flowList(data as RuleAction, field) !== undefined || list.length) setFlowList(data as RuleAction, field, list)
    }
}

export function replaceGraphNode(graph: RuleGraph, id: string, data: RuleGraphNode['data']): RuleGraph {
    const next = copy(graph), node = next.nodes.find(n => n.id === id)!
    if (node.kind === 'value') {
        const inputs = graphValueInputs({kind: 'value', data}), oldInputs = graphValueInputs(node)
        node.data = copy(data)
        if (JSON.stringify(inputs) === JSON.stringify(oldInputs)) return next
        // Rebuild a connected data subtree from its current values, retaining the
        // complete flow AST and all unchanged node positions/identities.
        const rule = graphToRule(graph, ruleSchema, false)
        const path = graphPathMap(graph)[id]
        if (!path) return next
        writeGraphInput(rule, path, (data as RuleGraphValue).value)
        const rebuilt = ruleToGraph(rule, next)
        rebuilt.selected = next.selected
        return rebuilt
    }
    if (node.kind === 'rule') {
        node.data = copy(data);
        return next
    }
    const removed = new Set<string>()
    const collect = (parent: string) => {
        for (const edge of next.edges.filter(e => e.target === parent && ['boolean-out', 'value-out'].includes(e.sourceHandle))) if (!removed.has(edge.source)) {
            removed.add(edge.source);
            collect(edge.source)
        }
    }
    collect(id)
    const collectFlow = (parent: string) => {
        for (const edge of next.edges.filter(e => e.source === parent && (e.sourceHandle.startsWith('flow:') || removed.has(parent) && e.sourceHandle === 'action-out'))) if (!removed.has(edge.target)) {
            removed.add(edge.target);
            collect(edge.target);
            collectFlow(edge.target)
        }
    }
    if (node.kind === 'action') collectFlow(id)
    const rule = next.nodes.find(n => n.kind === 'rule')!.data as Rule
    const paths = graphPathMap(graph), basePath = paths[id] ?? node.path
    const fragmentPath = node.kind === 'condition' ? '/when' : '/actions/0'
    const previous = {
        ...graph,
        nodes: graph.nodes.filter(n => n.id === id || removed.has(n.id)).map(n => ({
            ...n,
            path: fragmentPath + (paths[n.id] ?? n.path).slice(basePath.length)
        }))
    }
    const fragment = ruleToGraph(node.kind === 'condition' ? {
        ...rule,
        when: data as RuleCondition,
        actions: []
    } : {...rule, when: {op: 'always'}, actions: [data as RuleAction]}, previous)
    const source = fragment.nodes.find(n => node.kind === 'condition' ? n.path === '/when' : n.kind === 'action')!
    const included = fragment.nodes.filter(n => node.kind === 'condition' ? n.path.startsWith('/when') : n.path.startsWith('/actions/0'))
    const allowed = new Set(included.map(n => n.id))
    const remap = new Map(included.map(n => {
        if (n.id === source.id) return [n.id, id]
        const conflict = next.nodes.some(old => old.id === n.id && !removed.has(old.id))
        return [n.id, conflict ? editorId() : n.id]
    }))
    const replacements = new Map(included.map(n => {
        const replacement = {...n, id: remap.get(n.id)!, position: n.id === source.id ? node.position : n.position}
        return [replacement.id, replacement]
    }))
    // Reuse original array slots so editing a focused inline control does not move its DOM node.
    next.nodes = next.nodes.flatMap(old => {
        const replacement = replacements.get(old.id)
        if (replacement) {
            replacements.delete(old.id);
            return [replacement]
        }
        return old.id === id || removed.has(old.id) ? [] : [old]
    })
    next.nodes.push(...replacements.values())
    next.edges = next.edges.filter(e => !removed.has(e.source) && !removed.has(e.target))
    next.edges.push(...fragment.edges.filter(e => allowed.has(e.source) && allowed.has(e.target)).map(e => ({
        ...e,
        source: remap.get(e.source)!,
        target: remap.get(e.target)!
    })))
    return next
}

export function graphPathMap(graph: RuleGraph): Record<string, string> {
    const result: Record<string, string> = {}
    const root = graph.nodes.find(n => n.kind === 'rule')
    if (!root) return result
    const visited = new Set<string>()
    const mapCondition = (id: string, path: string) => {
        if (visited.has(id)) return
        visited.add(id);
        result[id] = path
        graph.edges.filter(e => e.target === id && e.targetHandle === 'boolean-in').sort((a, b) => a.order - b.order).forEach((e, i) => mapCondition(e.source, `${path}/conditions/${i}`))
    }
    result[root.id] = ''
    const when = graph.edges.find(e => e.target === root.id && e.targetHandle === 'boolean-in')
    if (when) mapCondition(when.source, '/when')
    const mapActions = (owner: string, handle: RuleGraphEdge['sourceHandle'], base: string) => {
        let id = owner, port = handle, index = 0
        for (; ;) {
            const edge = graph.edges.find(e => e.source === id && e.sourceHandle === port)
            if (!edge || visited.has(edge.target)) break
            id = edge.target;
            port = 'action-out';
            visited.add(id)
            const path = `${base}/${index++}`
            result[id] = path
            graph.edges.filter(e => e.target === id && e.targetHandle.startsWith('predicate:')).forEach(e => mapCondition(e.source, `${path}/params/${e.targetHandle.slice(10)}`))
            const node = graph.nodes.find(node => node.id === id)!
            for (const field of actionFlowPorts(node.data as RuleAction)) mapActions(id, `flow:${field}`, `${path}/params/${field}`)
        }
    }
    mapActions(root.id, 'action-out', '/actions')
    const mapValues = (id: string) => {
        for (const edge of graph.edges.filter(edge => edge.target === id && edge.sourceHandle === 'value-out')) {
            if (visited.has(edge.source)) continue
            visited.add(edge.source)
            const target = graph.nodes.find(node => node.id === id)!
            const input = edge.targetHandle.slice(6)
            result[edge.source] = `${result[id]}${target.kind === 'value' ? input.slice(6) : input}`
            mapValues(edge.source)
        }
    }
    for (const id of Object.keys(result)) mapValues(id)
    return result
}
