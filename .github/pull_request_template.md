## What this changes

<!-- And why. The why is the part that is hard to recover from the diff later. -->

## How it was verified

<!--
CI already runs gofmt, go vet, go test -race, the Windows/Unix cross-compile
gates, and
npm run verify, so there is no need to say those pass.

Worth saying:
- for a bug fix, the test that fails without it, and that you watched it fail
- for a concurrency or process-lifecycle change, the repeated run you used
  (count, -race, GOMAXPROCS) rather than a single green run
- for anything touching the JSON result, exit codes, or debug output, that you
  checked the documented contract in docs/json-contracts.md still holds
- for a change to skill installation, the target agents you installed against
-->

## Notes

<!--
Anything you are unsure about, or deliberately left out.

Confirm you have not included prompts, workspace contents, provider
configuration, or credentials in the diff or in the description.
-->
