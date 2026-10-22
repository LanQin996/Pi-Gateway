import {beforeRequest, expect, simulationResponse, test} from './admin.fixture'
import {
    addModule,
    closeParameters,
    configureModelReplacement,
    focusNode,
    inlineNodeControls,
    readCode,
    ruleEditor,
    ruleName,
    selectCapture,
    showCanvas
} from './rules-ui.helpers'

test('HTTP 僅使用 getRandomValues 仍可新增、追加和複製動作', async ({page, admin}) => {
    await page.addInitScript(() => {
        Object.defineProperty(Crypto.prototype, 'randomUUID', {value: undefined, configurable: true})
    })
    await admin.open()
    expect(new URL(page.url()).protocol).toBe('http:')
    expect(await page.evaluate(() => typeof window.crypto?.randomUUID)).toBe('undefined')
    expect(await page.evaluate(() => typeof window.crypto?.getRandomValues)).toBe('function')
    await page.getByRole('button', {name: '新增规则', exact: true}).click()
    await ruleName(page).fill('HTTP getRandomValues')
    await showCanvas(page)
    await addModule(page, 'action', 'text_replace')
    await focusNode(page)
    await configureModelReplacement(inlineNodeControls(page), 'fixture-model-a')
    await expect(page.locator('.rule-graph-action')).toHaveCount(1)
    await closeParameters(page)
    await page.locator('.rule-canvas-toolbar').getByRole('button', {name: '复制', exact: true}).click()
    await expect(page.locator('.rule-graph-action')).toHaveCount(2)
    const nodeIDs = await page.locator('.rule-graph-node').evaluateAll(nodes => nodes.map(node => node.getAttribute('data-node-id')))
    expect(new Set(nodeIDs).size).toBe(nodeIDs.length)
    // A copied, unconnected node must be retained and block serialization.
    await ruleEditor(page).getByRole('button', {name: '保存规则', exact: true}).click()
    await expect(ruleEditor(page).getByRole('alert').filter({hasText: '草稿包含未连接节点'})).toBeVisible()
    expect(admin.requests.some(request => request.method === 'POST' && request.path === '/api/rules')).toBe(false)
    await page.locator('.rule-canvas-toolbar').getByRole('button', {name: '撤销', exact: true}).click()
    await addModule(page, 'action', 'text_replace')
    await focusNode(page)
    await configureModelReplacement(inlineNodeControls(page), 'fixture-model-b')
    const rule = await readCode(page)
    expect(rule.actions).toHaveLength(2)
    expect(new Set(rule.actions.map(action => action.id)).size).toBe(2)
    await ruleEditor(page).getByRole('button', {name: '保存规则', exact: true}).click()
    await expect.poll(() => admin.rules.find(saved => saved.name === rule.name)?.id).toBeTruthy()
    const saved = admin.rules.find(candidate => candidate.name === rule.name)!
    expect(saved.actions).toEqual(rule.actions)
    await page.reload()
    await admin.editRule(rule.name)
    expect(await readCode(page)).toEqual(saved)
})

for (const mode of ['absent', 'missing', 'throwing'] as const) {
    test(`安全亂數 ${mode} 時載入不崩潰、請求 ID 明確失敗`, async ({page}) => {
        await page.addInitScript(unavailable => {
            if (unavailable === 'absent') {
                Object.defineProperty(window, 'crypto', {value: undefined, configurable: true})
            } else {
                const getRandomValues = unavailable === 'missing' ? undefined : () => { throw new Error('entropy blocked') }
                Object.defineProperty(Crypto.prototype, 'getRandomValues', {value: getRandomValues, configurable: true})
            }
        }, mode)
        await page.goto('/login')
        const result = await page.evaluate(async sourcePath => {
            const module = await import(sourcePath)
            try {
                return {loaded: true, id: module.editorId()}
            } catch (error) {
                return {loaded: true, error: error instanceof Error ? error.message : String(error)}
            }
        }, '/src/utils/editorId.ts')
        expect(result.loaded).toBe(true)
        expect(result.error).toContain('Secure random values are unavailable')
        expect(result).not.toHaveProperty('id')
    })
}

