// Facade API(migrate serve) 호출 — 실패하면 서버가 준 오류 문구로 Error 를 던진다.
// GET 이 아닌 요청은 늘 JSON 으로 보낸다 (서버는 다른 사이트의 폼 전송을 막으려고 JSON 만 받는다).
async function req(method, url, body) {
  const write = method !== 'GET';
  const res = await fetch(url, {
    method,
    headers: write ? { 'Content-Type': 'application/json' } : undefined,
    body: write ? JSON.stringify(body || {}) : undefined
  });
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = text;
  }
  if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
  return data;
}

export const api = {
  info: () => req('GET', '/api/info'),
  workloads: () => req('GET', '/api/workloads'),
  namespaces: () => req('GET', '/api/namespaces'),
  sources: (ns) => req('GET', `/api/sources?ns=${encodeURIComponent(ns || 'default')}`),
  regions: () => req('GET', '/api/regions'),
  preview: (body) => req('POST', '/api/preview', body),
  caseFile: (name) => req('GET', `/api/cases/${encodeURIComponent(name)}`),
  migrate: (body) => req('POST', '/api/migrate', body),
  runs: () => req('GET', '/api/runs'),
  run: (c, r) => req('GET', `/api/runs/${c}/${r}`),
  log: (c, r, offset) => req('GET', `/api/runs/${c}/${r}/log?offset=${offset || 0}`),
  files: (c, r) => req('GET', `/api/runs/${c}/${r}/files`),
  targetLive: (c, r) => req('GET', `/api/runs/${c}/${r}/target`),
  fileText: async (c, r, p) => {
    const res = await fetch(`/api/runs/${c}/${r}/file?path=${encodeURIComponent(p)}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.text();
  },
  note: (c, r, body) => req('POST', `/api/runs/${c}/${r}/note`, body),
  stop: (c, r) => req('POST', `/api/runs/${c}/${r}/stop`),
  cleanup: (c, r) => req('POST', `/api/runs/${c}/${r}/cleanup`),
  deleteTarget: (c, r, confirm) => req('POST', `/api/runs/${c}/${r}/delete-target`, { confirm })
};

export const STAGE_LABEL = {
  collect: '수집',
  recommend: '추천',
  provision: '타겟 생성',
  package: '패키지',
  transfer: '이관',
  start: '기동',
  verify: '판정',
  cleanup: '정리'
};

export function fmtSec(s) {
  if (s === null || s === undefined) return '—';
  if (s < 60) return `${s}초`;
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  return h ? `${h}시간 ${m}분` : `${m}분 ${String(sec).padStart(2, '0')}초`;
}

export function fmtBytes(n) {
  if (n < 1024) return `${n}B`;
  if (n < 1048576) return `${(n / 1024).toFixed(1)}KB`;
  return `${(n / 1048576).toFixed(1)}MB`;
}

export function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getMonth() + 1}/${d.getDate()} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
