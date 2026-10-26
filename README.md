# gpu-migrate-orchestrator — GPU 추론서비스 이관 오케스트레이터

사례 설정 파일 하나를 받아 8단계를 순서대로 부르는 Go 명령줄 도구다. 실행 파일 하나라 따로 설치할 것이 없다.
Cloud-Barista 코어(CB-Tumblebug · CM-Beetle)는 고정 버전을 REST 로만 부르고, 워크로드(WidgetRAG · LexAI)의 차이는 `adapters/` 설정에만 둔다.

| 단계 | 하는 일 | 부르는 것 |
| --- | --- | --- |
| collect | Tumblebug 메타(노드 · 스펙 · 이미지 · vNet · 보안 그룹)를 Beetle 소스 모델 `source-model.json` 으로 바꾼다. 소스가 중지돼 있어도 된다 | Tumblebug 조회 |
| recommend | Beetle 후보 → 정책 선택 → 보정(이미지 · 디스크 · 포트). Tumblebug 직접 생성이면 사례의 spec 또는 recommendSpec | Beetle 추천 · Tumblebug 스펙/이미지 조회 |
| provision | 타겟 생성 → Running → 원격 명령으로 접속 확인 → 보안 그룹에 없는 열 포트만 추가 | Beetle 생성 또는 Tumblebug infraDynamic |
| package | 소스 상태 확인(서비스 · LLM 설정 · 식별 → `identify.json`) → `package.sh backup` | Tumblebug 원격 명령 |
| transfer | 어댑터의 전송 목록(기본: 패키지 폴더 하나 → PACKAGE_DIR)을 Beetle 데이터 이관(소스 → 타겟 rsync) → 타겟에서 체크섬 · 파일 목록 대조 | Beetle |
| start | 단일 명령을 분리 실행하고(이미 돌고 있으면 띄우지 않음), 1분마다 확인 명령으로 재부팅 재개 · 어댑터의 완료 판정까지 따라간다 | Tumblebug 원격 명령 |
| verify | 결과표 · 비교표 · 증적 폴더를 원격 명령 출력으로 받아 PASS · FAIL · NO-RESULT | Tumblebug 원격 명령 |
| cleanup | 타겟 suspend · terminate · keep (Beetle 생성분 terminate 는 Beetle 삭제 API), 소스 되돌리기(어댑터 source_restore), `summary.md` | Tumblebug 제어 · Beetle |

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
| `note --case … --stage provision "VPC 한도 상향 신청"` | 사람이 한 조치 기록 (수동 개입 구간 목록의 원천) — `summary.md` 도 다시 쓴다 |
| `summary --case … --run latest` | `summary.md` 만 다시 쓴다 (`state.json` 기준, 원격 호출 없음). `run` 은 단계 실행이 끝날 때마다 다시 쓴다 |
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

- `cases/*.yaml` — 이번 운행에서 바뀌는 값만: 소스 인프라, 타겟 CSP · 리전 · provider(beetle | tumblebug), 정책(GPU 선택 · 이미지(기본 `source` = 소스와 같은 OS · 버전, `ubuntu-24.04` 처럼 지정 가능, `keep` = Beetle 추천 그대로) · 드라이버 시리즈(기본 비움 — 추천 GPU 이미지를 드라이버 버전과 상관없이 그대로, `"595"` 처럼 적으면 그 시리즈가 내장된 이미지만) · 디스크 · 열 포트), 이관 방식(기본 beetle), run.env, cleanup. 새 이관 조합은 `cases/case.sample.yaml` 을 복사해 만든다 — 조합마다 바꾸는 값(소스 인프라 · 타겟 CSP · 리전 · 이름 등)은 비어 있고 주석에 예시가 있다.
- `adapters/*.yaml` — 워크로드가 다른 점 전부 (아래 「어댑터 항목」). 도구 코드는 워크로드를 모르고, 새 서비스는 어댑터 한 장으로 붙인다. WidgetRAG 두 벌(결과표로 판정)과 LexAI 두 벌(상태 파일 `~/lexai-run/status.json` 으로 판정, 전송 2건: 패키지 + 저장소 · 배포 디렉터리)이 채워져 있다. `diagnostics` 는 판정 단계가 타겟에서 받아 `verify/target/` 에 두는 것(파일 이름 · 명령)이다. 명령 템플릿의 `{env}` 는 사례 `run.env` 전체(타겟 단일 명령용)이고, `{source_env}` 는 그중 `LLM_TEMPERATURE` · `LLM_SEED` 만이다(소스 재기동용 — `CODE_SOURCE` · `COMPARE` · `FORM` · 비밀번호는 소스에 넘기지 않는다).
- `settings.example.yaml` — Tumblebug(이름별) · Beetle 주소와 계정, 증적 루트, 제한 시간, 호출 간격. 주소 · 계정은 환경변수로 받으므로 대개 그대로 쓴다. 고쳐 쓰려면 `settings.yaml` 로 복사한다 (gitignore, 있으면 이쪽을 먼저 읽는다).

