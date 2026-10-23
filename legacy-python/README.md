# 파이썬 판 (보관)

2026-10-22 에 Go 판(프로젝트 맨 위 `migrate`)으로 옮기기 전의 1차 구현이다. 비교용으로 남겨 둔다.

- 실행: `legacy-python/migrate run --case cases/… --dry-run` (프로젝트 맨 위의 `.venv` 를 쓴다)
- 시험: `cd legacy-python && ../.venv/bin/python -m unittest discover -s tests`

Go 판과 다른 점:

- VM 을 로컬 PC 의 SSH 로 본다. Go 판은 Tumblebug 원격 명령으로 보고, SSH 는 검증용 비밀번호 파일과 이관 대안(stream)에만 쓴다.
- 원격 파일을 읽는 `fetch_text` 가 경로 전체를 따옴표로 감싸 `~/` 가 홈으로 풀리지 않는다. 그래서 검증 단계가 결과표를 못 읽어 늘 NO-RESULT 로 판정한다. Go 판에서 고쳤고, 이 판은 고치지 않았다.
