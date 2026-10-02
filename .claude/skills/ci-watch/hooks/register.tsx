import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { CiWatchItem } from '../types'

const POLL_MS = 10_000
const NO_RUN_MS = 90_000
const MAX_WATCH_MS = 60 * 60_000
const BAR_WIDTH = 20

type Run = { workflowName: string; workflowDatabaseId: number; event: string; headBranch: string; status: string; conclusion: string; startedAt: string }

// One watch per ref and commit: a branch push and the PR opened on it merge into one row instead of two.
type Watch = { labels: string[]; required: Map<string, number> }

const watches = atom({ plugin: 'ci-watch', key: 'watches' } as const, [] as CiWatchItem[])
const active = new Map<string, Watch>()
let repoUrl: string | undefined

// Bash calls start in the session cwd, so a push from elsewhere arrives as `cd <repo> && git push` or `git -C <repo>`.
// ponytail: only a leading cd or git -C; pushd and subshells fall back to the session cwd.
export const repoDir = (command: string, home: string, root: string) => {
  const m = command.match(/^\s*cd\s+("[^"]+"|'[^']+'|[^\s;&|]+)\s*(&&|;)/) ?? command.match(/\bgit\s+-C\s+("[^"]+"|'[^']+'|\S+)/)
  if (!m?.[1]) return root
  const dir = m[1].replace(/^["']|["']$/g, '')
  if (dir === '~' || dir.startsWith('~/')) return home + dir.slice(1)
  return dir.startsWith('/') ? dir : `${root}/${dir}`
}

// This mod watches this repo only; a push or PR in another checkout is someone else's. Worktrees live under the root.
export const isInside = (dir: string, root: string) => !dir.split('/').includes('..') && (dir === root || dir.startsWith(`${root}/`))

// The engine's gitOperation.push covers branches only; tag pushes (release.yml) are read from git's own output.
export const pushedTag = (output: string) => output.match(/\[new tag\]\s+\S+\s*->\s*(\S+)/)?.[1]

const runName = (r: Pick<Run, 'workflowName' | 'event'>) => (r.event === 'pull_request' ? `${r.workflowName} (PR)` : r.workflowName)

export const summarize = (label: string, runs: Pick<Run, 'workflowName' | 'event' | 'conclusion'>[]) => {
  const failed = runs.filter(r => r.conclusion !== 'success' && r.conclusion !== 'skipped' && r.conclusion !== 'neutral')
  return failed.length
    ? `✗ ${label}: ${failed.map(r => `${runName(r)} ${r.conclusion}`).join(', ')}`
    : `✓ ${label}: ${runs.map(runName).join(', ')} passed`
}

// Progress is elapsed time against the workflow's last successful run: monotonic, unlike step counts,
// which drop when a dependent job starts and reveals its steps. Never shows full until the run completes.
export const bar = (elapsedMs: number, expectedMs: number | null) => {
  const filled = expectedMs ? Math.min(BAR_WIDTH - 1, Math.round((elapsedMs / expectedMs) * BAR_WIDTH)) : 0
  return '█'.repeat(filled) + '░'.repeat(BAR_WIDTH - filled)
}

export const duration = (ms: number) => `${Math.floor(ms / 60_000)}m${String(Math.floor(ms / 1000) % 60).padStart(2, '0')}s`

const gh = async ($: EngineInterface, cwd: string, args: string[]) => {
  const r = await $.process.run(['gh', ...args], { cwd, timeoutMs: 20_000 }).catch(() => undefined)
  return r?.exitCode === 0 ? r.stdout.trim() : undefined
}

const git = async ($: EngineInterface, cwd: string, args: string[]) => {
  const r = await $.process.run(['git', ...args], { cwd }).catch(() => undefined)
  return r?.exitCode === 0 ? r.stdout.trim() : undefined
}

const lastSuccessMs = async ($: EngineInterface, cwd: string, workflowId: number) => {
  const out = await gh($, cwd, ['run', 'list', '-w', String(workflowId), '--status', 'success', '-L', '1', '--json', 'startedAt,updatedAt'])
  const [r] = out ? (JSON.parse(out) as { startedAt: string; updatedAt: string }[]) : []
  return r ? Date.parse(r.updatedAt) - Date.parse(r.startedAt) : null
}

const setItem = ($: EngineInterface, item: CiWatchItem) => update($, watches, list => [...(list ?? []).filter(w => w.id !== item.id), item])

// A watch ends once every event it waits for has runs and none is pending. An event that never produced a run
// stops being waited for after NO_RUN_MS, so a push CI that finishes before `gh pr create` does not end the watch.
const watch = async ($: EngineInterface, cwd: string, sha: string, ref: string, label: string, event: string) => {
  const id = `${ref}@${sha.slice(0, 7)}`
  const startedAt = await $.clock.now()
  const existing = active.get(id)
  if (existing) {
    existing.required.set(event, startedAt)
    if (!existing.labels.includes(label)) existing.labels.push(label)
    return
  }

  const w: Watch = { labels: [label], required: new Map([[event, startedAt]]) }
  active.set(id, w)
  const expected = new Map<number, number | null>()
  let isBusy = false
  await setItem($, { id, label, runs: [] })

  const finish = async (result: string) => {
    active.delete(id)
    timer.cancel()
    await setItem($, { id, label: w.labels.join(', '), runs: [], result, isFailed: !result.startsWith('✓') })
    $.ui.toast(result)
  }

  const timer = $.clock.every(POLL_MS, async () => {
    if (isBusy || active.get(id) !== w) return
    isBusy = true
    try {
      const out = await gh($, cwd, ['run', 'list', '--commit', sha, '--json', 'workflowName,workflowDatabaseId,event,headBranch,status,conclusion,startedAt'])
      const runs = (out ? (JSON.parse(out) as Run[]) : []).filter(r => r.headBranch === ref && w.required.has(r.event))
      const now = await $.clock.now()
      const label = w.labels.join(', ')
      const pending = runs.filter(r => r.status !== 'completed')
      const isAwaiting = [...w.required].some(([ev, since]) => !runs.some(r => r.event === ev) && now - since <= NO_RUN_MS)
      if (!isAwaiting && !runs.length) {
        await finish(`${label}: no workflow triggered`)
      } else if (!isAwaiting && !pending.length) {
        await finish(summarize(label, runs))
      } else if (now - startedAt > MAX_WATCH_MS) {
        await finish(`${label}: still running after 60 min`)
      } else {
        for (const r of pending) {
          if (!expected.has(r.workflowDatabaseId)) expected.set(r.workflowDatabaseId, await lastSuccessMs($, cwd, r.workflowDatabaseId))
        }
        const rows = pending.map(r => ({
          name: runName(r),
          elapsedMs: Math.max(0, now - Date.parse(r.startedAt)) || 0,
          expectedMs: expected.get(r.workflowDatabaseId) ?? null,
        }))
        await setItem($, { id, label, runs: rows })
      }
    } finally {
      isBusy = false
    }
  })
}

type BashResult = { stdout: string; stderr: string; gitOperation?: { push?: { branch: string }; pr?: { number: number; url?: string; action: string } } }

const track = async ($: EngineInterface, command: string, result: BashResult) => {
  const op = result.gitOperation
  const tag = /\bgit\b[^;&|]*\bpush\b/.test(command) ? pushedTag(`${result.stdout}\n${result.stderr}`) : undefined
  const pr = op?.pr?.action === 'created' ? op.pr : undefined
  if (!op?.push && !pr && !tag) return

  const root = await $.session.cwd()
  const cwd = repoDir(command, (await $.env.get('HOME')) ?? '', root)
  if (!isInside(cwd, root)) return

  if (op?.push) {
    const { branch } = op.push
    // ponytail: assumes the remote is origin; another remote falls back to the local branch's sha.
    const sha = (await git($, cwd, ['rev-parse', `origin/${branch}`])) ?? (await git($, cwd, ['rev-parse', branch]))
    if (sha) await watch($, root, sha, branch, `push ${branch}`, 'push')
  }
  if (tag) {
    const sha = await git($, cwd, ['rev-parse', `${tag}^{commit}`])
    if (sha) await watch($, root, sha, tag, `tag ${tag}`, 'push')
  }
  if (pr) {
    repoUrl ??= await gh($, root, ['repo', 'view', '--json', 'url', '-q', '.url'])
    if (pr.url && (!repoUrl || !pr.url.startsWith(`${repoUrl}/`))) return
    const out = await gh($, root, ['pr', 'view', String(pr.number), '--json', 'headRefOid,headRefName'])
    const head = out ? (JSON.parse(out) as { headRefOid: string; headRefName: string }) : undefined
    if (head) await watch($, root, head.headRefOid, head.headRefName, `PR #${pr.number}`, 'pull_request')
  }
}

export const register: Register = on => {
  on('tool.call', { tool: 'Bash' }, async ($, e, next) => {
    const ran = await next(e)
    if (ran.deny === undefined && !ran.isError && ran.result) {
      void track($, e.command, ran.result as BashResult).catch(() => undefined)
    }
    return ran
  })

  // A reload drops the old module's timers (session.start fires again), so its unfinished rows would never end.
  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if ((await read($, watches)).some(w => !w.result)) await update($, watches, list => (list ?? []).filter(w => w.result))
    return started
  })

  // A finished watch stays in the band until the person sends their next prompt.
  on('prompt.submit', async ($, e, next) => {
    if ((await read($, watches)).some(w => w.result)) await update($, watches, list => (list ?? []).filter(w => !w.result))
    return next(e)
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const items = await read($, watches)
    if (e.props.hasSurvey || !items.length) return next(e)

    const { Box, Text } = $.ui.resolve(e)
    const rows = items.flatMap(w => {
      if (w.result) return [<Text key={w.id} color={w.isFailed ? 'red' : 'green'}>{w.result}</Text>]
      if (!w.runs.length) return [<Text key={w.id} dimColor>{`⟳ ${w.label} · waiting for workflows…`}</Text>]
      return w.runs.map((r, i) => (
        <Text key={`${w.id}:${i}`}>
          {`⟳ ${w.label} · ${r.name} ${bar(r.elapsedMs, r.expectedMs)} ${duration(r.elapsedMs)}${r.expectedMs ? ` / ~${duration(r.expectedMs)}` : ''}`}
        </Text>
      ))
    })

    return <Box flexDirection="column">{rows}</Box>
  })
}
