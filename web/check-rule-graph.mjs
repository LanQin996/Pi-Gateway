import assert from 'node:assert/strict'
import {build} from 'esbuild'

export async function checkRuleGraph(schema, backend) {
    const bundled = await build({
        stdin: {
            contents: "export * from './src/utils/ruleGraph'; export * from './src/utils/ruleEditor'; export * from './src/utils/ruleSchema'; export * from './src/utils/ruleLabels'; export * from './src/utils/samplePaths';",
            resolveDir: process.cwd(),
            loader: 'ts'
        }, bundle: true, write: false, format: 'esm', platform: 'node'
    })
    const m = await import(`data:text/javascript;base64,${Buffer.from(bundled.outputFiles[0].text).toString('base64')}`)
    const fixtures = [...(schema.examples ?? [])]
    const special = {
        ...m.newRule(),
        id: 'persisted',
        revision: 7,
        order_index: 0,
        source: 'legacy',
        legacy_name: 'original-user-name',
        name: 'do not translate',
        when: {
            op: 'all',
            conditions: [{op: 'eq', value: null}, {
                op: 'any',
                conditions: [{
                    op: 'eq',
                    source: 'context',
                    path: '/model',
                    value: {$ref: {source: 'client', path: '/model'}}
                }, {op: 'not', conditions: [{op: 'eq', value: false}]}]
            }, {op: 'eq', value: 0}]
        },
        actions: [{
            id: 'keep-this-id',
            type: 'json_set',
            params: {path: '/x', value: {$literal: {$ref: {source: 'context', path: '/model'}}}, create_parents: false}
        }, {
            id: 'filter-id',
            type: 'array_filter',
            params: {
                path: '/input',
                predicate: {
                    op: 'not',
                    conditions: [{
                        op: 'all',
                        conditions: [{op: 'eq', source: 'item', value: null}, {op: 'eq', source: 'item', value: 0}]
                    }]
                }
            }
        }, {
            id: 'another-id',
            type: 'json_set',
            params: {path: '/empty', value: {nested: [false, null, 0, '', {$ref: {source: 'context', path: '/model'}}]}}
        }]
    }
    fixtures.push(special)
    checkFixtures()

    function checkFixtures() {
        for (const cap of schema.conditions) {
            const when = m.defaultCondition(cap.id, schema)
            if (cap.id === 'regex' || cap.id === 'not_regex') when.value = 'test'
            fixtures.push({...m.newRule(), name: cap.id, when})
        }
        for (const cap of schema.actions) {
            const action = m.newAction(cap.id, schema)
            if (cap.id === 'rewrite_model') action.params.model = 'test'
            if (cap.id === 'text_replace') action.params.pattern = 'test'
            if (cap.id === 'json_merge') action.params.value = {a: null}
            if (cap.id === 'scope') action.params.functions = {}
            if (cap.id === 'call') continue // A call requires its enclosing scope; tested below through nested fixtures
            fixtures.push({...m.newRule(), name: cap.id, phase: cap.phases[0], actions: [action]})
        }
        fixtures.push({
            ...m.newRule(), name: 'nested-fragment', phase: 'upstream_headers', actions: [{
                id: 'scope',
                type: 'scope',
                params: {
                    functions: {
                        metadata: [{
                            id: 'agent',
                            type: 'json_set',
                            params: {path: '/user-agent', value: ['example']}
                        }]
                    }, steps: [{id: 'invoke', type: 'call', params: {name: 'metadata'}}]
                }
            }]
        })
        checkFixtureRoundtrips()

        function checkFixtureRoundtrips() {
            for (const rule of fixtures) {
                const graph = m.ruleToGraph(rule)
                assert.deepEqual(m.graphToRule(graph, schema), rule, `${rule.name}: graph changed wire AST`)
                const again = m.ruleToGraph(rule, graph)
                assert.deepEqual(again.nodes.map(n => n.id), graph.nodes.map(n => n.id), 'stable editor identities')
                for (let i = 0; i < graph.nodes.length; i++) for (const other of graph.nodes.slice(i + 1)) assert.ok(Math.abs(graph.nodes[i].position.x - other.position.x) >= 360 || Math.abs(graph.nodes[i].position.y - other.position.y) >= 300, 'fresh layout must leave room for full-size cards')
                const manual = structuredClone(graph);
                manual.nodes.forEach((node, index) => node.position = {x: index * 17, y: index * -23})
                assert.deepEqual(m.ruleToGraph(rule, manual).nodes.map(node => node.position), manual.nodes.map(node => node.position), 'new defaults never move a previous layout')
                assert.ok(!JSON.stringify(m.graphToRule(graph, schema)).includes('viewport'))
                const draft = m.createDraft(rule)
                draft.graph.nodes[0].position = {x: 99, y: 88};
                draft.graph.viewport = {x: 12, y: 34, zoom: .5};
                draft.graph.selected = [draft.graph.nodes[0].id]
                assert.equal(m.draftDirty(draft), false, 'layout does not change persistence')
            }
        }
    }
    // Black boxes preserve execution AST and map typed boundary ports back to real nodes.
    const groupingRule = {
        ...m.newRule(),
        when: {op: 'always'},
        actions: [0, 1, 2].map(i => ({id: `group-action-${i}`, type: 'json_set', params: {path: `/x${i}`, value: i}}))
    }
    const groupingGraph = m.ruleToGraph(groupingRule)
    const groupActions = groupingGraph.nodes.filter(n => n.kind === 'action')
    const boxed = m.groupGraphNodes(groupingGraph, groupActions.slice(0, 2).map(n => n.id), 'Cleanup')
    const box = boxed.groups[0], ports = m.graphGroupPorts(boxed, box)
    assert.equal(ports.filter(p => p.direction === 'input').length, 1)
    assert.equal(ports.filter(p => p.direction === 'output').length, 1)
    const outer = m.groupedWorkflow(boxed)
    assert.equal(outer.nodes.filter(n => n.kind === 'action').length, 1)
    assert.equal(m.groupedWorkflow(boxed, box.id).nodes.filter(n => n.kind === 'action').length, 2)
    for (const edge of outer.edges) assert.ok(boxed.edges.some(real => {
        const resolved = m.resolveGroupConnection(boxed, edge);
        return real.source === resolved.source && real.target === resolved.target && real.sourceHandle === resolved.sourceHandle && real.targetHandle === resolved.targetHandle
    }))
    const port = ports[0]
    box.ports = box.ports.filter(p => p.id !== port.id);
    box.ports.push({...port, name: 'Request input'})
    const restoredBox = m.applyGraphLayout(m.ruleToGraph(groupingRule), m.graphLayoutSnapshot(boxed))
    assert.equal(restoredBox.groups[0].ports.find(p => p.id === port.id).name, 'Request input')
    assert.deepEqual(m.graphToRule(restoredBox, schema), groupingRule)
    const removedBox = m.removeGraphNodes(boxed, groupActions.slice(0, 2).map(n => n.id))
    assert.equal(removedBox.groups.length, 0)
    assert.doesNotThrow(() => m.graphLayoutSnapshot(removedBox))
    assert.deepEqual(m.graphToRule({...boxed, groups: []}, schema), groupingRule)
    console.log('PASS: group boundary inputs/outputs, internal view, external connection mapping, renamed-port layout restore, deletion cleanup and AST preservation')
    const graph = m.ruleToGraph(special)
    const actions = graph.nodes.filter(n => n.kind === 'action'), root = graph.nodes.find(n => n.kind === 'rule'),
        leaf = graph.nodes.find(n => n.kind === 'condition' && n.data.op === 'eq')
    assert.equal(m.graphPathMap(graph)[actions[1].id], '/actions/1')
    const filter = m.graphNodeValue(graph, actions[1].id)
    assert.deepEqual(filter, special.actions[1], 'array predicate subtree rebuild')
    const edited = m.replaceGraphNode(graph, actions[1].id, {
        ...filter,
        params: {...filter.params, predicate: {op: 'eq', source: 'item', value: false}}
    })
    assert.equal(edited.nodes.find(n => n.id === actions[1].id).data.id, 'filter-id')
    assert.deepEqual(m.graphToRule(edited, schema).actions[1].params.predicate, {
        op: 'eq',
        source: 'item',
        value: false
    })
    const changedLeaf = m.replaceGraphNode(graph, leaf.id, {...leaf.data, value: false})
    assert.equal(changedLeaf.nodes.find(n => n.id === leaf.id).id, leaf.id)
    assert.equal(m.graphToRule(changedLeaf, schema).when.conditions[0].value, false)
    assert.deepEqual(changedLeaf.nodes.map(node => node.id), graph.nodes.map(node => node.id), 'editing the first condition preserves its array slot and every unchanged node')
    const changedFirstAction = m.replaceGraphNode(graph, actions[0].id, {
        ...actions[0].data,
        params: {...actions[0].data.params, value: 'inline edit'}
    })
    assert.deepEqual(changedFirstAction.nodes.map(node => node.id), graph.nodes.map(node => node.id), 'editing the first action preserves node order and focus')
    assert.equal(m.graphToRule(changedFirstAction, schema).actions[0].params.value, 'inline edit')
    const rootConditionNode = graph.nodes.find(node => node.path === '/when')
    const changedGroup = m.replaceGraphNode(graph, rootConditionNode.id, {
        ...special.when,
        conditions: special.when.conditions.map((condition, index) => index ? condition : {
            ...condition,
            value: 'group edit'
        })
    })
    assert.deepEqual(changedGroup.nodes.map(node => node.id), graph.nodes.map(node => node.id), 'unchanged condition structure retains all descendant slots')
    assert.equal(m.graphToRule(changedGroup, schema).when.conditions[0].value, 'group edit')
    const updatedPredicate = {
        op: 'any',
        conditions: [{op: 'not', conditions: [{op: 'eq', source: 'item', path: '/enabled', value: false}]}, {
            op: 'all',
            conditions: [{op: 'eq', source: 'item', value: 0}, {op: 'exists', source: 'item', path: '/keep'}]
        }]
    }
    const complex = m.replaceGraphNode(graph, actions[1].id, {
        ...filter,
        params: {...filter.params, predicate: updatedPredicate}
    })
    const originalIds = new Set(graph.nodes.map(node => node.id)),
        retainedIds = new Set(complex.nodes.filter(node => originalIds.has(node.id)).map(node => node.id))
    assert.deepEqual(complex.nodes.filter(node => originalIds.has(node.id)).map(node => node.id), graph.nodes.filter(node => retainedIds.has(node.id)).map(node => node.id), 'reused predicate descendants keep their original relative slots')
    assert.ok(complex.nodes.slice(0, retainedIds.size).every(node => originalIds.has(node.id)), 'new predicate nodes are appended after retained nodes')
    assert.ok(graph.nodes.some(node => !retainedIds.has(node.id)), 'obsolete predicate children are removed')
    assert.deepEqual(m.graphToRule(complex, schema), {
        ...special,
        actions: special.actions.map((action, index) => index === 1 ? {
            ...action,
            params: {...action.params, predicate: updatedPredicate}
        } : action)
    }, 'complex predicate changes still roundtrip without losing other subtrees/actions')
    assert.deepEqual(m.graphToRule(graph, schema), special, 'inline replacements never mutate their input graph')
    const orphan = structuredClone(graph);
    orphan.nodes.push({...structuredClone(leaf), id: 'orphan'})
    assert.ok(m.graphErrors(orphan, schema).some(e => e.code === 'graph.orphan'))
    const draft = m.createDraft(special);
    draft.graph = orphan;
    assert.equal(m.draftDirty(draft), true, 'invalid graph draft is dirty')
    assert.throws(() => m.graphToRule(orphan, schema))
    const wrong = structuredClone(graph);
    wrong.edges[0].sourceHandle = 'action-out'
    assert.ok(m.graphErrors(wrong, schema).some(e => e.code === 'graph.port'))
    const cycle = structuredClone(graph);
    cycle.edges.push(m.graphEdge(actions.at(-1).id, actions[0].id, 'action-in'))
    assert.ok(m.graphErrors(cycle, schema).some(e => e.code === 'graph.cycle'))
    assert.throws(() => m.connectGraph(graph, {
        source: root.id,
        target: root.id,
        sourceHandle: 'action-out',
        targetHandle: 'action-in'
    }, schema))
    assert.throws(() => m.connectGraph(graph, {
        source: leaf.id,
        target: actions[0].id,
        sourceHandle: 'boolean-out',
        targetHandle: 'action-in'
    }, schema))
    const phase = m.ruleToGraph({
        ...m.newRule(),
        name: 'phase',
        phase: 'response_body',
        actions: [m.newAction('rewrite_model', schema)]
    })
    assert.ok(m.graphErrors(phase, schema).some(e => e.code === 'graph.phase'))
    const order = structuredClone(graph)
    const children = order.edges.filter(e => e.target === graph.edges.find(e => e.target === root.id).source)
    children.forEach((edge, index) => edge.order = children.length - index)
    assert.deepEqual(m.graphToRule(order, schema).when.conditions, [...special.when.conditions].reverse())
    checkQuickAdd(m, schema)
    checkNestedWorkflow(m, schema)
    // Layout persistence only contains stable AST/action identities and geometry, never rule/sample values.
    const storageDescriptor = Object.getOwnPropertyDescriptor(globalThis, 'localStorage'), stored = new Map()
    try {
        Object.defineProperty(globalThis, 'localStorage', {
            configurable: true,
            value: {getItem: key => stored.get(key) ?? null, setItem: (key, value) => stored.set(key, value)}
        })
        const layoutRule = {
            ...special,
            id: 'layout-check',
            name: 'secret-name',
            actions: special.actions.map(a => ({...a, params: {...a.params, value: 'sample-token-secret'}}))
        }
        const positioned = m.ruleToGraph(layoutRule)
        positioned.nodes.forEach((n, i) => n.position = {x: 100 + i * 55, y: 240 + i * 70});
        positioned.viewport = {x: -33, y: 22, zoom: .62}
        m.persistGraphLayout(positioned)
        const serialized = stored.get('pi-rule-layout:layout-check')
        assert.ok(serialized)
        for (const secret of ['secret-name', 'sample-token-secret', '"params":', '/model', '"value":']) assert.ok(!serialized.includes(secret), `layout leaks ${secret}`)
        assert.deepEqual(Object.keys(JSON.parse(serialized)), ['signature', 'nodes', 'viewport'])
        for (const position of Object.values(JSON.parse(serialized).nodes)) assert.deepEqual(Object.keys(position), ['x', 'y'])
        const restored = m.restoreGraphLayout(m.ruleToGraph(layoutRule))
        assert.deepEqual(restored.nodes.map(n => n.position), positioned.nodes.map(n => n.position))
        assert.deepEqual(restored.viewport, positioned.viewport)
        assert.equal(restored.layoutRestored, true)
        const changedParams = m.ruleToGraph({...layoutRule, name: 'renamed', revision: 99})
        assert.equal(m.graphLayoutSignature(changedParams), m.graphLayoutSignature(positioned))
        assert.deepEqual(m.restoreGraphLayout(changedParams).viewport, positioned.viewport)
        const changedStructure = m.ruleToGraph({...layoutRule, when: {op: 'always'}})
        assert.notEqual(m.graphLayoutSignature(changedStructure), m.graphLayoutSignature(positioned))
        assert.deepEqual(m.restoreGraphLayout(changedStructure).nodes.map(n => n.position), changedStructure.nodes.map(n => n.position))
        const malformed = JSON.parse(serialized);
        malformed.viewport.zoom = 0;
        stored.set('pi-rule-layout:layout-check', JSON.stringify(malformed))
        assert.equal(m.restoreGraphLayout(m.ruleToGraph(layoutRule)).layoutRestored, false)
        Object.defineProperty(globalThis, 'localStorage', {
            configurable: true, get() {
                throw new Error('unavailable')
            }
        })
        positioned.nodes[0].position.x = 444;
        m.persistGraphLayout(positioned)
        assert.equal(m.restoreGraphLayout(m.ruleToGraph(layoutRule)).nodes[0].position.x, 444, 'memory fallback when storage getter throws')
        const fresh = m.ruleToGraph({...layoutRule, id: undefined});
        m.persistGraphLayout(fresh)
        assert.equal(m.restoreGraphLayout(fresh), fresh, 'unsaved rules are never cached')
    } finally {
        if (storageDescriptor) Object.defineProperty(globalThis, 'localStorage', storageDescriptor); else delete globalThis.localStorage
    }
    const payload = {
        input: [{content: 'private text', nullable: null, flag: false, count: 0}],
        metadata: {'a/b~c': 'private token', '': []}
    }
    const options = m.samplePathOptions(payload)
    assert.ok(options.some(o => o.path === '/input/0/content'))
    assert.ok(options.some(o => o.path === '/metadata/a~1b~0c'))
    assert.ok(options.some(o => o.path === '/metadata/'))
    assert.ok(!JSON.stringify(options).includes('private'))
    assert.deepEqual(m.samplePathOptions(null), [{path: '', label: '/'}])
    assert.equal(m.samplePathOptions(Array.from({length: 1000}, () => ({x: true}))).length, 256)
    assert.equal(m.samplePathOptions({a: {b: {c: 0}}}, {maxDepth: 1}).length, 2)
    const cyclic = {};
    cyclic.self = cyclic;
    assert.ok(m.samplePathOptions(cyclic).length <= 9)
    const pathField = schema.conditions.find(c => c.id === 'exists').fields.find(f => f.name === 'path')
    for (const source of [undefined, 'current']) assert.equal(m.currentSamplePathField(pathField, source), true)
    for (const source of ['item', 'client', 'context', null]) assert.equal(m.currentSamplePathField(pathField, source), false)
    assert.equal(m.currentSamplePathField(schema.actions.find(c => c.id === 'let').fields.find(f => f.name === 'name'), 'current'), false)
    for (const locale of ['en', 'zh-CN']) {
        const legacy = {name: 'drop_environment_context', legacy_name: 'drop_environment_context', source: 'legacy'}
        assert.equal(m.ruleDisplayName(legacy, locale), m.ruleLabel('action', 'drop_environment_context', locale))
        assert.equal(m.ruleDisplayName({...legacy, name: 'Custom user name'}, locale), 'Custom user name')
        assert.equal(m.ruleDisplayName({...legacy, source: 'user'}, locale), 'drop_environment_context')
        assert.equal(m.ruleDisplayName({
            name: 'drop_fields',
            legacy_name: 'drop_fields',
            source: 'legacy'
        }, locale), m.ruleLabel('action', 'json_remove', locale))
        assert.equal(m.ruleDisplayName({
            name: 'block_prompt',
            legacy_name: 'block_prompt',
            source: 'legacy'
        }, locale), m.ruleLabel('action', 'reject_request', locale))
    }
    console.log('PASS: bounded sample pointers/current-source gating; coordinate-only cache/restoration/shape mismatch/memory fallback; migration names preserve custom input')
    const labels = checkCatalogLabels(m, schema, backend)
    console.log(`PASS: ${fixtures.length} lossless graph roundtrips, graph invariants/orphans/cycles/ports/phases/order/IDs/dirty state, ${labels} catalog bilingual labels`)
}

