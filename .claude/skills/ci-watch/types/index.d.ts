export type CiWatchRun = { name: string; elapsedMs: number; expectedMs: number | null }

export type CiWatchItem = { id: string; label: string; runs: CiWatchRun[]; result?: string; isFailed?: boolean }

declare module 'claude-code' {
  interface PluginState {
    'ci-watch': { watches: CiWatchItem[] }
  }
}
