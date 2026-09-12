import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      // 开发期代理到本地网关（go run ./cmd/server 或 docker 容器）
      '/api': {
        target: 'http://127.0.0.1:7863',
        changeOrigin: true,
      },
    },
  },
  build: {
    // 产物直出到 Go embed 目录（internal/web/dist），Dockerfile node 阶段同样路径
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 900,
  },
});
