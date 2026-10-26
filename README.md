# gpu-migrate-orchestrator

GPU 추론서비스를 한 클라우드에서 다른 클라우드로 옮기고, 옮긴 서비스가 같은지 판정하는 도구입니다.
Cloud-Barista 의 CB-Tumblebug · CM-Beetle API 를 불러 타겟 생성 → 패키지 이관 → 기동 → 동등성 판정 → 정리를 한 번에 수행하고, 단계마다 증적을 남깁니다.

- 실행 파일 하나(`migrate`)로 명령줄과 웹 화면을 모두 제공합니다.
- VM 에 SSH 로 접속하지 않습니다. 실행하는 PC 는 Tumblebug · Beetle API 에만 닿으면 됩니다.
- 워크로드(WidgetRAG · LexAI)마다 다른 점은 `adapters/` 설정 파일에만 있습니다.

## 요구 사항

| 항목 | 버전 |
| --- | --- |
| Go | 1.26 이상 |
| Node.js | 20 이상 (화면을 빌드할 때만) |
| OS | Linux 또는 macOS |
| CB-Tumblebug · CM-Beetle | 접속할 수 있는 서버와 계정 |

## 빠른 시작

### 1. 빌드

```bash
make build
```

화면(`web/`)을 빌드한 뒤 `migrate` 실행 파일을 만듭니다. 화면 없이 명령줄만 쓸 때는 `go build -o migrate ./cmd/migrate` 만으로도 됩니다.

Linux 서버용 실행 파일은 `make linux` 로 만듭니다 (`bin/migrate-linux-amd64`).

### 2. 환경변수 설정

```bash
cp .env.example .env
```