test('画布组合两个条件和两个串行动作，保存后无损往返 JSON', async ({page, admin}, testInfo) => {
    admin.simulate = request => {
        const response = simulationResponse(request)
        const result = response.results[0]!.result
        result.body = {...structuredClone(beforeRequest), model: 'fixture-final-model'}
        result.model = 'fixture-final-model'
        result.traces.forEach((trace, index) => {
            trace.changes = [{
                path: '/model', operation: 'replace', before_exists: true, after_exists: true,
                before: index ? 'fixture-first-model' : 'fixture-old-model',
                after: index ? 'fixture-final-model' : 'fixture-first-model'
            }]
        })
        result.condition_traces = ['/when', '/when/conditions/0', '/when/conditions/1'].map(path => ({
            rule_id: response.draft_rule_id,
            path,
            status: 'matched',
            matched: true
        }))
        return {body: response}
    }
    await admin.open()
    await page.getByRole('button', {name: '新增规则', exact: true}).click()
    await ruleName(page).fill('Two conditions and actions')
    await showCanvas(page)
    await addModule(page, 'condition', 'exists')
    await focusNode(page)
    await inlineNodeControls(page).getByLabel('字段路径', {exact: true}).fill('/model')
    await addModule(page, 'condition', 'exists')
    await focusNode(page)
    await inlineNodeControls(page).getByLabel('字段路径', {exact: true}).fill('/input')
    await addModule(page, 'action', 'text_replace')
    await focusNode(page)
    await configureModelReplacement(inlineNodeControls(page), 'fixture-first-model')
    await addModule(page, 'action', 'text_replace')
    await focusNode(page)
    await configureModelReplacement(inlineNodeControls(page), 'fixture-final-model', 'fixture-first-model')
    await expect(page.locator('.rule-graph-condition')).toHaveCount(3)
    await expect(page.locator('.rule-graph-action')).toHaveCount(2)
    await expect(page.locator('.vue-flow__edge')).toHaveCount(5)
    const before = await readCode(page)
    expect(before.when.op).toBe('all')
    expect(before.when.conditions?.map(condition => condition.path)).toEqual(['/model', '/input'])
    expect(before.actions.map(action => action.params.replacement)).toEqual(['fixture-first-model', 'fixture-final-model'])
    expect(before).not.toHaveProperty('nodes')
    expect(before).not.toHaveProperty('viewport')
    await showCanvas(page)
    expect(await readCode(page)).toEqual(before)
    await ruleEditor(page).getByRole('button', {name: '保存规则', exact: true}).click()
    await expect.poll(() => admin.rules.find(rule => rule.name === before.name)?.id).toBeTruthy()
    const saved = admin.rules.find(rule => rule.name === before.name)!
    expect(saved.when).toEqual(before.when)
    expect(saved.actions).toEqual(before.actions)
    await page.reload()
    await admin.editRule(before.name)
    const reopened = await readCode(page)
    expect(reopened).toEqual(saved)
    await showCanvas(page)
    await expect(page.locator('.rule-graph-condition')).toHaveCount(3)
    await expect(page.locator('.rule-graph-action')).toHaveCount(2)
    await selectCapture(page, 101)
    await page.getByTestId('simulate-capture').click()
    await expect(page.getByTestId('simulation-diff')).toContainText('fixture-final-model')
    await expect(page.getByTestId('simulation-stale')).toHaveCount(0)
    await page.setViewportSize({width: 1600, height: 1900})
    // Sample-path pickers make condition cards taller; the real layout action uses their measured dimensions.
    await page.locator('.rule-canvas-toolbar').getByRole('button', {name: '自动整理', exact: true}).click()
    await page.getByTestId('canvas-fit').click()
    await expect(page.getByTestId('simulation-stale')).toHaveCount(0)
    const leafBounds = await page.locator('.rule-graph-condition').filter({has: page.locator('.rule-graph-node-title', {hasText: '字段存在'})}).evaluateAll(nodes => nodes.map(node => {
        const rect = node.getBoundingClientRect()
        return {left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom}
    }))
    expect(leafBounds).toHaveLength(2)
    const [firstLeaf, secondLeaf] = leafBounds
    expect(firstLeaf!.right <= secondLeaf!.left || secondLeaf!.right <= firstLeaf!.left || firstLeaf!.bottom <= secondLeaf!.top || secondLeaf!.bottom <= firstLeaf!.top).toBe(true)
    await page.locator('.app-main').evaluate(main => main.scrollTo({top: 0}))
    const screenshot = testInfo.outputPath('rules-canvas-capture-success.png')
    await page.screenshot({path: screenshot, fullPage: true})
    await testInfo.attach('组合条件动作与传输记录模拟成功', {path: screenshot, contentType: 'image/png'})
})

