# Security

The supported release is **the latest published version**. Releases have always moved forward from the previous one since v0.1.x; no release has ever been a backport to an older line.

Workers execute `bash` with the current user's permissions in the current writable workspace. Pi Worker is not a sandbox.

Do not post credentials, Pi profiles, provider configuration, prompts,
workspace contents, or sensitive logs in public issues. Do not disclose a
vulnerability publicly.

Use [GitHub private vulnerability reporting](https://github.com/arasovic/pi-worker/security/advisories/new).
If that channel is unavailable, do not disclose publicly or send secrets
elsewhere. Publication requires the private reporting channel to be enabled
first.
