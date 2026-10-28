# web

`migrate serve` 가 내는 웹 화면입니다. React 18 · React Router 7 · Vite 6 로 만들고, 빌드 결과(`dist/`)는 `embed.go` 를 통해 `migrate` 실행 파일에 포함됩니다.
API 는 저장소의 `api/`(Go)에 있습니다.

## 빌드

저장소 맨 위에서 실행합니다.

```bash
make build
```

`npm ci` → `npm run build` → `go build` 순서로 진행합니다. 화면만 다시 빌드하려면 `make web` 을 씁니다.

## 개발

한 터미널에서 API 서버를 띄웁니다.

```bash
./migrate serve
```

다른 터미널에서 Vite 개발 서버를 띄웁니다.

```bash
make dev
```

브라우저로 `http://127.0.0.1:5173` 을 엽니다. Vite 가 `/api` 요청을 `serve` 로 넘기고, 화면 코드를 저장하면 바로 반영됩니다. API(Go)를 고쳤으면 `go build -o migrate ./cmd/migrate` 뒤 `serve` 를 다시 띄웁니다.

## 구조

```
src/
├── main.jsx               경로 설정
├── api.js                 API 호출
├── Layout.jsx             왼쪽 메뉴 · 공통 표시
├── styles.css             스타일
└── pages/
    ├── Runs.jsx           이관 목록
    ├── NewMigration.jsx   새 이관 — 워크로드 · 소스 · 리전 선택
    ├── Preview.jsx        이관 확인 — 추천 타겟 확인 · Migrate
    └── RunPage.jsx        run 상세 — 진행 · 판정 · 로그 · 증적
```
