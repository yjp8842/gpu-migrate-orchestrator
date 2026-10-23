# gpu-migrate-orchestrator — GPU 추론서비스 이관 오케스트레이터

사례 설정 파일 하나를 받아 8단계를 순서대로 부르는 Go 명령줄 도구다. 실행 파일 하나라 따로 설치할 것이 없다.
Cloud-Barista 코어(CB-Tumblebug · CM-Beetle)는 고정 버전을 REST 로만 부르고, 워크로드(WidgetRAG · LexAI)의 차이는 `adapters/` 설정에만 둔다.

| 단계 | 하는 일 | 부르는 것 |
| --- | --- | --- |
| collect | Tumblebug 메타(노드 · 스펙 · 이미지 · vNet · 보안 그룹)를 Beetle 소스 모델 `source-model.json` 으로 바꾼다. 소스가 중지돼 있어도 된다 | Tumblebug 조회 |
| recommend | Beetle 후보 → 정책 선택 → 보정(이미지 · 디스크 · 포트). Tumblebug 직접 생성이면 사례의 spec 또는 recommendSpec | Beetle 추천 · Tumblebug 스펙/이미지 조회 |
| provision | 타겟 생성 → Running → 원격 명령으로 접속 확인 → 보안 그룹에 없는 열 포트만 추가 | Beetle 생성 또는 Tumblebug infraDynamic |
| package | 소스 상태 확인(서비스 · LLM 설정 · 식별 → `identify.json`) → `package.sh backup` | Tumblebug 원격 명령 |
| transfer | 패키지를 타겟 PACKAGE_DIR 로 Beetle 데이터 이관(소스 → 타겟 rsync) → 타겟에서 체크섬 · 파일 목록 대조 | Beetle |
| start | 단일 명령을 분리 실행하고, 1분마다 확인 명령으로 재부팅 재개까지 따라간다 | Tumblebug 원격 명령 |
| verify | 결과표 · 비교표 · 증적 폴더를 원격 명령 출력으로 받아 PASS · FAIL · NO-RESULT | Tumblebug 원격 명령 |
| cleanup | 타겟 suspend · terminate · keep, 남은 키 파일 파기, `summary.md` | Tumblebug 제어 |

## 빌드 · 실행

```bash
go build -o migrate ./cmd/migrate
```

```bash
cp .env.example .env
```

`.env` 에 Tumblebug · Beetle 주소와 계정, 타겟 웹 포트를 열 출발지, 검증용 관리자 비밀번호를 채운다 (아래 「환경변수」).

```bash
./migrate run --case cases/aws-to-aws-widgetrag-a.yaml --dry-run
```

| 명령 | 용도 |
| --- | --- |
| `run --case …` | 끝까지. `--dry-run` 은 조회는 실제로 하고 생성 · 제어 · 원격 명령 · 이관은 요청 본문만 `run/<단계>/dry-run.json` 에 쓴다 |
| `run --case … --resume latest` | 멈춘 run 을 멈춘 단계부터. 실패로 끝난 재기동 · backup · Beetle 이관은 새 요청으로 다시 보낸다. `latest` 는 가장 최근 run 폴더라, 사이에 dry-run 을 돌렸으면 run-id 를 직접 준다 |
| `run --case … --from start --to verify` | 단계 범위만 (지정한 단계는 done 이어도 다시) |
| `verify --case … --run latest` | 단계 하나만 |
| `status --case …` | 읽기 전용 — 소스 인프라 상태 · 최근 run |
| `note --case … --stage provision "VPC 한도 상향 신청"` | 사람이 한 조치 기록 (수동 개입 구간 목록의 원천) |
| `beetle-selftest` | Beetle 데이터 이관 암호화 형식 확인 |

`--settings` 는 하위 명령 앞뒤 어디에 써도 된다. 종료 코드는 0 정상 · 1 판정 FAIL 또는 NO-RESULT · 2 오류다.
실행 파일은 자기 폴더에서 `adapters/` 를 찾고, 없으면 현재 폴더부터 위로 찾는다 (`GMP_ROOT` 로 지정할 수도 있다).

## VM 접근 방식

도구는 VM 에 SSH 로 접속하지 않는다. 로컬 PC 는 Tumblebug · Beetle API 에만 닿으면 되고, VM 의 22 포트에 닿지 않아도 된다.

