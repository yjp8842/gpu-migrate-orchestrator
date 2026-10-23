"""gpu-migrate-orchestrator — GPU 추론서비스 이관 오케스트레이터.

사람이 MapUI · Remote Command · scp 로 하던 열 단계(수집 → 추천 → 생성 → 패키지 → 전송 → 기동 → 검증 → 정리)를
사례 설정 파일 하나로 순서대로 부르는 CLI. 워크로드(WidgetRAG · LexAI)의 차이는 adapters/ 의 설정에만 두고
파이프라인 코드는 워크로드를 모른다. Cloud-Barista 코어(Tumblebug · Beetle)는 고정 버전을 API 로만 부른다.
"""
__version__ = "0.1.0"
