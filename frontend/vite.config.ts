import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Optional local docs in the repo root (kept out of Git). When present they are
// served at /docs/* in dev and copied into the production build; the Learn
// panel only links to the ones that exist, so a fresh clone builds fine.
const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const DOC_TYPES: Record<string, string> = {
  'LEARNING_GUIDE.md': 'text/markdown; charset=utf-8',
  'GKE_SETUP_TUTORIAL.html': 'text/html; charset=utf-8',
};
const DOCS: Record<string, string> = Object.fromEntries(
  Object.entries(DOC_TYPES).filter(([name]) => existsSync(resolve(ROOT, name))),
);

function docsPlugin(): Plugin {
  const root = ROOT;
  return {
    name: 'kubescope-docs',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const name = req.url?.startsWith('/docs/') ? decodeURIComponent(req.url.slice(6).split('?')[0]) : '';
        if (name && DOCS[name]) {
          res.setHeader('Content-Type', DOCS[name]);
          res.end(readFileSync(resolve(root, name)));
          return;
        }
        next();
      });
    },
    generateBundle() {
      for (const name of Object.keys(DOCS)) {
        this.emitFile({ type: 'asset', fileName: `docs/${name}`, source: readFileSync(resolve(root, name)) });
      }
    },
  };
}

export default defineConfig({
  plugins: [react(), docsPlugin()],
  define: {
    __KUBESCOPE_LOCAL_DOCS__: JSON.stringify(Object.keys(DOCS)),
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    proxy: {
      // Same-origin from the browser's point of view; no CORS needed.
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: false },
    },
  },
});
