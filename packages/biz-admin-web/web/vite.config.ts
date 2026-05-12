import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// dev: vite proxy /api 到本地 biz-admin-web Go server (它再代理到各服务).
// prod: Go server 直接 serve embed 后的 dist/.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
  build: {
    outDir: '../static-react',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // hash 文件名给 cache busting
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },
});
