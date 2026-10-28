# GPU 이관 오케스트레이터 — 화면을 빌드해 실행 파일에 담고(go:embed), migrate 하나로 CLI 와 화면을 다 낸다.

.PHONY: build web test serve dev linux clean help

build: web ## 화면 빌드 → migrate 실행 파일
	go build -o migrate ./cmd/migrate

web: ## 화면만 빌드 (web/dist)
	cd web && (test -d node_modules || npm ci) && npm run build

test: ## Go 시험
	go test ./...

serve: build ## 화면 + Facade API — http://127.0.0.1:5180
	./migrate serve

dev: ## 화면을 고치는 중: 다른 터미널에서 ./migrate serve, 여기서 vite(5173) — /api 는 5180 으로 넘어간다
	cd web && (test -d node_modules || npm ci) && npm run dev

linux: web ## Linux amd64 실행 파일 → bin/migrate-linux-amd64
	GOOS=linux GOARCH=amd64 go build -o bin/migrate-linux-amd64 ./cmd/migrate

clean: ## 빌드 결과 지우기 (web/dist/.gitkeep 은 남김)
	rm -f migrate && rm -rf bin && find web/dist -mindepth 1 ! -name .gitkeep -delete

help:
	@grep -E '^[a-z]+:.*## ' Makefile | sed 's/:.*## /\t/'
