import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import license from "rollup-plugin-license";

// The Go backend serves the built SPA from web/dist via //go:embed and proxies
// nothing itself, so in dev we point /api and /events at the running backend
// (default :8686).
const BACKEND = "http://localhost:8686";

export default defineConfig({
  plugins: [
    react(),
    // Licence notices for the third-party packages that end up in the bundle
    // (not the whole devDependency tree). Written into dist/ so the embedded SPA
    // carries its own notices, and `make notices` copies the file, as web.txt,
    // into the THIRD_PARTY_NOTICES/ folder shipped beside every binary. A
    // bundled package with no declared licence, or one outside the allow-list,
    // fails the build instead of shipping without a notice.
    license({
      thirdParty: {
        allow: {
          test: "(MIT OR ISC OR 0BSD OR BSD-2-Clause OR BSD-3-Clause OR Apache-2.0)",
          failOnUnlicensed: true,
          failOnViolation: true,
        },
        output: {
          file: fileURLToPath(new URL("./dist/third-party-notices.txt", import.meta.url)),
          template: (deps) =>
            deps
              .map((d) =>
                [
                  "=".repeat(80),
                  `${d.name} ${d.version}`,
                  `License: ${d.license}`,
                  "-".repeat(80),
                  "",
                  (d.licenseText ?? "").trim(),
                  d.noticeText ? `\n${d.noticeText.trim()}` : "",
                ].join("\n"),
              )
              .join("\n\n"),
        },
      },
    }),
  ],
  // The UI's own build version, baked in at compile time. The Makefile's `spa`
  // target passes ASIAIR_COMMANDER_VERSION=$(VERSION) — the SAME $(VERSION) it
  // feeds the Go binary's -ldflags — so the embedded SPA and the server it ships
  // inside report the identical string, and VersionBanner only fires on a real
  // skew (a tab left open across a server upgrade). Unset (local/dev, CI's
  // untagged build-test) falls back to "dev", which makes the banner inert.
  define: {
    __APP_VERSION__: JSON.stringify(process.env.ASIAIR_COMMANDER_VERSION || "dev"),
  },
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Assets are content-hashed; index.html is the SPA-history fallback target.
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      // changeOrigin rewrites Host to the backend; we also rewrite Origin. Together
      // this lets the dev server be reached over the LAN (e.g. a phone at
      // http://<mac-ip>:5173) without the browser's real Host/Origin tripping the
      // backend's Host allowlist and CSRF Origin checks (design architecture §8)
      // — those defend the production embedded-SPA deployment; the dev proxy
      // runs on the operator's own trusted machine.
      "/api": {
        target: BACKEND,
        changeOrigin: true,
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyReq) => {
            proxyReq.setHeader("origin", BACKEND);
          });
        },
      },
      "/events": {
        target: BACKEND,
        changeOrigin: true,
        // Long-lived SSE stream plus the Host/Origin rewrite.
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyReq) => {
            proxyReq.setHeader("accept", "text/event-stream");
            proxyReq.setHeader("origin", BACKEND);
          });
        },
      },
      "/healthz": { target: BACKEND, changeOrigin: true },
    },
  },
});
