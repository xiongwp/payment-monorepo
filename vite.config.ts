import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'path'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 3100,
    proxy: {
      // /api → payment-admin-web backend (localhost:9190)
      '/api': {
        target: 'http://localhost:9190',
        changeOrigin: true,
      },
    },
  },
})