## 어댑터 항목

새 서비스는 어댑터 한 장으로 붙인다. 도구 코드에는 특정 워크로드의 경로 · 패키지 · 열 이름을 두지 않는다.

**워크로드가 지킬 인터페이스**

1. 소스에서 출력 폴더를 인자로 받는 패키지 생성 명령. 그 폴더에 `checksums.sha256` 을 남긴다 (이관 뒤 대조).
2. 타겟에서 옮겨 온 패키지만으로 적재 · 기동하는 진입 한 줄 (원격에서 새로 받지 않는다).
3. 진행을 알리는 위치 — 결과표(CSV) 또는 상태 파일(JSON, 문자열 값) 중 하나 이상.
4. 기동 중인 진입 한 줄을 `pgrep -f` 로 찾을 수 있는 이름.

| 항목 | 필수 | 뜻 |
| --- | --- | --- |
| `workload` · `form` | ✓ | 워크로드 이름 · 형태(shell · compose 등, 기록용) |
| `entry` | ✓ | 타겟 진입 한 줄. `{env}`(사례 run.env) · `{package_dir}`, 어댑터에 있으면 `{repo_raw}` · `{branch}` |
| `backup` · `backup_out` · `package_dir` | ✓ | 소스 패키지 생성 명령(`{out_dir}` · `{golden}`) · 출력 폴더(`{ts}`) · 타겟 패키지 경로 |
| `results` | ✓ | 판정 근거 — `csv`(결과표) · `status`(상태 파일) 중 하나 이상 |
| `proc_pattern` | ✓ | 진입 한 줄을 찾는 `pgrep -f` 패턴 — 이미 돌고 있으면 다시 띄우지 않는다 |
| `results.columns` | | 결과표의 판정 열 — `{result, pass, fail, timestamp}` (기본 `result` · `PASS` · `fail` · `timestamp`). 이번 회차 행의 result 가 pass 값이고 fail(있으면)이 0 이면 PASS |
| `results.status` | | 상태 파일 판정 — `{path, require: {필드: [허용 값]}}`. 결과표가 없으면 이것만으로 판정한다 (`require` 필수, 파일이 없으면 NO-RESULT) |
| `results.compare` · `results.evidence` | | 골든 비교표 · 증적 폴더 글롭 (있는 워크로드만) |
| `done` | | 기동 완료 판정 — `results_rows`(결과표 행 증가, 결과표가 있을 때 기본) 또는 `{type: status_file, path, field, success: [], failure: []}`(결과표가 없을 때 기본, 이번 기동 뒤 갱신된 파일만 본다. 진입 한 줄의 종료 코드 0 만으로는 끝내지 않는다) |
| `transfer` | | 전송 목록 — `[{from, to, exclude: [], delete, fresh, verify}]`. `{backup_out}` · `{package_dir}` 사용. 제외 목록은 source · destination 양쪽에 들어가고, `~` 는 Beetle 에 보낼 때 노드 사용자 홈 절대경로로 바뀐다. 없으면 `[{from: "{backup_out}/", to: "{package_dir}", fresh: true, verify: true}]` |
| `package_marker` | | 패키지가 다 만들어졌다는 표식 파일 (기본 `checksums.sha256`) — 있으면 재개 때 backup 을 건너뛴다 |
| `package_expect` | | 패키지 `package.env` 에서 기대하는 값 — 다르면 경고. 예: `{APP_STATUS: included}` |
| `logs` | | 분리 실행 출력 · 종료 코드 파일 (기본 `~/tc-run.log` · `~/tc-run.exit`) |
| `resume_unit` | | 드라이버 재부팅 뒤 이어서 도는 systemd 유닛 (있는 워크로드만) |
| `existing_check` · `existing_env` | | 타겟 기존 설치 확인 명령(종료 0 = 있음) · 지우고 진행할 때 붙일 환경변수. 사례 `run.on_existing: stop`(기본) · `clean` |
| `source_restore` | | cleanup 에서 소스에 돌릴 명령 (패키지 생성이 서비스를 멈추는 워크로드) |
| `rebuild` · `restart` | | backup 전 소스 재기동 명령. 사례 `run.rebuild_before_backup: true` 일 때만 (기본은 소스를 건드리지 않음) |
| `health` · `golden_check` · `health_fields` | | 소스 health 주소 · 골든 전제(LLM 설정) 확인 여부(기본: health 가 있으면 true) · 응답 필드 이름(기본 `llm_temperature` · `llm_seed`) |
| `identify` | | 소스 상태 확인 — `units` · `compose` · `env_file`(키만) · `config` · `tree` · `code_ref`(커밋 확인 명령) · `packages`(버전을 남길 dpkg 이름) · `versions`(버전 명령) |
| `repo_raw` · `branch` · `services` · `ports` | | 진입 스크립트 주소 · 기록용 |

