import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    host: true,
    port: 5174,
    proxy: { '/api': { target: 'http://127.0.0.1:8090', changeOrigin: true }, '/v1': { target: 'http://127.0.0.1:8090', changeOrigin: true } },
  },
  build: {
    // 内联字体与小资源，避免产生外部请求
    assetsInlineLimit: 409600,
  },
})