function checkCatalogLabels(m, schema, backend) {
    // Coverage is against the authoritative backend catalog, not equality of locale key counts.
    let labels = 0
    const label = (group, value) => {
        assert.ok(m.ruleLabels[group]?.[value], `missing label ${group}.${value}`);
        for (const locale of ['en', 'zh-CN']) assert.ok(m.ruleLabel(group, value, locale).trim());
        labels++
    }
    for (const field of [...backend.rule_fields, ...backend.context_fields]) label('field', field.name)
    checkCapabilityLabels()

    function checkCapabilityLabels() {
        for (const [section, group] of [['conditions', 'condition'], ['actions', 'action'], ['value_expressions', 'expression']]) for (const cap of backend[section]) {
            label(group, cap.id)
            for (const field of cap.fields) {
                label('field', field.name);
                for (const value of field.enum ?? []) {
                    const found = ['option', 'phase', 'source', 'condition'].find(group => m.ruleLabels[group]?.[value]);
                    assert.ok(found, `missing enum label ${cap.id}.${field.name}:${value}`);
                    label(found, value)
                }
            }
        }
    }
    for (const field of backend.rule_fields) for (const value of field.enum ?? []) {
        const group = ['option', 'phase', 'source'].find(group => m.ruleLabels[group]?.[value]);
        assert.ok(group);
        label(group, value)
    }
    for (const phase of backend.phases) label('phase', phase)
    for (const source of schema.sources) label('source', source)
    for (const status of Object.keys(m.ruleLabels.status)) label('status', status)
    return labels
}

