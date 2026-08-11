---
name: pi-worker
description: Use when an agent delegates work through Pi, needs cheaper or separately metered models, or assigns one to three Pi workers.
---

# Pi Worker

1. Confirm `pi-worker` is on `PATH` before proposing or running work.
2. Resolve an informal model name with `pi-worker models --json --debug --timeout 30s`. Select one unambiguous exact `provider/model`; report ambiguity and stop.
3. Pass an exact model unchanged with `--model`. If it is unavailable or unauthenticated, report the setup action and stop. Never substitute another model or provider.
4. Treat reasoning effort separately. Map an explicit `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, or `max` request to `--thinking <level>`. “Spark Max” means exact Spark plus `--thinking max`. If effort is omitted, omit `--thinking`; never guess it or encode it in the model selector.
5. If no model is named, omit `--model` so the configured personal default applies. If it is missing, stop with the CLI guidance.
6. Put each requested task in an external task file. Create one to three files only. Run parallel workers only for disjoint tasks; serialize overlapping writes or combine them into one worker.
7. Run `pi-worker run` with `--task-file`, a bounded `--timeout`, `--json`, and `--debug`.
8. Parse the single JSON document. Return each worker's model, effective `thinkingLevel`, status, explanation, failures, and setup action. If `thinkingFallback` is true, report its `warning` prominently: Pi continued with the selected model's confirmed default effort. Do not repeat debug output or raw frames.
9. Never ask a Pi worker to invoke `$pi-worker` or delegate again.

```sh
pi-worker run --model <provider/model> --thinking <level> --task-file <task-a.txt> --task-file <task-b.txt> --timeout <duration> --json --debug
```

## Common mistakes

- Do not invent `pi-worker` subcommands or infer a model from a nickname.
- Do not fall back after an unavailable explicit model; only pi-worker may retain Pi's confirmed default effort and report it.
- Do not turn `max` into a model suffix or silently add effort when none was requested.
- Do not send four or overlapping write tasks in parallel.
- Do not treat debug output as the result; use the final JSON document.
