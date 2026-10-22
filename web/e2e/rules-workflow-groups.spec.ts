import {expect, fixtureRule, test} from './admin.fixture'
import {readCode, showCanvas} from './rules-ui.helpers'

test('黑盒组保留入口连线，多端口可命名，组内修改、保存刷新和解散保真', async ({page, admin}, testInfo) => {
    const original = fixtureRule({
        when: {op: 'always'},
        actions: [0, 1, 2].map(i => ({id: `step-${i}`, type: 'json_set', params: {path: `/x${i}`, value: i}}))
    })
    admin.rules[0] = structuredClone(original)
    await admin.open();
    await admin.editRule(original.name)
    await page.getByTestId('canvas-fit').click()
    const actions = page.locator('.rule-graph-action')
    await actions.nth(0).locator('.rule-node-drag-handle').click()
    await actions.nth(1).locator('.rule-node-drag-handle').click({modifiers: ['Shift']})
    await page.getByTestId('canvas-create-group').click()
    const box = page.locator('.rule-group-node')
    await expect(box).toHaveCount(1)
    await expect(actions).toHaveCount(1)
    await expect(box.locator('.group-port.input')).toHaveCount(1)
    await expect(box.locator('.group-port.output')).toHaveCount(1)
    // Remove and reconnect through the actual group output handle.
    const groupID = await box.getAttribute('data-group-id'), tailID = await actions.first().getAttribute('data-node-id')
    const boundary = page.locator(`.vue-flow__edge[aria-label="Edge from ${groupID} to ${tailID}"]`)
    await expect(boundary).toHaveCount(1)
    await boundary.locator('.vue-flow__edge-interaction').click()
    await page.getByTestId('rule-canvas').press('Delete')
    await expect(boundary).toHaveCount(0)
    await page.getByTestId('canvas-fit').click()
    await page.getByTestId('rule-canvas').scrollIntoViewIfNeeded()
    const from = await box.locator('.group-port.output .vue-flow__handle').boundingBox(),
        to = await actions.first().locator('.vue-flow__handle[data-handleid="action-in"]').boundingBox()
    await page.mouse.move(from!.x + from!.width / 2, from!.y + from!.height / 2)
    await page.mouse.down();
    await page.mouse.move(to!.x + to!.width / 2, to!.y + to!.height / 2, {steps: 12});
    await page.mouse.up()
    await expect(boundary).toHaveCount(1)

    await box.getByLabel('组名', {exact: true}).fill('清理字段')
    await box.getByLabel('组名', {exact: true}).press('Tab')
    await box.locator('.group-port.input input').fill('请求入口')
    await box.locator('.group-port.input input').press('Tab')
    await box.getByRole('button', {name: '管理端口', exact: true}).click()
    const manager = page.locator('.group-port-manager')
    const choices = manager.getByLabel('注册组端口')
    const extra = await choices.locator('option').evaluateAll(options => (options as HTMLOptionElement[]).find(option => option.value !== '' && option.textContent?.includes('value-out'))?.value)
    await choices.selectOption(extra!)
    await manager.getByRole('button', {name: '+', exact: true}).click()
    await expect(box.locator('.group-port.output')).toHaveCount(2)
    await manager.getByRole('button', {name: '关闭', exact: true}).click()
    await page.getByTestId('canvas-fit').click()
    await page.screenshot({path: testInfo.outputPath('workflow-group.png')})
    await box.getByRole('button', {name: '进入组', exact: true}).click()
    await expect(actions).toHaveCount(2)
    await expect(box).toHaveCount(0)
    const path = actions.first().getByLabel('字段路径', {exact: true})
    await path.fill('/edited')
    await page.getByTestId('canvas-leave-group').click()
    await expect(box).toHaveCount(1)
    const expected = structuredClone(original);
    expected.actions[0]!.params.path = '/edited'
    expect(await readCode(page)).toEqual(expected)
    await showCanvas(page)
    await page.locator('form.rule-editor').getByRole('button', {name: '保存规则', exact: true}).click()
    await expect.poll(() => admin.rules[0]!.actions).toEqual(expected.actions)
    await page.reload();
    await admin.editRule(original.name)
    await expect(box).toHaveCount(1)
    await expect(box.getByLabel('组名', {exact: true})).toHaveValue('清理字段')
    await expect(box.locator('.group-port.input input')).toHaveValue('请求入口')
    await box.getByRole('button', {name: '解散', exact: true}).click()
    await expect(box).toHaveCount(0);
    await expect(actions).toHaveCount(3)
    expect((await readCode(page)).actions).toEqual(expected.actions)
})