- **확인 · 읽기 · 스크립트는 Tumblebug 원격 명령.** 스크립트를 `bash -c` 로 감싸고 끝에 종료 코드 표식을 찍어 출력에서 읽는다. 파일은 `gzip | base64`, 폴더는 `tar.gz | base64` 로 받는다.
- **패키지 이관은 Beetle 데이터 이관.** 개인키는 Tumblebug 에서 메모리로 받아 Beetle 암호화 봉투로 바로 감싼다 (파일로 두지 않는다). 대안 경로는 없어서 Beetle 이 안 되면 transfer 단계에서 멈춘다. 경로는 각 VM 의 `$HOME` 을 읽어 절대 경로로 넘긴다 — direct 는 소스 VM 에서 `rsync … <소스>/ <user>@<타겟>:<경로>` 를 돌리는데, 타겟 쪽 `~` 는 소스 셸이 풀지 않는다. 보내기 전에 두 VM 의 `rsync` 를 확인하고, 없으면 설치한다.
- **관리자 비밀번호.** WidgetRAG 배포 스크립트의 기본 비밀번호로 소스 · 타겟이 같은 값으로 뜬다. 사례의 `run.env` 의 `VERIFY_ADMIN_PASSWORD` 가 환경변수 `GMP_VERIFY_ADMIN_PASSWORD` 로 같은 값을 받아 로그인 판정을 명시한다. 이관된 타겟은 소스 DB 의 계정을 그대로 쓰므로 이 값으로 로그인까지 판정한다. 이 값은 원격 명령에 실려 Tumblebug 명령 이력과 run 폴더에 남는다 (로그에서는 가린다).
- **대신 생기는 것.** 확인할 때마다 Tumblebug 에 명령 기록이 남는다 (기동 확인 간격 `pacing.start_poll_sec`, 기본 60초). 재부팅 중에는 원격 명령이 닿지 않아 기다렸다가 다시 본다.

## 사례 · 어댑터 · 설정

- `cases/*.yaml` — 이번 운행에서 바뀌는 값만: 소스 인프라, 타겟 CSP · 리전 · provider(beetle | tumblebug), 정책(GPU 선택 · 이미지 · 드라이버 시리즈 · 디스크 · 열 포트), 이관 방식(기본 beetle), run.env, cleanup.
- `adapters/*.yaml` — 워크로드가 다른 점 전부: 단일 명령, 재기동 · 패키지 생성 명령, 패키지 경로, 결과 파일 위치, 기동 로그 위치, 재개 유닛, 상태 확인 위치. WidgetRAG 두 벌은 채워져 있고 LexAI 두 벌은 자리만 있다. 명령 템플릿의 `{env}` 는 사례 `run.env` 전체(타겟 단일 명령용)이고, `{source_env}` 는 그중 `LLM_TEMPERATURE` · `LLM_SEED` 만이다(소스 재기동용 — `CODE_SOURCE` · `COMPARE` · `FORM` · 비밀번호는 소스에 넘기지 않는다).
- `settings.example.yaml` — Tumblebug(이름별) · Beetle 주소와 계정, 증적 루트, 제한 시간, 호출 간격. 주소 · 계정은 환경변수로 받으므로 대개 그대로 쓴다. 고쳐 쓰려면 `settings.yaml` 로 복사한다 (gitignore, 있으면 이쪽을 먼저 읽는다).

## 환경변수

서버 주소 · 계정 · 비밀번호 · 사무실 IP 는 저장소에 두지 않는다. 설정 · 사례 파일에는 `${이름}`(꼭 필요) · `${이름:-값}`(없으면 값) 으로만 적고, 실행할 때 환경변수로 채운다. 비어 있는 변수가 있으면 이름을 모두 적고 멈춘다. `migrate` 는 시작할 때 프로젝트 폴더의 `.env`(gitignore)를 읽고, 셸에서 export 한 값이 그보다 우선한다. 어댑터는 셸 명령을 담고 있어 치환하지 않는다.

| 변수 | 쓰는 곳 | 뜻 |
| --- | --- | --- |
| `GMP_TB_URL` · `GMP_TB_USER` · `GMP_TB_PASSWORD` | settings | Tumblebug 주소(`http://<서버>:1323/tumblebug`)와 계정 |
| `GMP_BEETLE_URL` · `GMP_BEETLE_USER` · `GMP_BEETLE_PASSWORD` | settings | Beetle 주소(`http://<서버>:8056/beetle`, 밖에서 막혀 있으면 Beetle UI 프록시 `:3080/beetle`)와 계정 |
| `GMP_WEB_ALLOW_CIDR` | cases | 타겟 80 포트를 열 출발지 (예: 사무실 공인 IP/32) |
| `GMP_VERIFY_ADMIN_PASSWORD` | cases | 타겟 검증 로그인에 쓰는 WidgetRAG 관리자 비밀번호 |
| `GMP_EVIDENCE_ROOT` | settings (선택) | 증적 폴더, 기본 `evidence` |
| `GMP_ROOT` | 실행 파일 (선택) | 프로젝트 폴더 |

증적은 `evidence_root/<사례>/<run-id>/` 에 쌓인다: `state.json`(단계 상태 · 재개용 키 · 개입 기록 · 판정) · `log.txt` · `http.jsonl`(요청 요약, 본문은 해시) ·
`collect/source-model.json` · `recommend/rationale.md` · `package/identify.json` · `transfer/verify-remote.txt` · `start/tc-run.log` · `verify/verdict.json` · `summary.md`.

## 알아 둘 것

