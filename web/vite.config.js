import fs from 'node:fs';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// 빌드 결과 dist/ 는 migrate 실행 파일에 담긴다 (web/embed.go 의 go:embed).
// dist/.gitkeep 은 화면을 빌드하지 않은 상태에서도 go build 가 되도록 두는 자리 — 빌드가 폴더를 비우므로 다시 만든다.
// 개발 중에는 vite(127.0.0.1:5173)가 /api 를 migrate serve(5180)로 넘긴다.
const keepDist = {
  name: 'keep-dist',
  closeBundle() {
    fs.writeFileSync('dist/.gitkeep', '');
  }
};

export default defineConfig({
  plugins: [react(), keepDist],
  // changeOrigin: false — Host 를 5173 그대로 넘겨야 serve 의 같은 출처 검사(Origin = Host)를 지난다
  server: { host: '127.0.0.1', port: 5173, proxy: { '/api': { target: 'http://127.0.0.1:5180', changeOrigin: false } } }
});
