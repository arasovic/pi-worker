---
name: pi-worker
description: Use when an agent delegates work through Pi, needs cheaper or separately metered models, or assigns one to three Pi workers.
---

# Pi Worker

1. Confirm `pi-worker` is on `PATH` before proposing or running work.
2. If the user names an informal or uncertain model, run `pi-worker models --json --debug --timeout 30s`. Select one unambiguous exact `provider/model` selector. If it is ambiguous, report the candidates and stop.
3. If the user names an exact selector, pass it unchanged with `--model`. Report unavailable or authentication errors and stop; never substitute another model or perform a local fallback.
4. If no model is named, omit `--model` so the configured personal default applies. If it is missing, stop with the CLI guidance.
5. Put each requested task in an external task file. Create one to three files only. Run parallel workers only for disjoint tasks; serialize overlapping writes or combine them into one worker.
6. Run `pi-worker run` with `--task-file`, a bounded `--timeout`, `--json`, and `--debug`.
7. Parse the single JSON document after the run. Return each worker's model, status, explanation, failures, and any required setup action. Do not repeat debug output or raw protocol frames.
8. Never ask a Pi worker to invoke `$pi-worker` or delegate again.

```sh
pi-worker run --model <provider/model> --task-file <task-a.txt> --task-file <task-b.txt> --timeout <duration> --json --debug
```

## Common mistakes

- Do not invent `pi-worker` subcommands or infer a model from a nickname.
- Do not fall back after an unavailable explicit model.
- Do not send four or overlapping write tasks in parallel.
- Do not treat debug output as the result; use the final JSON document.
