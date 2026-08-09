import { expect, test, type Browser, type Page } from '@playwright/test'

async function player(browser: Browser, code: string, name: string): Promise<Page> {
  const page = await (await browser.newContext()).newPage()
  await page.goto(`/r/${code}`)
  await page.getByLabel('Your name').fill(name)
  await page.getByRole('button', { name: 'Join' }).click()
  return page
}

const score = (page: Page, team: string) => page.locator('.scores tr', { hasText: team }).locator('.score')

test('moderator and two players play a tossup and bonus', async ({ browser }) => {
  const mod = await (await browser.newContext()).newPage()
  await mod.goto('/')
  await mod.getByLabel('Moderator password').fill('hunter22')
  await mod.getByRole('button', { name: 'Create room' }).click()
  await mod.waitForURL(/\/r\/[A-Z0-9]{6}\/mod$/)
  const code = /\/r\/([A-Z0-9]{6})\/mod$/.exec(mod.url())?.[1] ?? ''

  const alice = await player(browser, code, 'Alice')
  const bob = await player(browser, code, 'Bob')

  for (const team of ['Alpha', 'Beta']) {
    await mod.getByLabel('New team name').fill(team)
    await mod.getByRole('button', { name: 'Add team' }).click()
  }
  await mod.getByLabel('Team for Alice').selectOption({ label: 'Alpha' })
  await mod.getByLabel('Team for Bob').selectOption({ label: 'Beta' })

  await mod.getByRole('button', { name: /Start tossup 1/ }).click()
  await alice.getByRole('button', { name: 'BUZZ' }).click()

  // Each key press waits for the moderator's own screen to reach the state it
  // acts on; another page updating first says nothing about this one.
  await expect(mod.getByRole('heading', { name: /awaiting ruling/ })).toBeVisible()
  await expect(mod.locator('.holder')).toContainText('Alice')
  await expect(alice.getByRole('button', { name: 'Your answer' })).toBeVisible()
  await expect(bob.getByRole('button', { name: 'Alice buzzed' })).toBeDisabled()

  await mod.keyboard.press('p') // power
  await expect(mod.getByRole('heading', { name: /bonus/ })).toBeVisible()
  await expect(score(bob, 'Alpha')).toHaveText('15')

  await mod.keyboard.press('1') // bonus part 1 correct
  await expect(mod.locator('.bonus li').first()).toHaveText('✓')
  await expect(score(alice, 'Alpha')).toHaveText('25')

  await mod.keyboard.press('Space') // next
  await expect(mod.getByRole('heading', { name: 'Tossup 2 · ready' })).toBeVisible()
  await expect(mod.getByRole('button', { name: /Export scoresheet/ })).toBeVisible()
})

test('a wrong moderator password is rejected, and the right one works', async ({ browser, request }) => {
  const resp = await request.post('/api/rooms', { data: { moderatorPassword: 'correct-horse' } })
  const { code } = (await resp.json()) as { code: string }
  const mod = await (await browser.newContext()).newPage()
  await mod.goto(`/r/${code}/mod`)
  await mod.getByLabel('Moderator password').fill('wrong-guess')
  await mod.getByRole('button', { name: 'Moderate' }).click()
  await expect(mod.getByLabel('Moderator password')).toBeVisible() // back at the form
  await mod.getByLabel('Moderator password').fill('correct-horse')
  await mod.getByRole('button', { name: 'Moderate' }).click()
  await expect(mod.getByRole('button', { name: /Start tossup 1/ })).toBeVisible()
})
