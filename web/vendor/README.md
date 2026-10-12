# Browser libraries

These are the production browser runtime sources used by the console:

- React and React DOM 19.3.0 (MIT)
- Scheduler 0.28.0 (MIT)
- Lucide React 0.468.0 (ISC), limited to the icons imported by the console

Only the browser client production files and their licenses are retained. Server renderers, development/refresh tools, package manifests, package-manager trees and executable runtimes are absent. React's production sources use CommonJS module syntax; the Go esbuild API resolves those modules at build time into a browser ES module. They are browser UI code and do not invoke Node.

`sources.json` records exact upstream package paths, versions and SHA-256 checksums. Lucide's `index.js` is the project's small export list over the unchanged upstream icon files. Update that list and vendor any required upstream icon with its license if the UI adds another icon.

Build both `web/dist` and the embedded `cmd/tosub2/web/dist` with:

```sh
go run ./cmd/build-web
```

The builder rejects bare imports outside this list, adds the stylesheet and content-addressed asset URLs to the HTML, and includes the full licenses in `THIRD_PARTY_NOTICES.txt`. No Node, npm, pnpm, Vite or Python is needed for this build.
