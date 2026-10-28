LexAI(legal-rag-chatbot deploy/migcompare.py)가 실제로 남기는 형식의 표본 — 판정(judge)과 콘솔(api compareSummary)이 읽는지 확인한다.
가짜 AI 서버로 만든 것이다: 타겟의 Ollama 버전만 달라 q02 의 문장이 달라진 경우(WARN), 나머지는 모두 일치.
  results.csv            verify.sh 결과표 (RUN_TS 로 비교표와 같은 시각)
  migration-compare.csv  비교표 (WidgetRAG 와 같은 열)
  run-evidence/          증적 폴더 (evidence/ 는 .gitignore 대상이라 이름을 바꿨다) ~/lexai-run/migration-<시각>/ 의 summary.txt · rows.tsv · golden-diff/