test('已有规则的 false、零、null、身份和有序动作在代码往返中保留', async ({page, admin}) => {
    const original = structuredClone(admin.rules[0]!)
    original.actions[0]!.params.value = {
        flag: false,
        count: 0,
        nullable: null,
        empty: '',
        list: [],
        escaped: {$literal: {$ref: 'not a reference'}}
    }
    admin.rules[0] = original
    await admin.open()
    await admin.editRule()
    expect(await readCode(page)).toEqual(original)
    await showCanvas(page)
    expect(await readCode(page)).toEqual(original)
    await ruleEditor(page).getByRole('button', {name: '保存规则', exact: true}).click()
    await expect.poll(() => admin.requests.filter(request => request.method === 'PUT').length).toBe(1)
    const saved = admin.requests.find(request => request.method === 'PUT')!
    expect(saved.body.rule).toEqual(original)
    expect(saved.body.expected_revision).toBe(1)
})

test('模块、节点、字段、端口与调试器随中英文切换，不出现裸模块 ID', async ({page, admin}) => {
    await admin.open()
    await admin.editRule()
    await expect(page.getByTestId('add-action-json_set')).toHaveText('设置字段值')
    await expect(page.getByTestId('add-condition-any')).toHaveText('任一满足')
    await expect(page.locator('.rule-graph-action').first()).toContainText('设置字段值')
    await expect(page.getByTestId('rule-debugger')).toContainText('传输记录调试器')
    await page.getByLabel('语言', {exact: true}).selectOption('en')
    await expect(page.getByTestId('add-action-json_set')).toHaveText('Set field value')
    await expect(page.getByTestId('add-condition-any')).toHaveText('Any condition')
    await expect(page.locator('.rule-graph-action').first()).toContainText('Set field value')
    await expect(page.getByTestId('rule-debugger')).toContainText('Capture debugger')
    await expect(ruleName(page)).toHaveValue('Fixture request rule')
    await expect(page.locator('.rule-graph-rule .vue-flow__handle[data-handleid="boolean-in"]')).toHaveAttribute('title', 'Boolean condition')
    for (const container of [page.getByTestId('module-library'), page.locator('.rule-graph-node-title'), page.locator('article').first()]) {
        const text = await container.allTextContents()
        expect(text.join(' ')).not.toMatch(/\b(json_set|rewrite_model|drop_input_items|not_exists|response_event|skip_rule)\b/)
    }
    await page.getByLabel('Language', {exact: true}).selectOption('zh-CN')
    await expect(page.getByTestId('add-action-json_set')).toHaveText('设置字段值')
})

