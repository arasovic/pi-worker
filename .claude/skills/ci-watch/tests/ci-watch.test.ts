import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import { bar, duration, isInside, pushedTag, repoDir, summarize } from '../hooks/register'


const ok = (data: unknown) => ({ value: { exitCode: 0, stdout: typeof data === 'string' ? data : JSON.stringify(data), stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })

// Session cwd, state and toasts, shared by the watch tests.
const harness = (on: On) => {
  const clock = mock.clock(on)
  mock.env(on, { HOME: '/home/a' })
  on('session.cwd', () => ({ value: '/repo' }))
  const box = { held: undefined as unknown, version: 0, toasts: [] as string[] }
  on('state.get', () => ({ value: { value: box.held, version: box.version } }))
  on('state.set', (_$, e) => {
    box.held = e.value
    box.version += 1
    return { value: { isSet: true as const, version: box.version } }
  })
  on('ui.toast', (_$, e) => {
    box.toasts.push(e.text)
    return { value: undefined }
  })
  return { clock, box }
}

const run = (workflowName: string, workflowDatabaseId: number, event: string, headBranch: string, conclusion?: string) => ({
  workflowName,
  workflowDatabaseId,
  event,
  headBranch,
  status: conclusion ? 'completed' : 'in_progress',
  conclusion: conclusion ?? '',
  startedAt: new Date(0).toISOString(),
})

test('repo dir and whether it is this repo', () => {
  expect(repoDir('cd ~/Projects/other && git push', '/home/a', '/repo')).toBe('/home/a/Projects/other')
  expect(repoDir('cd .claude/worktrees/x && git push', '/home/a', '/repo')).toBe('/repo/.claude/worktrees/x')
  expect(repoDir('git -C ../pi push origin main', '/home/a', '/repo')).toBe('/repo/../pi')
  expect(repoDir('git push', '/home/a', '/repo')).toBe('/repo')
  expect(isInside('/repo', '/repo')).toBe(true)
  expect(isInside('/repo/.claude/worktrees/x', '/repo')).toBe(true)
  expect(isInside('/repo/../pi', '/repo')).toBe(false)
  expect(isInside('/repo-other', '/repo')).toBe(false)
})

test('tag push, summary, progress bar', () => {
  expect(pushedTag('To github.com:a/b.git\n * [new tag]         v1.2.0 -> v1.2.0\n')).toBe('v1.2.0')
  expect(pushedTag('   abc..def  main -> main')).toBeUndefined()
  const ci = { workflowName: 'CI', event: 'push', conclusion: 'success' }
  expect(summarize('push x', [ci, { ...ci, event: 'pull_request' }])).toBe('✓ push x: CI, CI (PR) passed')
  expect(summarize('push x', [ci, { ...ci, workflowName: 'Dependency Review', event: 'pull_request', conclusion: 'failure' }])).toBe('✗ push x: Dependency Review (PR) failure')
  expect(bar(10_000, 40_000)).toBe('█████' + '░'.repeat(15))
  expect(bar(90_000, 40_000)).toBe('█'.repeat(19) + '░')
  expect(bar(10_000, null)).toBe('░'.repeat(20))
  expect(duration(83_000)).toBe('1m23s')
})

test('a PR opened on a pushed branch joins its watch and waits for the PR runs even after push CI is done', async ($, on) => {
  const { clock, box } = harness(on)
  let phase = 0
  on('process.run', (_$, e) => {
    const a = e.argv.join(' ')
    if (a === 'git rev-parse origin/feat/x') return ok('aaa1111\n')
    if (a.startsWith('gh repo view')) return ok('https://github.com/o/r\n')
    if (a.startsWith('gh pr view 5')) return ok({ headRefOid: 'aaa1111', headRefName: 'feat/x' })
    if (a.includes('--status success')) return ok([{ startedAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:40Z' }])
    if (!a.includes('--commit aaa1111')) return ok('')
    const ciPush = run('CI', 1, 'push', 'feat/x', phase >= 1 ? 'success' : undefined)
    if (phase < 2) return ok([ciPush])
    return ok([ciPush, run('CI', 1, 'pull_request', 'feat/x', phase >= 3 ? 'success' : undefined), run('Dependency Review', 2, 'pull_request', 'feat/x', 'success')])
  })
  on('tool.call', (_$, e) => {
    const command = (e as { command: string }).command
    const gitOperation = command.startsWith('gh pr create') ? { pr: { number: 5, url: 'https://github.com/o/r/pull/5', action: 'created' } } : { push: { branch: 'feat/x' } }
    return { result: { stdout: '', stderr: '', interrupted: false, gitOperation } } as never
  })

  await $.tool.call({ tool: 'Bash', command: 'git push -u origin feat/x' })
  await clock.advance(10_000)
  expect(box.held).toEqual([{ id: 'feat/x@aaa1111', label: 'push feat/x', runs: [{ name: 'CI', elapsedMs: 10_000, expectedMs: 40_000 }] }])

  await $.tool.call({ tool: 'Bash', command: 'gh pr create --draft --fill' })
  await clock.settle()
  phase = 1
  await clock.advance(10_000)
  expect(box.toasts).toEqual([])
  expect(box.held).toEqual([{ id: 'feat/x@aaa1111', label: 'push feat/x, PR #5', runs: [] }])

  phase = 2
  await clock.advance(10_000)
  expect(box.held).toEqual([{ id: 'feat/x@aaa1111', label: 'push feat/x, PR #5', runs: [{ name: 'CI (PR)', elapsedMs: 30_000, expectedMs: 40_000 }] }])

  phase = 3
  await clock.advance(10_000)
  const result = '✓ push feat/x, PR #5: CI, CI (PR), Dependency Review (PR) passed'
  expect(box.toasts).toEqual([result])
  expect(box.held).toEqual([{ id: 'feat/x@aaa1111', label: 'push feat/x, PR #5', runs: [], result, isFailed: false }])
})

test('a tag push waits for its own run, not the branch CI already done on that commit', async ($, on) => {
  const { clock, box } = harness(on)
  let polls = 0
  on('process.run', (_$, e) => {
    if (e.argv[0] === 'git') return ok('bbb2222\n')
    if (e.argv.includes('--status')) return ok([])
    polls += 1
    const ci = run('CI', 1, 'push', 'main', 'success')
    return ok(polls < 2 ? [ci] : [ci, run('Release', 3, 'push', 'v1.0.0', polls < 3 ? undefined : 'success')])
  })
  on('tool.call', () => ({ result: { stdout: '', stderr: ' * [new tag]         v1.0.0 -> v1.0.0', interrupted: false } }) as never)

  await $.tool.call({ tool: 'Bash', command: 'git push origin v1.0.0' })
  await clock.advance(10_000)
  await clock.advance(10_000)
  expect(box.toasts).toEqual([])
  await clock.advance(10_000)
  expect(box.toasts).toEqual(['✓ tag v1.0.0: Release passed'])
})

test('a push or PR in another repo is not watched', async ($, on) => {
  const { clock, box } = harness(on)
  const argvs: string[] = []
  on('process.run', (_$, e) => {
    argvs.push(e.argv.join(' '))
    return ok(e.argv[1] === 'repo' ? 'https://github.com/o/r\n' : '')
  })
  on('tool.call', (_$, e) => {
    const command = (e as { command: string }).command
    const gitOperation = command.includes('gh pr create') ? { pr: { number: 9, url: 'https://github.com/o/other/pull/9', action: 'created' } } : { push: { branch: 'main' } }
    return { result: { stdout: '', stderr: '', interrupted: false, gitOperation } } as never
  })

  await $.tool.call({ tool: 'Bash', command: 'cd ~/Projects/other && git push origin main' })
  await $.tool.call({ tool: 'Bash', command: 'gh pr create --fill' })
  await clock.settle()
  expect(argvs.filter(a => !a.startsWith('gh repo view'))).toEqual([])
  expect(box.held).toBeUndefined()
})