`.env` 에 Tumblebug · Beetle 주소와 계정, 타겟 웹 포트를 열 출발지를 채웁니다. 각 항목은 아래 [환경변수](#환경변수)를 봅니다.

### 3. 사례 파일 만들기

```bash
cp cases/case.sample.yaml cases/aws-to-gcp-widgetrag-a.yaml
```

복사한 파일에서 소스 인프라 이름, 타겟 CSP · 리전, 타겟 이름을 채웁니다. 나머지 항목은 기본값으로 둬도 됩니다. 항목 설명은 [사례 파일](#사례-파일)을 봅니다.

### 4. 사전 확인 (dry-run)

```bash
./migrate run --case cases/aws-to-gcp-widgetrag-a.yaml --dry-run
```

소스 조회와 Beetle 추천은 실제로 하고, 타겟 생성 · 원격 명령 · 이관은 보내지 않고 요청 본문만 증적 폴더에 남깁니다.

### 5. 이관

```bash
./migrate run --case cases/aws-to-gcp-widgetrag-a.yaml
```

8단계를 순서대로 수행하고 결과를 `evidence/<사례>/<run-id>/` 에 남깁니다. 중간에 멈추면 `--resume <run-id>` 로 멈춘 단계부터 이어 갑니다.

## 웹 화면으로 사용하기

```bash
./migrate serve
```

브라우저로 `http://127.0.0.1:5180` 을 엽니다. 화면에서 하는 일은 다음과 같습니다.

1. **새 이관** — 워크로드, Tumblebug 네임스페이스(기본 `default`)와 그 안의 소스 인프라, 타겟 CSP · 리전을 고르고 「다음 단계」를 누릅니다.
2. **이관 확인** — 고른 소스와 Beetle 이 추천한 타겟(사양 · GPU · OS · 예상 비용)을 확인하고 「Migrate」를 누릅니다.
3. **run 상세** — 단계 진행, 실시간 로그, 판정 결과, 골든 응답 차이, 증적 파일을 봅니다. 증적은 ZIP 으로 받을 수 있습니다.

- 기본으로 판정 단계까지만 수행하고 정리 단계 앞에서 멈춥니다. 타겟 화면을 확인한 뒤 「정리 단계 실행」을 누릅니다.
- 화면에서 만든 사례 파일은 `cases/console/` 에 저장되며, 명령줄에서 그대로 다시 실행할 수 있습니다.
- `serve` 는 `127.0.0.1` 에만 열립니다. 다른 PC 에서 보려면 SSH 터널을 씁니다.

```bash
ssh -L 5180:127.0.0.1:5180 <serve 를 띄운 PC>
```

## 명령

```
migrate <명령> [옵션]
```

| 명령 | 설명 |
| --- | --- |
| `run --case <파일>` | 8단계를 처음부터 끝까지 수행 |
| `run --case <파일> --dry-run` | 조회 · 추천만 실제로 하고 나머지는 요청 본문만 기록 |
| `run --case <파일> --resume <run-id\|latest>` | 멈춘 run 을 멈춘 단계부터 이어서 수행 |
| `run --case <파일> --from <단계> --to <단계>` | 지정한 단계 범위만 수행 |
| `<단계> --case <파일> [--run <run-id>]` | 단계 하나만 수행 (`collect` … `cleanup`) |
| `status --case <파일>` | 소스 인프라 상태와 최근 run 요약 (읽기만) |
| `note --case <파일> [--stage <단계>] "<내용>"` | 사람이 한 조치를 run 기록에 남김 |
| `summary --case <파일> [--run <run-id>]` | `summary.md` 를 다시 씀 |
| `beetle-selftest` | Beetle 데이터 이관 암호화 형식 확인 |
| `serve [--addr 127.0.0.1:5180]` | 웹 화면과 API 서버 실행 |

공통 옵션 `--settings <파일>` 로 설정 파일을 바꿀 수 있습니다.
종료 코드는 `0` 정상, `1` 판정 FAIL 또는 결과 없음, `2` 오류입니다.

### 단계

| 단계 | 하는 일 |
| --- | --- |
| collect | Tumblebug 에서 소스 인프라 정보(사양 · GPU · 이미지 · 네트워크 · 보안 그룹)를 읽어 Beetle 소스 모델로 변환 |
| recommend | Beetle 추천 후보 중 정책에 맞는 타겟을 고르고 이미지 · 디스크 · 포트를 보정 |
| provision | Beetle(또는 Tumblebug)로 타겟 생성, 접속 확인, 열 포트 추가. 존에 자원이 없으면 같은 리전의 다른 존으로 다시 시도 |
| package | 소스 서비스 상태를 확인하고 패키지 생성 |
| transfer | Beetle 데이터 이관으로 패키지를 타겟에 복사하고 체크섬 대조 |
| start | 타겟에서 설치 · 기동 명령을 실행하고 완료될 때까지 확인 (드라이버 설치 뒤 재부팅 포함) |
| verify | 타겟의 검증 결과를 받아 PASS · FAIL 판정 |
| cleanup | 타겟을 멈춤 · 삭제 · 유지 중 사례에 적은 대로 처리하고 요약 작성 |

## 설정

### 환경변수

주소 · 계정 · 비밀번호는 저장소에 두지 않고 환경변수로 받습니다. `migrate` 는 시작할 때 프로젝트 폴더의 `.env` 를 읽고, 셸에서 export 한 값이 더 우선합니다.

| 변수 | 필수 | 설명 |
| --- | --- | --- |
| `GMP_TB_URL` | ✓ | Tumblebug 주소 (예: `http://<서버>:1323/tumblebug`) |
| `GMP_TB_USER` · `GMP_TB_PASSWORD` | ✓ | Tumblebug 계정 |
| `GMP_BEETLE_URL` | ✓ | Beetle 주소 (예: `http://<서버>:8056/beetle`) |
| `GMP_BEETLE_USER` · `GMP_BEETLE_PASSWORD` | ✓ | Beetle 계정 |
| `GMP_WEB_ALLOW_CIDR` | ✓ | 타겟 웹 포트를 열 출발지 (예: `203.0.113.10/32`) |
| `GMP_VERIFY_ADMIN_PASSWORD` | WidgetRAG | 타겟 검증 로그인에 쓰는 관리자 비밀번호 (`.env.example` 의 기본값) |
| `GMP_EVIDENCE_ROOT` | | 증적 폴더 (기본 `evidence`) |
| `GMP_CONSOLE_MAX_RUNS` | | `serve` 에서 동시에 돌릴 수 있는 이관 수 (기본 2) |
| `GMP_ROOT` | | 프로젝트 폴더 (기본은 실행 파일이 있는 폴더) |

### 설정 파일

`settings.example.yaml` 에 Tumblebug · Beetle 접속, 증적 폴더, 단계별 제한 시간, API 호출 간격이 있습니다. 주소와 계정은 환경변수로 채워지므로 보통 그대로 씁니다.
값을 바꾸려면 `settings.yaml` 로 복사해 고칩니다. `settings.yaml` 이 있으면 그 파일을 먼저 읽습니다.

### 사례 파일

`cases/*.yaml` 은 이관 한 건의 값입니다. `cases/case.sample.yaml` 을 복사해 만들고, 각 항목의 뜻과 선택지는 그 파일의 주석에 있습니다.

| 항목 | 설명 |
| --- | --- |
| `workload` | 어댑터 이름 (`widgetrag-a` · `widgetrag-b` · `lexai-a` · `lexai-b`) |
| `source` | 소스 인프라 (Tumblebug 네임스페이스 · 인프라 이름) |
| `target` | 타겟 CSP · 리전 · 존(비우면 자동) · 이름 |
| `policy` | GPU 선택 방식 · 이미지 · 루트 디스크 · 열 포트 |
| `transfer` | 데이터 이관 방식 (`direct` · `relay`) |
| `run` | 타겟 기동에 넘길 환경변수, 기동 제한 시간 |
| `cleanup` | 정리 단계에서 타겟 처리 (`suspend` · `terminate` · `keep`) |

설정 · 사례 파일에서 `${이름}` 은 환경변수로 채워지고, `${이름:-값}` 은 변수가 없을 때 값을 씁니다.

### 어댑터

`adapters/*.yaml` 은 워크로드별 설치 · 패키지 · 판정 방법입니다. 새 워크로드는 어댑터 파일 하나를 추가해 붙입니다.

워크로드가 갖춰야 할 것은 네 가지입니다.

1. 소스에서 패키지를 만드는 명령 (출력 폴더에 `checksums.sha256` 을 남김)
2. 타겟에서 옮겨 온 패키지만으로 설치 · 기동하는 명령
3. 진행과 결과를 알리는 파일 — 결과표(CSV) 또는 상태 파일(JSON)
4. 실행 중인 기동 명령을 `pgrep -f` 로 찾을 수 있는 이름

| 항목 | 필수 | 설명 |
| --- | --- | --- |
| `workload` · `form` | ✓ | 워크로드 이름 · 형태 |
| `entry` | ✓ | 타겟 설치 · 기동 명령. `{env}` · `{package_dir}` 를 치환 |
| `backup` · `backup_out` · `package_dir` | ✓ | 소스 패키지 명령 · 출력 폴더 · 타겟 패키지 경로 |
| `results` | ✓ | 판정에 쓰는 결과표(`csv`) 또는 상태 파일(`status`) |
| `proc_pattern` | ✓ | 기동 명령을 찾는 `pgrep -f` 패턴 |
| `done` | | 기동 완료 판정 방법 (기본: 결과표 행 증가) |
| `transfer` | | 이관할 폴더 목록 (기본: 패키지 폴더 하나) |
| `diagnostics` | | 판정 뒤 타겟에서 받아 둘 진단 정보 |
| `source_restore` | | 정리 단계에서 소스에 실행할 명령 |
| `rebuild` · `health` · `identify` | | 패키지 전 소스 재기동 · 상태 확인 방법 |

전체 항목은 `adapters/widgetrag-a.yaml` 을 참고합니다.

## 결과물 (증적)

run 마다 `<증적 폴더>/<사례>/<run-id>/` 가 만들어집니다.

```
<run-id>/
├── state.json          단계 상태 · 재개 정보 · 사람 개입 기록 · 판정
├── summary.md          사람이 읽는 요약
├── log.txt             진행 로그
├── http.jsonl          API 호출 기록
├── collect/            소스 정보 · 소스 모델
├── recommend/          추천 후보 · 선택 근거 · 생성 요청
├── provision/          실제로 보낸 생성 요청 · 타겟 노드 정보
├── package/            패키지 목록 · 체크섬 · 소스 상태
├── transfer/           이관 요청 · 타겟 대조 결과
├── start/              타겟 설치 · 기동 출력
└── verify/             판정 결과 · 비교 자료 · 골든 응답 차이 · 타겟 진단
```

비밀번호와 키는 로그와 증적에 쓸 때 가려집니다.

## 프로젝트 구조

```
cmd/migrate/            실행 파일 진입점 (명령줄 · serve)
internal/
  stages/               8단계 실행
  config/               사례 · 어댑터 · 설정 읽기
  state/                run 폴더 · 상태 · 잠금 · 재개
  tumblebug/ beetle/    Tumblebug · Beetle 클라이언트
  sourcemodel/          Tumblebug 정보 → Beetle 소스 모델
  policy/               추천 후보 선택 · 보정
  judge/                판정
  inspect/              소스 상태 확인
  crypto/               Beetle 데이터 이관 암호화
  logx/ jsonx/ textx/   로그 · JSON · 문자열 도구
api/                    웹 화면용 API (serve)
web/                    웹 화면 (React · Vite) — 빌드 결과가 실행 파일에 포함됨
adapters/               워크로드별 설정
cases/                  사례 파일 (case.sample.yaml 견본)
testdata/               시험용 고정 자료
```

## 개발

| 명령 | 설명 |
| --- | --- |
| `make build` | 화면 빌드 후 실행 파일 빌드 |
| `make test` | Go 시험 실행 (`go test ./...`) |
| `make serve` | 빌드 후 `migrate serve` 실행 |
| `make dev` | 화면 개발 서버 (Vite, `127.0.0.1:5173`) |
| `make linux` | Linux amd64 실행 파일 빌드 |
| `make clean` | 빌드 결과 삭제 |

화면을 고칠 때는 한 터미널에서 `./migrate serve` 를, 다른 터미널에서 `make dev` 를 실행합니다. `http://127.0.0.1:5173` 을 열면 Vite 개발 서버가 API 요청을 `serve` 로 넘기고 화면을 저장할 때마다 바로 반영됩니다. 자세한 내용은 [web/README.md](web/README.md) 를 봅니다.