test('节点拖动、画布缩放、键盘撤销与明暗主题可操作', async ({page, admin}) => {
    await admin.open()
    await admin.editRule()
    const canvas = page.getByTestId('rule-canvas')
    await canvas.scrollIntoViewIfNeeded()
    const viewport = canvas.locator('.vue-flow__transformationpane')
    const initialTransform = await viewport.getAttribute('style')
    await page.getByTestId('canvas-zoom-in').click()
    await expect.poll(() => viewport.getAttribute('style')).not.toBe(initialTransform)
    await page.getByTestId('canvas-fit').click()
    const node = page.locator('.vue-flow__node').filter({has: page.locator('.rule-graph-rule')})
    const previous = await node.evaluate(element => (element as HTMLElement).style.transform)
    const box = await node.locator('.rule-node-drag-handle').boundingBox()
    expect(box).toBeTruthy()
    await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2)
    await page.mouse.down()
    await page.mouse.move(box!.x + box!.width / 2 + 65, box!.y + box!.height / 2 + 35, {steps: 8})
    await page.mouse.up()
    await expect.poll(() => node.evaluate(element => (element as HTMLElement).style.transform)).not.toBe(previous)
    await addModule(page, 'action', 'text_replace')
    await closeParameters(page)
    await expect(page.locator('.rule-graph-action')).toHaveCount(3)
    await canvas.focus()
    await page.keyboard.press('Control+z')
    await expect(page.locator('.rule-graph-action')).toHaveCount(2)
    const theme = await page.locator('html').getAttribute('data-theme')
    await page.locator('.sidebar-controls button').first().click()
    await expect(page.locator('html')).not.toHaveAttribute('data-theme', theme!)
    await expect(canvas).toBeVisible()
})

test('复制动作通过真实端口拖线接入，循环连接被拒绝', async ({page, admin}) => {
    await admin.open()
    await admin.editRule()
    await page.getByTestId('canvas-fit').click()
    const original = page.locator('.rule-graph-action').nth(1)
    await original.locator('.rule-node-drag-handle').click()
    await closeParameters(page)
    await page.locator('.rule-canvas-toolbar').getByRole('button', {name: '复制', exact: true}).click()
    await expect(page.locator('.rule-graph-action')).toHaveCount(3)
    await page.getByTestId('canvas-fit').click()
    const copied = page.locator('.rule-graph-action').nth(2)
    const source = original.locator('[data-handleid="action-out"]')
    const target = copied.locator('[data-handleid="action-in"]')
    await target.scrollIntoViewIfNeeded()
    await expect(source).toBeInViewport()
    await expect(target).toBeInViewport()
    await source.dragTo(target)
    await expect(page.locator('.vue-flow__edge')).toHaveCount(6)
    await copied.locator('[data-handleid="action-out"]').dragTo(page.locator('.rule-graph-action').first().locator('[data-handleid="action-in"]'))
    await expect(page.locator('.vue-flow__edge')).toHaveCount(6)
    const connected = await readCode(page)
    expect(connected.actions).toHaveLength(3)
    expect(connected.actions[1]!.params).toEqual(connected.actions[2]!.params)
    expect(connected.actions[1]!.id).not.toBe(connected.actions[2]!.id)
})

test('窄屏模块和节点内参数仍可新增、编辑与保存', async ({page, admin}) => {
    await page.setViewportSize({width: 390, height: 844})
    await admin.open()
    await page.getByRole('button', {name: '新增规则', exact: true}).click()
    await ruleName(page).fill('Narrow screen draft')
    await showCanvas(page)
    await expect(page.getByTestId('module-library')).not.toBeVisible()
    await addModule(page, 'action', 'text_replace')
    await focusNode(page)
    await configureModelReplacement(inlineNodeControls(page), 'fixture-mobile-model')
    await closeParameters(page)
    await page.getByTestId('canvas-fit').click()
    const canvas = await page.getByTestId('rule-canvas').boundingBox()
    expect(canvas!.width).toBeGreaterThan(250)
    expect(canvas!.width).toBeLessThanOrEqual(390)
    await ruleEditor(page).getByRole('button', {name: '保存规则', exact: true}).click()
    await expect.poll(() => admin.rules.find(rule => rule.name === 'Narrow screen draft')?.actions[0]?.params.replacement).toBe('fixture-mobile-model')
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(392)
})
