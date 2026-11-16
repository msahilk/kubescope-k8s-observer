import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// Serve the repo-root learning docs at /docs/* in dev and copy them into the
// production build, so the Learn panel links always work.
const DOCS: Record<string, string> = {
  'LEARNING_GUIDE.md': 'text/markdown; charset=utf-8',
  'GKE_SETUP_TUTORIAL.html': 'text/html; charset=utf-8',
};

function docsPlugin(): Plugin {
  const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
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