사례의 `run.golden`(골든 질문 포함 backup)은 기본 false — 골든 응답이 있는 워크로드만 켠다.

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
`collect/source-model.json` · `recommend/rationale.md` · `package/identify.json` · `transfer/verify-remote.txt` · `start/tc-run.log` · `verify/verdict.json` · `verify/target/`(타겟 서비스 로그 · 앱 로그 · GPU · 모델 상태 — 판정과 무관하게 받는다) · `summary.md`.

## 알아 둘 것

- **재실행 안정성.** 인프라 이름 · Beetle nameSeed · 원격 명령 X-Request-Id · 패키지 경로를 부작용 전에 state 에 적고, 재개 때 조회로 이어 간다. 생성 · 원격 명령 POST 는 자동 재시도하지 않고, 서버가 받지 않은 요청(429, Beetle 비동기 시작의 503)만 Retry-After 만큼 쉬고 다시 보낸다. Beetle 은 같은 X-Request-Id 를 409 로 거절하는데, 이미 받은 요청이라 상태를 이어서 본다.
- **Beetle 생성.** nameSeed 는 `target.name` + run 시각(월일시분, 20자 이하)으로 run 마다 다르고 `useExisting=false` 로 보낸다 — 기본값 true 는 같은 이름의 vNet · SSH 키 · 보안 그룹을 재사용한다. 생성 응답을 못 받고 멈췄으면 재개 때 nameSeed 로 Beetle 인프라 목록에서 찾아 이어 간다.
- **존 자동 넘김 (Beetle 생성).** Beetle 은 VM 을 서브넷 존에 만들고, 추천은 존별 자원을 보지 않는다. 시도 순서는 사례 `target.zone`(있으면) → 연결의 기본 존(`/connConfig` 의 `assignedZone`) → 그 리전의 나머지 존. 생성이 존 자원 부족(GCP `does not have enough resources` · `ZONE_RESOURCE_POOL_EXHAUSTED`, AWS `InsufficientInstanceCapacity` 등)으로 실패하면 그 시도가 남긴 인프라를 Beetle 삭제 API 로 지우고(vNet · 보안 그룹 · SSH 키 포함) 다음 존으로 다시 보낸다. 쿼터 초과 같은 다른 오류는 넘기지 않고 멈춘다. 시도는 `state.json` 의 `provision_zone_tries`, 마지막에 보낸 본문은 `provision/migrate-request.sent.json`. 재개 때 앞선 시도의 인프라가 Failed 로 남아 있으면 같은 방식으로 지우고 다음 존부터 이어 간다.
- **재개 때 앞선 요청 다루기.** 재기동 · backup 원격 명령과 Beetle 이관은 적어 둔 요청의 상태부터 본다. 도는 중이거나 성공이면 그 요청을 이어서 보고, 실패로 끝났거나 기록이 없으면(보내기 전에 멈춤) 새 ID 로 다시 보낸다. Beetle 이관은 성공했지만 타겟 대조에서 떨어진 경우도 다시 보낸다. 앞선 시도는 `state.json` 의 `<키>_tries` 에 남고, 그 시도의 산출물은 `backup.try1.stdout.txt` 처럼 이름을 바꿔 둔다. 앞선 시도가 남긴 원격 폴더(소스의 backup 출력 폴더, 타겟의 패키지 폴더)는 지우지 않고 `<폴더>.failed-<N>` 으로 옮긴다. 같은 실행 안에서는 다시 보내지 않는다 — 실패하면 멈추고, 원인을 본 뒤 `--resume` 으로 다시 보낸다. 기동 단계는 여기에 들지 않는다(재개하면 확인만 다시 한다).
- **dry-run 은 조회를 실제로 한다.** 쓰기 여부는 메서드가 아니라 호출별로 정한다 (제어는 GET 이지만 쓰기, searchImage · recommendSpec · Beetle 추천은 POST 지만 조회, Beetle 암호화 키 발급은 GET 이지만 쓰기). 소스가 아직 없으면 자리표시자 모델(AWS g4dn.xlarge · T4 · Ubuntu 24.04)로 추천받고 `rationale.md` 에 적는다.
- **분리 실행.** 단일 명령은 드라이버 설치 뒤 재부팅하고, Tumblebug 는 끊긴 원격 명령을 한 번 더 실행한다. 그래서 즉시 반환하는 launcher 만 보내고, 이미 돌고 있으면 띄우지 않는다. 완료는 어댑터 `done` 으로 본다(기본 `results.csv` 행 수 증가).
- **relay 스테이징.** relay 전략은 Beetle 컨테이너의 `/tmp/transx-staging/<소스 폴더 이름>` 에 사본을 남기고 다음 이관 때만 비운다. API 로 지울 수 없어 cleanup 이 지울 명령을 로그 · summary 에 남긴다. 되도록 direct 를 쓴다.
- **호출 제한.** `GET /ns/{ns}/infra[/{id}]` 는 출발 IP 당 2 req/s 를 MapUI 사용자와 나눠 쓴다 → 1초 간격, 429 는 지수 백오프.
- **Tumblebug 특성.** 공용 스펙 · 이미지는 ns `system` 에 있고, 이미지 ID 는 CSP 이미지 이름이다. 없는 자원은 500 + "does not exist" 로 답해 "없음"으로 읽는다.
- **CODE_SOURCE=package.** 이관 운행의 타겟은 패키지의 실행 파일 트리로만 뜬다.