function checkNestedWorkflow(m, schema) {
    const set = id => ({id, type: 'json_set', params: {path: `/${id}`, value: false}})
    const branch = {
        id: 'branch', type: 'if', params: {
            predicate: {op: 'always'},
            then: [{id: 'each', type: 'for_each', params: {path: '/input', steps: [set('shared'), set('second')]}}],
            else: [set('shared')],
        },
    }
    const rule = {...m.newRule(), actions: [set('before'), branch, set('after')]}
    const graph = m.ruleToGraph(rule), owner = graph.nodes.find(node => node.data.id === 'branch')
    assert.deepEqual(m.graphToRule(graph, schema), rule)
    assert.deepEqual(m.graphNodeValue(graph, owner.id), branch)
    assert.equal(new Set(graph.nodes.map(node => m.graphLayoutNodeKey(node, graph))).size, graph.nodes.length, 'same action ID in separate branches has distinct layout identity')
    const edited = m.replaceGraphNode(graph, owner.id, {
        ...m.graphNodeValue(graph, owner.id),
        params: {...branch.params, predicate: {op: 'exists', path: '/input'}}
    })
    assert.deepEqual(edited.nodes.map(node => node.id), graph.nodes.map(node => node.id), 'editing a parent retains every branch node identity')
    assert.deepEqual(m.graphToRule(edited, schema).actions[1].params.then, branch.params.then)
    const inserted = m.addGraphModule(graph, {
        kind: 'action',
        type: 'json_set',
        targetId: owner.id,
        sourceHandle: 'flow:else'
    }, schema)
    const insertion = inserted.nodes.find(node => node.id === inserted.selected[0])
    assert.deepEqual(m.graphToRule(inserted, schema).actions[1].params.else.map(action => action.id), [insertion.data.id, 'shared'])
    const thenTail = graph.nodes.find(node => node.data.id === 'each')
    const appended = m.addGraphModule(graph, {kind: 'action', type: 'json_set', targetId: thenTail.id}, schema)
    assert.equal(m.graphToRule(appended, schema).actions[1].params.then.length, 2)
    assert.deepEqual(m.graphToRule(m.removeGraphNodes(graph, [owner.id]), schema).actions, [rule.actions[0], rule.actions[2]], 'deleting parent removes all bodies and closes the chain')
    const second = graph.nodes.find(node => node.data.id === 'second')
    const each = graph.nodes.find(node => node.data.id === 'each')
    const copied = m.duplicateGraphNodes(graph, [owner.id, each.id])
    assert.equal(copied.nodes.length - graph.nodes.length, m.graphNodeClosure(graph, [owner.id]).size, 'copy includes every child even when a child is also selected')
    assert.ok(copied.selected.every(id => !graph.nodes.some(node => node.id === id)))
    assert.equal(new Set(copied.nodes.filter(node => copied.selected.includes(node.id) && node.kind === 'action').map(node => node.data.id)).size, 5)
    const copiedOwner = copied.nodes.find(node => copied.selected.includes(node.id) && node.data.type === 'if')
    const after = graph.nodes.find(node => node.data.id === 'after')
    const connected = m.connectGraph(copied, {
        source: after.id,
        target: copiedOwner.id,
        sourceHandle: 'action-out',
        targetHandle: 'action-in'
    }, schema)
    const copiedRule = m.graphToRule(connected, schema)
    assert.equal(copiedRule.actions.length, 4)
    assert.deepEqual(copiedRule.actions[3].params.then[0].params.steps.map(action => action.params), branch.params.then[0].params.steps.map(action => action.params))
    const withoutHead = m.removeGraphNodes(graph, [graph.nodes.find(node => node.data.id === 'shared' && node.path.includes('/then/')).id])
    assert.deepEqual(m.graphToRule(withoutHead, schema).actions[1].params.then[0].params.steps, [set('second')])
    assert.equal(m.graphPathMap(withoutHead)[second.id], '/actions/1/params/then/0/params/steps/0')
    for (const type of ['walk', 'scope']) {
        const action = type === 'walk' ? {id: type, type, params: {path: '/input', steps: [branch]}} : {
            id: type,
            type,
            params: {functions: {'a/b~c': [set('function')]}, steps: [branch]}
        }
        const nested = {...m.newRule(), actions: [action]}, built = m.ruleToGraph(nested)
        assert.deepEqual(m.graphToRule(built, schema), nested)
        assert.deepEqual(m.graphNodeValue(built, built.nodes.find(node => node.data.id === type).id), action)
        assert.deepEqual(m.graphToRule(m.replaceGraphNode(built, built.nodes.find(node => node.data.id === type).id, action), schema), nested)
    }
    // Empty and nested legacy containers are transparent, including continuation after their last child.
    const sequence = (id, steps) => ({id, type: 'sequence', params: {steps}})
    const old = {
        ...m.newRule(),
        actions: [set('a'), sequence('outer', [sequence('empty', []), set('b'), sequence('inner', [set('c'), set('d')]), set('e')]), set('f')]
    }
    const built = m.ruleToGraph(old), visible = m.visibleWorkflow(built)
    assert.ok(visible.nodes.every(node => node.data.type !== 'sequence'))
    let current = built.nodes.find(node => node.kind === 'rule').id
    const order = []
    for (; ;) {
        const edge = visible.edges.find(edge => edge.source === current && edge.sourceHandle === 'action-out')
        if (!edge) break
        current = edge.target
        order.push(visible.nodes.find(node => node.id === current).data.id)
        assert.ok(order.length < 10, 'transparent sequence must not loop')
    }
    assert.deepEqual(order, ['a', 'b', 'c', 'd', 'e', 'f'])
    assert.deepEqual(m.graphToRule(built, schema), old, 'transparent presentation preserves legacy AST')
    console.log('PASS: branch/body/function roundtrips; parent editing; flow insertion; copy/delete closure; transparent legacy sequence order')
    const expression = {
        $expr: {
            op: 'concat',
            args: ['prefix-', {$expr: {op: 'string', args: [{$ref: {source: 'vars', path: '/message/index'}}]}}]
        }
    }
    const computedRule = {
        ...m.newRule(),
        actions: [{
            id: 'each',
            type: 'for_each',
            params: {
                path: '/input',
                bind: 'message',
                steps: [{id: 'set', type: 'json_set', params: {path: '/label', value: expression}}]
            }
        }]
    }
    const computedGraph = m.ruleToGraph(computedRule)
    const calculation = computedGraph.nodes.find(node => node.kind === 'value' && node.data.mode === 'computed' && node.data.value.$expr.op === 'concat')
    assert.deepEqual(m.graphToRule(computedGraph, schema), computedRule, 'expression graph preserves nested reference and argument order')
    const valuePaths = m.graphPathMap(computedGraph)
    assert.equal(valuePaths[calculation.id], '/actions/0/params/steps/0/params/value')
    const first = computedGraph.nodes.find(node => node.kind === 'value' && node.data.mode === 'literal' && node.data.value === 'prefix-')
    const changed = m.replaceGraphNode(computedGraph, first.id, {mode: 'literal', value: 'edited-'})
    assert.equal(m.graphToRule(changed, schema).actions[0].params.steps[0].params.value.$expr.args[0], 'edited-')
    assert.deepEqual(changed.nodes.map(node => node.id), computedGraph.nodes.map(node => node.id), 'operand editing retains graph identities')
    const reordered = m.replaceGraphNode(computedGraph, calculation.id, {
        mode: 'computed',
        value: {$expr: {op: 'concat', args: [...expression.$expr.args].reverse()}}
    })
    assert.deepEqual(m.graphToRule(reordered, schema).actions[0].params.steps[0].params.value.$expr.args, [...expression.$expr.args].reverse())
    const dataEdge = computedGraph.edges.find(edge => edge.source === first.id)
    const disconnected = structuredClone(computedGraph);
    disconnected.edges = disconnected.edges.filter(edge => edge.id !== dataEdge.id)
    assert.ok(m.graphErrors(disconnected, schema).some(error => error.code === 'graph.missingValue'), 'disconnected inputs cannot silently reuse stale operands')
    assert.throws(() => m.connectGraph(computedGraph, {
        source: calculation.id,
        target: calculation.id,
        sourceHandle: 'value-out',
        targetHandle: 'value:/value/$expr/args/0'
    }, schema))
    const computedCopy = m.duplicateGraphNodes(computedGraph, [computedGraph.nodes.find(node => node.data.id === 'set').id])
    assert.equal(computedCopy.selected.length, 5, 'copy action includes complete expression dependency tree')
    const escaped = {
        ...m.newRule(),
        actions: [{
            id: 'literal',
            type: 'json_set',
            params: {path: '/literal', value: {$literal: {$expr: {op: 'unknown', args: [false, 0, null]}}}}
        }]
    }
    assert.deepEqual(m.graphToRule(m.ruleToGraph(escaped), schema), escaped, 'escaped objects remain literal and never become expression nodes')
    console.log('PASS: computed workflow inputs, nested references, operand edits/order, missing-source and cycle rejection, dependency copy, escaped literal preservation')
}

