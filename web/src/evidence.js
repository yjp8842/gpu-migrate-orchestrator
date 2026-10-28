// 완료 화면의 증적 분류 — run 폴더의 파일을 핵심 증적 · 문제 볼 때 파일 · 나머지(중간 계산 · 중복, ZIP 에만)로 나눈다.
// 핵심 증적은 파일 개수 · 용량, 해시 검증, 질문 응답 동일성 세 가지와 판정 · 보고서다. 워크로드마다 남기는 파일이 달라 있는 것만 보인다.
// re 는 run 폴더 기준 경로 전체와 맞춘다. 이관 항목이 여럿이면 둘째부터 verify-remote-2.txt 처럼 번호가 붙는다.

export const CORE_GROUPS = [
  {
    key: 'report', title: '판정 · 보고서', items: [
      { re: /^summary\.md$/, desc: 'run 요약 — 단계 시각 · 선택 근거 · 판정 · 사람 개입' },
      { re: /^verify\/verdict\.json$/, desc: '최종 판정 — PASS · FAIL 과 그 이유' },
      { re: /^recommend\/rationale\.md$/, desc: '타겟 사양 · 이미지를 고른 근거' }
    ]
  },
  {
    key: 'files', title: '1. 파일 개수 · 용량', items: [
      { re: /^package\/package-files\.tsv$/, desc: '소스 패키지 파일 목록 — 이름 · 바이트 · 해시' },
      { re: /^package\/MANIFEST\.txt$/, desc: '소스 패키지 구성 — 파일 수 · 용량' },
      { re: /^transfer\/verify-remote(-\d+)?\.txt$/, desc: '타겟에 도착한 파일을 목록 · 체크섬과 대조한 결과' }
    ]
  },
  {
    key: 'hash', title: '2. 해시 검증', items: [
      { re: /^package\/checksums\.sha256$/, desc: '소스 패키지 체크섬 (SHA-256)' },
      { re: /^verify\/evidence\/summary\.txt$/, desc: '소스 ↔ 타겟 대조 요약 — 패키지 · DB · 색인 · 업로드 · 응답' },
      { re: /^verify\/evidence\/rows\.tsv$/, desc: '대조 항목 전체 — 항목마다 소스 값 · 타겟 값 · 결과' },
      { re: /^verify\/status\.json$/, desc: '타겟이 남긴 상태 — data_check 는 DB 테이블별 행 수 · 내용 대조' },
      { re: /^verify\/target\/data-check\.txt$/, desc: 'DB 테이블별 원본 · 대상 행 수 · 내용 해시 대조표 (data_check 근거)' }
    ]
  },
  {
    key: 'answer', title: '3. 질문 응답 동일성', empty: '이 워크로드는 이관 때 응답을 대조하지 않습니다', items: [
      { re: /^verify\/evidence\/golden\.json$/, desc: '소스에서 받아 둔 골든 질문 · 응답' },
      { re: /^verify\/evidence\/golden-diff\/[^/]+$/, desc: '소스와 다른 문항의 응답 차이' }
    ]
  }
];

// 문제가 생겼을 때 보는 파일 — 묶음마다 한 줄 설명. 판정에는 쓰지 않는다.
export const DEBUG_GROUPS = [
  { title: '실행 기록', desc: '진행 로그 · 단계 상태 · API 호출', re: /^(log\.txt|state\.json|http\.jsonl)$/ },
  { title: '수집 · 추천 · 생성', desc: '소스 사양, Beetle 후보 원본 · 사전 검사, 보낸 생성 요청', re: /^(collect\/[^/]+|recommend\/(candidates[^/]*|recommend-request|validation|infra-review|target-choice)\.json|provision\/(migrate-request\.sent|target)\.json)$/ },
  { title: '패키지 · 이관', desc: '소스 백업 출력, 소스 식별 정보, Beetle 이관 요청', re: /^(package\/(backup\.stdout|rebuild\.stdout)\.txt|package\/identify\.json|package\/package\.env|transfer\/beetle-request(-\d+)?\.json)$/ },
  { title: '기동 · 판정', desc: '타겟 설치 · 기동 로그, 워크로드 결과표 · 비교표 원본', re: /^(start\/[^/]+|verify\/(results|migration-compare)\.csv|verify\/evidence\/compare\.log)$/ },
  { title: '타겟 상태', desc: '판정 때 받은 서비스 로그 · GPU · 모델 상태', re: /^verify\/target\/[^/]+$/ }
];

// classifyFiles 는 { core: [{...group, files: [{path, size, desc}]}], debug: [{...group, files}], rest } 를 돌려준다.
export function classifyFiles(files) {
  const used = new Set();
  const core = CORE_GROUPS.map((g) => ({
    ...g,
    files: g.items.flatMap((it) => files.filter((f) => it.re.test(f.path)).map((f) => (used.add(f.path), { ...f, desc: it.desc })))
  }));
  const debug = DEBUG_GROUPS.map((g) => ({ ...g, files: files.filter((f) => !used.has(f.path) && g.re.test(f.path)) }))
    .filter((g) => g.files.length > 0);
  debug.forEach((g) => g.files.forEach((f) => used.add(f.path)));
  return { core, debug, rest: files.filter((f) => !used.has(f.path)) };
}