## 검증 상태 (2026-10-22)

- `go test ./...` — 119개 통과 (설정 · 상태 · 클라이언트 재시도와 dry-run 차단 · 정책 · 판정 · 소스 모델 · 상태 확인 · 원격 명령 표식 · 홈 경로 · 폴더 받기 · Beetle 이관 본문의 키 암호화 · 단계 · 10/23 범용 어댑터: 전송 목록 · 완료 판정 · 상태 파일 판정 · 기존 설치 · 재기동 중복 방지 · Beetle nameSeed · 503/409 · Beetle 삭제 · 소스 되돌리기 · 결과표 없는 워크로드 · 결과표 열 이름 · 상태 확인 명령 · Beetle 경로의 홈 절대경로화 · 소스 OS 그대로).
- 이전 파이썬 판(저장소에는 두지 않는다)과 같은 사례로 비교: 두 사례 dry-run 과 종료된 소스 기록의 실제 수집 → 추천에서 Tumblebug · Beetle 요청 순서와 소스 모델 · 추천 후보 · 생성 요청이 같았다.
- **생성부터 정리까지는 아직 돌리지 않았다.** 테스트 4(AWS→AWS)가 첫 실 운행이다. Beetle 생성 · 데이터 이관도 아직 호출하지 않았다.

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
adapters/  cases/  testdata/
```