function checkQuickAdd(m, schema) {
    const freeze = value => {
        if (value && typeof value === 'object') {
            Object.values(value).forEach(freeze);
            Object.freeze(value)
        }
        return value
    }
    const add = (graph, options, capabilities = schema) => {
        const before = structuredClone(graph), beforeOptions = structuredClone(options)
        freeze(graph);
        freeze(options)
        try {
            const next = m.addGraphModule(graph, options, capabilities)
            assert.notEqual(next, graph)
            assert.equal(next.selected.length, 1)
            assert.ok(next.nodes.some(node => node.id === next.selected[0]))
            assert.deepEqual(m.graphStructureErrors(next, capabilities), [])
            assert.equal(next.nodes.filter(node => node.kind === 'rule').length, 1, 'fragment must not retain a temporary rule root')
            return next
        } finally {
            assert.deepEqual(graph, before, 'quick add mutated its input graph')
            assert.deepEqual(options, beforeOptions, 'quick add mutated its options')
        }
    }
    const rejected = (graph, options, code) => assert.throws(() => add(graph, options), error => error instanceof m.RuleInputError && (!code || error.errors.some(error => error.code === code)))
    const make = (when, actions = [], extra = {}) => m.ruleToGraph({
        ...m.newRule(),
        name: 'quick add',
        when: when ?? {op: 'always'},
        actions, ...extra
    })
    const rootOf = graph => graph.nodes.find(node => node.kind === 'rule')
    const selected = graph => graph.nodes.find(node => node.id === graph.selected[0])
    const rootCondition = graph => graph.nodes.find(node => node.id === graph.edges.find(edge => edge.target === rootOf(graph).id && edge.targetHandle === 'boolean-in').source)
    const ruleOf = graph => m.graphToRule(graph, schema)
    const eq = m.defaultCondition('eq', schema)
    const oldActions = [{
        id: 'first-stable-id',
        type: 'json_set',
        params: {path: '/before', value: {nested: [null, false, 0]}}
    }, {
        id: 'second-stable-id',
        type: 'array_filter',
        params: {
            path: '/items',
            predicate: {op: 'not', conditions: [{op: 'eq', source: 'item', path: '/keep', value: false}]}
        }
    }]
    const chain = make(undefined, oldActions), oldNodes = chain.nodes.filter(node => node.kind === 'action')
    const oldLink = chain.edges.find(edge => edge.source === oldNodes[0].id && edge.targetHandle === 'action-in')
    const inserted = add(chain, {
        kind: 'action',
        type: 'json_remove',
        targetId: oldNodes[0].id,
        position: {x: 1777, y: -845}
    })
    const insertion = selected(inserted), insertedRule = ruleOf(inserted)
    assert.deepEqual(insertedRule.actions.map(action => action.id), [oldActions[0].id, insertion.data.id, oldActions[1].id])
    assert.deepEqual([insertedRule.actions[0], insertedRule.actions[2]], oldActions, 'insertion retains both old action IDs, parameters and relative order')
    assert.deepEqual(insertion.position, {x: 1777, y: -845})
    assert.match(insertion.data.id, /^(?:[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}|editor-)/i, 'new action uses editorId')
    for (const old of chain.nodes) assert.deepEqual(inserted.nodes.find(node => node.id === old.id).data, old.data, 'existing graph-node identity/data changed')
    assert.deepEqual(inserted.edges.find(edge => edge.id === oldLink.id), {
        ...oldLink,
        source: insertion.id
    }, 'old successor edge is reconnected, not discarded')
    assert.deepEqual(rootOf(inserted), rootOf(chain), 'temporary fragments never change the existing rule node')
    const appended = add(chain, {kind: 'action', type: 'json_set'})
    assert.deepEqual(ruleOf(appended).actions.slice(0, 2), oldActions, 'default adds to the unique chain tail')
    assert.equal(appended.edges.find(edge => edge.target === selected(appended).id && edge.targetHandle === 'action-in').source, oldNodes[1].id)
    assert.equal(selected(appended).position.x, oldNodes[1].position.x + m.ruleNodeColumnGap)
    const prepended = add(chain, {kind: 'action', type: 'json_set', targetId: rootOf(chain).id})
    assert.deepEqual(ruleOf(prepended).actions.slice(1), oldActions, 'rule target inserts before the first action')
    const empty = make(), firstAction = add(empty, {kind: 'action', type: 'json_set'})
    assert.equal(ruleOf(firstAction).actions.length, 1)
    assert.equal(selected(firstAction).position.x, rootOf(empty).position.x + m.ruleNodeColumnGap)

    for (const op of ['all', 'any']) {
        const group = make({op, conditions: [{op: 'eq', value: 0}, {op: 'eq', value: false}]}),
            parent = rootCondition(group)
        group.edges.filter(edge => edge.target === parent.id).forEach((edge, index) => edge.order = [3, 11][index])
        const next = add(group, {kind: 'condition', type: 'exists', targetId: parent.id})
        assert.deepEqual(ruleOf(next).when, {
            op,
            conditions: [{op: 'eq', value: 0}, {op: 'eq', value: false}, m.defaultCondition('exists', schema)]
        })
        assert.equal(next.edges.find(edge => edge.source === selected(next).id).order, 12, 'append uses max order rather than child count')
        assert.equal(next.nodes.length, group.nodes.length + 1)
    }
    const replacement = add(empty, {kind: 'condition', type: 'eq', position: {x: -720, y: 555}})
    assert.equal(selected(replacement).id, rootCondition(empty).id, 'root always replacement retains its graph ID')
    assert.deepEqual(selected(replacement).position, {x: -720, y: 555})
    assert.deepEqual(ruleOf(replacement).when, eq)
    const defaultReplacement = add(empty, {kind: 'condition', type: 'not'})
    assert.deepEqual(selected(defaultReplacement).position, rootCondition(empty).position)
    assert.deepEqual(ruleOf(defaultReplacement).when, m.defaultCondition('not', schema))
    const subtree = {
        op: 'any',
        conditions: [{op: 'eq', path: '/first', value: false}, {
            op: 'not',
            conditions: [{op: 'eq', path: '/second', value: true}]
        }]
    }
    const nonAlways = make(subtree), combined = add(nonAlways, {kind: 'condition', type: 'eq'})
    assert.deepEqual(ruleOf(combined).when, {
        op: 'all',
        conditions: [subtree, eq]
    }, 'non-always root remains the first intact subtree')
    assert.ok(combined.nodes.some(node => node.id === rootCondition(nonAlways).id))
    assert.equal(selected(combined).data.op, 'eq', 'selection is the requested condition, not the implicit all')
    const crowded = make(subtree, oldActions)
    checkCrowdedPlacement()

    function checkCrowdedPlacement() {
        for (const options of [{kind: 'condition', type: 'not'}, {
            kind: 'condition',
            type: 'any',
            targetId: rootCondition(crowded).id
        }, {kind: 'action', type: 'array_filter', targetId: rootOf(crowded).id}]) {
            const placed = add(crowded, options)
            for (const node of placed.nodes.filter(node => !crowded.nodes.some(old => old.id === node.id))) for (const other of placed.nodes.filter(other => other.id !== node.id)) assert.ok(Math.abs(node.position.x - other.position.x) >= 360 || Math.abs(node.position.y - other.position.y) >= 240, 'auto-added main nodes and predicate/group descendants avoid existing cards')
            for (const old of crowded.nodes) assert.deepEqual(placed.nodes.find(node => node.id === old.id).position, old.position, 'quick-add never moves existing cards')
        }
    }
    const nestedLeaf = nonAlways.nodes.find(node => node.data.path === '/first')
    const nestedEdge = nonAlways.edges.find(edge => edge.source === nestedLeaf.id)
    const besideLeaf = add(nonAlways, {kind: 'condition', type: 'exists', targetId: nestedLeaf.id})
    const nestedParent = besideLeaf.edges.find(edge => edge.id === nestedEdge.id)
    assert.equal(nestedParent.target, nestedEdge.target)
    assert.equal(nestedParent.targetHandle, nestedEdge.targetHandle)
    assert.equal(nestedParent.order, nestedEdge.order)
    assert.deepEqual(ruleOf(besideLeaf).when.conditions[0], {
        op: 'all',
        conditions: [subtree.conditions[0], m.defaultCondition('exists', schema)]
    })

    const originalNot = {
        op: 'not',
        conditions: [{
            op: 'all',
            conditions: [{op: 'eq', path: '/short-circuit-first', value: false}, {
                op: 'eq',
                path: '/never',
                value: true
            }]
        }]
    }
    const negated = make(originalNot)
    const besideNot = add(negated, {kind: 'condition', type: 'eq', targetId: rootCondition(negated).id})
    assert.deepEqual(ruleOf(besideNot).when, {
        op: 'all',
        conditions: [originalNot, eq]
    }, 'new condition is outside the occupied NOT')
    const trace = []
    const evaluate = condition => {
        if (condition.op === 'all') return condition.conditions.every(evaluate)
        if (condition.op === 'any') return condition.conditions.some(evaluate)
        if (condition.op === 'not') return !evaluate(condition.conditions[0])
        trace.push(condition.path ?? '')
        return !!condition.value
    }
    assert.equal(evaluate(ruleOf(besideNot).when), false)
    assert.deepEqual(trace, ['/short-circuit-first', eq.path ?? ''], 'NOT subtree short circuits before evaluating the added sibling')
    for (const op of ['all', 'any']) {
        trace.length = 0
        const before = make({op, conditions: [{op: 'eq', path: '/old-first', value: op === 'any'}]})
        const after = add(before, {kind: 'condition', type: 'eq', targetId: rootCondition(before).id})
        evaluate(ruleOf(after).when)
        assert.deepEqual(trace, ['/old-first'], `${op} keeps existing short-circuit order`)
    }
    const emptyNot = make({op: 'not', conditions: []})
    assert.deepEqual(ruleOf(add(emptyNot, {
        kind: 'condition',
        type: 'eq',
        targetId: rootCondition(emptyNot).id
    })).when, {op: 'not', conditions: [eq]})
    checkConditionWrapping()

    function checkConditionWrapping() {
        for (const op of ['all', 'any', 'not']) {
            const before = make(subtree), target = rootCondition(before),
                edge = before.edges.find(edge => edge.source === target.id)
            edge.order = 17
            const wrapped = add(before, {kind: 'condition', type: op, mode: 'wrap', position: {x: -111, y: 888}})
            assert.deepEqual(ruleOf(wrapped).when, {op, conditions: [subtree]})
            assert.equal(wrapped.nodes.length, before.nodes.length + 1, 'wrap must not retain a default always sibling')
            assert.deepEqual(selected(wrapped).position, {x: -111, y: 888})
            assert.deepEqual(wrapped.edges.find(next => next.id === edge.id), {...edge, source: selected(wrapped).id})
            for (const node of before.nodes) assert.deepEqual(wrapped.nodes.find(next => next.id === node.id).data, node.data, 'wrap preserves every subtree node')
        }
    }
    const wrappedLeaf = add(nonAlways, {kind: 'condition', type: 'not', mode: 'wrap', targetId: nestedLeaf.id})
    assert.deepEqual(ruleOf(wrappedLeaf).when.conditions[0], {op: 'not', conditions: [subtree.conditions[0]]})
    const wrapRule = add(nonAlways, {kind: 'condition', type: 'not', mode: 'wrap', targetId: rootOf(nonAlways).id})
    assert.deepEqual(ruleOf(wrapRule).when, {op: 'not', conditions: [subtree]})

    const filterGraph = add(empty, {kind: 'action', type: 'array_filter', position: {x: 2000, y: 500}}),
        filterNode = selected(filterGraph)
    assert.equal(filterGraph.nodes.length, empty.nodes.length + 2, 'array_filter fragment includes only action and predicate')
    const predicateEdge = filterGraph.edges.find(edge => edge.target === filterNode.id && edge.targetHandle === 'predicate:predicate')
    assert.ok(predicateEdge)
    assert.deepEqual(m.graphNodeValue(filterGraph, filterNode.id), ruleOf(filterGraph).actions[0])
    const predicatePosition = filterGraph.nodes.find(node => node.id === predicateEdge.source).position
    assert.equal(predicatePosition.x, filterNode.position.x - m.ruleNodeColumnGap)
    assert.ok(predicatePosition.y > filterNode.position.y, 'predicate starts below its owning action')
    const changedPredicate = add(filterGraph, {
        kind: 'condition',
        type: 'eq',
        targetId: filterNode.id,
        position: {x: 500, y: 600}
    })
    assert.equal(selected(changedPredicate).id, predicateEdge.source)
    assert.deepEqual(selected(changedPredicate).position, {x: 500, y: 600})
    assert.deepEqual(ruleOf(changedPredicate).actions[0].params.predicate, eq)
    assert.equal(selected(changedPredicate).data.source, eq.source, 'predicate addition never guesses item source')
    assert.deepEqual(ruleOf(changedPredicate).when, {op: 'always'})
    const mixedSources = add(chain, {kind: 'condition', type: 'exists', targetId: oldNodes[1].id})
    assert.deepEqual(ruleOf(mixedSources).actions[1].params.predicate, {
        op: 'all',
        conditions: [oldActions[1].params.predicate, m.defaultCondition('exists', schema)]
    })
    const wrappedPredicate = add(chain, {
        kind: 'condition',
        type: 'not',
        mode: 'wrap',
        targetId: chain.edges.find(edge => edge.target === oldNodes[1].id && edge.targetHandle === 'predicate:predicate').source
    })
    assert.deepEqual(ruleOf(wrappedPredicate).actions[1].params.predicate, {
        op: 'not',
        conditions: [oldActions[1].params.predicate]
    })
    const sourceGraph = make(undefined, [{
        id: 'source-filter',
        type: 'array_filter',
        params: {path: '/items', predicate: {op: 'all', conditions: [{op: 'eq', source: 'item', value: true}]}}
    }])
    const predicateGroup = sourceGraph.nodes.find(node => node.kind === 'condition' && node.data.op === 'all')
    const sourceAddition = add(sourceGraph, {kind: 'condition', type: 'eq', targetId: predicateGroup.id})
    assert.deepEqual(ruleOf(sourceAddition).actions[0].params.predicate.conditions, [{
        op: 'eq',
        source: 'item',
        value: true
    }, eq])

    checkDetachedModules()

    function checkDetachedModules() {
        for (const [kind, type, count] of [['condition', 'eq', 1], ['condition', 'not', 2], ['action', 'array_filter', 2]]) {
            const detached = add(empty, {kind, type, mode: 'detached', targetId: rootOf(empty).id})
            assert.equal(detached.nodes.length, empty.nodes.length + count)
            assert.deepEqual(detached.edges.filter(edge => empty.edges.some(old => old.id === edge.id)), empty.edges)
            assert.ok(m.graphErrors(detached, schema).some(error => error.code === 'graph.orphan'))
            assert.throws(() => ruleOf(detached), m.RuleInputError, 'orphan modules block serialization/save')
            rejected(detached, {kind, type, targetId: selected(detached).id}, 'graph.orphan')
            if (kind === 'action') rejected(detached, {kind: 'action', type: 'json_set'}, 'graph.orphan')
            else rejected(detached, {
                kind: 'condition',
                type: 'not',
                mode: 'wrap',
                targetId: selected(detached).id
            }, 'graph.orphan')
        }
    }
    rejected(empty, {kind: 'action', type: 'drop_event'}, 'graph.phase')
    rejected(make(undefined, [], {phase: 'response_body'}), {kind: 'action', type: 'reject_request'}, 'graph.phase')
    rejected(empty, {kind: 'action', type: 'drop_event', mode: 'detached'}, 'graph.phase')
    for (const kind of ['action', 'condition']) {
        rejected(empty, {kind, type: 'unknown'}, 'graph.capability')
        rejected(empty, {kind, type: kind === 'action' ? 'json_set' : 'eq', targetId: 'missing'}, 'graph.target')
    }
    rejected(empty, {kind: 'action', type: 'json_set', targetId: rootCondition(empty).id}, 'graph.target')
    rejected(chain, {kind: 'condition', type: 'eq', targetId: oldNodes[0].id}, 'graph.target')
    rejected(chain, {kind: 'condition', type: 'not', mode: 'wrap', targetId: oldNodes[1].id}, 'graph.target')
    rejected(empty, {kind: 'action', type: 'json_set', mode: 'wrap'}, 'graph.mode')
    rejected(empty, {kind: 'condition', type: 'eq', mode: 'wrap'}, 'graph.mode')
    rejected(empty, {kind: 'condition', type: 'eq', position: {x: Infinity, y: 0}}, 'graph.position')
    const noCondition = structuredClone(empty);
    noCondition.nodes = noCondition.nodes.filter(node => node.kind === 'rule');
    noCondition.edges = []
    rejected(noCondition, {kind: 'condition', type: 'not', mode: 'wrap'}, 'graph.missingCondition')
    assert.deepEqual(ruleOf(add(noCondition, {kind: 'condition', type: 'eq'})).when, eq)
    const branch = structuredClone(chain);
    branch.edges.push(m.graphEdge(rootOf(branch).id, oldNodes[1].id, 'action-in'))
    rejected(branch, {kind: 'action', type: 'json_set', targetId: rootOf(branch).id}, 'graph.successor')
    const broken = structuredClone(chain);
    broken.edges.push(m.graphEdge('missing-node', oldNodes[1].id, 'predicate:predicate'))
    rejected(broken, {kind: 'action', type: 'json_set'}, 'graph.endpoint')
    const cyclic = structuredClone(chain);
    cyclic.edges.push(m.graphEdge(oldNodes[1].id, oldNodes[0].id, 'action-in'))
    rejected(cyclic, {kind: 'action', type: 'json_set'}, 'graph.cycle')
    const unfinished = add(empty, {kind: 'action', type: 'text_replace'})
    selected(unfinished).data.params.pattern = ''
    assert.ok(m.graphErrors(unfinished, schema).some(error => error.path === '/actions/0/params/pattern'), 'empty required default remains editable instead of blocking creation')
    const unfinishedAgain = add(unfinished, {kind: 'action', type: 'json_set'})
    assert.equal(m.graphToRule(unfinishedAgain, schema, false).actions.length, 2)
    const customSchema = structuredClone(schema)
    customSchema.conditions.find(cap => cap.id === 'eq').fields.find(field => field.name === 'value').default = 'custom-default'
    assert.equal(selected(add(empty, {kind: 'condition', type: 'eq'}, customSchema)).data.value, 'custom-default')

    checkPersistedLayouts()

    function checkPersistedLayouts() {
        for (const persisted of [false, true]) {
            const before = make(undefined, [], persisted ? {id: 'saved-rule', revision: 4} : {}),
                draft = m.createDraft(rootOf(before).data, !persisted)
            const after = add(before, {kind: 'action', type: 'json_set'})
            after.nodes.forEach((node, index) => node.position = {x: 4444 + index * 360, y: -9999 + index * 240})
            after.viewport = {x: 12345, y: 54321, zoom: .7};
            after.layoutRestored = true
            draft.graph = after;
            draft.rule = ruleOf(after)
            const body = JSON.parse(m.stringifyRule(draft.rule))
            assert.equal(m.draftDirty(draft), true)
            assert.deepEqual(Object.keys(body).sort((a, b) => a.localeCompare(b)), Object.keys(rootOf(before).data).sort((a, b) => a.localeCompare(b)))
            for (const key of ['nodes', 'edges', 'position', 'viewport', 'selected', 'layoutRestored', 'graph', 'graphUndo', 'graphRedo']) assert.equal(Object.hasOwn(body, key), false, `${persisted ? 'update' : 'create'} body leaked ${key}`)
            assert.deepEqual(Object.keys(body.actions[0]).sort((a, b) => a.localeCompare(b)), ['id', 'params', 'type'])
            assert.ok(!JSON.stringify(body).includes('pi-rule-layout:'))
            assert.equal(body.id, persisted ? 'saved-rule' : undefined)
            assert.equal(body.revision, persisted ? 4 : undefined)
            const again = m.ruleToGraph(body, after)
            assert.equal(again.nodes.find(node => node.kind === 'action').id, selected(after).id, 'quick-added action retains editor identity on rebuild')
        }
    }
    console.log('PASS: immutable graph quick-add; middle/tail actions and stable IDs; ordered conditions/NOT/wrap/predicates; detached orphans; invalid targets/phases; UI-free create/update AST')
}