- **재실행 안정성.** 인프라 이름 · 원격 명령 X-Request-Id · 패키지 경로를 부작용 전에 state 에 적고, 재개 때 조회로 이어 간다. 생성 · 원격 명령 POST 는 자동 재시도하지 않고, 429 로 거절된 요청만 다시 보낸다.
- **재개 때 앞선 요청 다루기.** 재기동 · backup 원격 명령과 Beetle 이관은 적어 둔 요청의 상태부터 본다. 도는 중이거나 성공이면 그 요청을 이어서 보고, 실패로 끝났거나 기록이 없으면(보내기 전에 멈춤) 새 ID 로 다시 보낸다. Beetle 이관은 성공했지만 타겟 대조에서 떨어진 경우도 다시 보낸다. 앞선 시도는 `state.json` 의 `<키>_tries` 에 남고, 그 시도의 산출물은 `backup.try1.stdout.txt` 처럼 이름을 바꿔 둔다. 앞선 시도가 남긴 원격 폴더(소스의 backup 출력 폴더, 타겟의 패키지 폴더)는 지우지 않고 `<폴더>.failed-<N>` 으로 옮긴다. 같은 실행 안에서는 다시 보내지 않는다 — 실패하면 멈추고, 원인을 본 뒤 `--resume` 으로 다시 보낸다. 기동 단계는 여기에 들지 않는다(재개하면 확인만 다시 한다).
- **dry-run 은 조회를 실제로 한다.** 쓰기 여부는 메서드가 아니라 호출별로 정한다 (제어는 GET 이지만 쓰기, searchImage · recommendSpec · Beetle 추천은 POST 지만 조회, Beetle 암호화 키 발급은 GET 이지만 쓰기). 소스가 아직 없으면 자리표시자 모델(AWS g4dn.xlarge · T4 · Ubuntu 24.04)로 추천받고 `rationale.md` 에 적는다.
- **분리 실행.** 단일 명령은 드라이버 설치 뒤 재부팅하고, Tumblebug 는 끊긴 원격 명령을 한 번 더 실행한다. 그래서 즉시 반환하는 launcher 만 보낸다. 완료는 `results.csv` 행 수 증가로만 본다.
- **호출 제한.** `GET /ns/{ns}/infra[/{id}]` 는 출발 IP 당 2 req/s 를 MapUI 사용자와 나눠 쓴다 → 1초 간격, 429 는 지수 백오프.
- **Tumblebug 특성.** 공용 스펙 · 이미지는 ns `system` 에 있고, 이미지 ID 는 CSP 이미지 이름이다. 없는 자원은 500 + "does not exist" 로 답해 "없음"으로 읽는다.
- **CODE_SOURCE=package.** 이관 운행의 타겟은 패키지의 실행 파일 트리로만 뜬다.

## 검증 상태 (2026-10-22)

- `go test ./...` — 60개 통과 (설정 · 상태 · 클라이언트 재시도와 dry-run 차단 · 정책 · 판정 · 소스 모델 · 상태 확인 · 원격 명령 표식 · 홈 경로 · 폴더 받기 · Beetle 이관 본문의 키 암호화 · 단계).
- 파이썬 판과 같은 사례로 비교: 두 사례 dry-run 과 종료된 소스 기록의 실제 수집 → 추천에서 Tumblebug · Beetle 요청 순서와 소스 모델 · 추천 후보 · 생성 요청이 같았다.
- **생성부터 정리까지는 아직 돌리지 않았다.** 테스트 4(AWS→AWS)가 첫 실 운행이다. Beetle 생성 · 데이터 이관도 아직 호출하지 않았다.
- 파이썬 1차 구현은 `legacy-python/` 에 보관했다 (그 판의 결함과 차이는 그 폴더 README).

## 결정 필요

- GPU 가 바뀐 사례(예: AWS T4 → GCP L4)의 테스트2 판정 기준 · 성능 임계값과 다음 후보 재선정 도입 시점
- 80 포트 개방 범위 · 증적 루트 위치와 키 파일 보존 · 저장소 위치(지금은 로컬 폴더만)

## 구조

```
cmd/migrate/            CLI — 하위 명령 · 단계 실행 · 종료 코드
internal/config         사례 · 어댑터 · 설정 읽기와 검사
internal/state          run 폴더 · state.json · 잠금 · 재개
internal/logx           로그 · http.jsonl · 비밀값 가림
internal/tumblebug      Tumblebug 클라이언트 (호출 간격 · 재시도 규칙 · dry-run 차단)
internal/beetle         Beetle 클라이언트
internal/crypto         Beetle 데이터 이관 비밀 필드 암호화
internal/sourcemodel    Tumblebug 메타 → Beetle 소스 모델
internal/inspect        소스 VM 상태 확인 명령과 해석
internal/policy         후보 선택 · 보정
internal/judge          결과표 · 비교표 → 판정
internal/stages         8단계 + 공통 문맥 (원격 명령 기반 VM 접근)
adapters/  cases/  testdata/  legacy-python/
```
