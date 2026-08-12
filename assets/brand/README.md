# Brand Assets

Pi Worker uses a black-and-white base with verification green reserved for the
verified inner counter.

## Palette

- Near black: `#111111`
- Off white: `#f7f7f4`
- Verification green: `#0e9b4e`

## Files

- `pi-worker-mark.svg`: monochrome mark using `currentColor`
- `pi-worker-mark-accent.svg`: light-surface mark with a green counter
- `pi-worker-avatar.svg`: dark tile with a 10% inset and green counter
- `pi-worker-lockup.svg`: responsive monochrome mark and wordmark
- `pi-worker-512.png`: raster avatar for package and repository surfaces
- `github-social-preview.svg`: editable social preview source
- `github-social-preview.png`: 1280 by 640 social preview export

The wordmark outlines are based on IBM Plex Sans SemiBold 3.005 from
[`@ibm/plex-sans` 1.1.0](https://github.com/IBM/plex), licensed under the
[SIL Open Font License 1.1](https://github.com/IBM/plex/blob/master/LICENSE.txt).
The font binary is not distributed with Pi Worker.

README and npm surfaces use the opaque social preview rather than a transparent
lockup so the identity remains stable across light and dark themes.

The published README references the social preview from the `main` branch.
Treat `assets/brand/github-social-preview.png` as a stable public path: replace
its contents in place when the artwork changes, and do not move or remove the
file while published npm versions depend on it.
