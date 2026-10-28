import React, { useEffect, useRef, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { api, fmtBytes, fmtSec, fmtTime, STAGE_LABEL } from '../api.js';
import { Csp, StatusPill, routeOf } from '../Layout.jsx';
import { classifyFiles } from '../evidence.js';

const STATE_LABEL = { done: '✓ 완료', running: '● 진행 중', failed: '✕ 실패', pending: '대기' };

// EvidenceFile 은 증적 파일 한 줄 — 경로 · 크기, 있으면 그 아래 한 줄 설명
function EvidenceFile({ f, href }) {
  return (
    <a className="ef" href={href} target="_blank" rel="noreferrer">
      <span className="mono" title={f.path}>{f.path}</span><small>{fmtBytes(f.size)}</small>
      {f.desc && <em>{f.desc}</em>}
    </a>
  );
}

function logClass(line) {
  if (/ERROR|실패/.test(line)) return 'e';
  if (/WARN/.test(line)) return 'w';
  if (/판정 PASS|통과|완료|만든 인프라|준비됨/.test(line)) return 'g';
  return '';
}

export default function RunPage() {
  const { caseName, runId } = useParams();
  const [d, setD] = useState(null);
  const [err, setErr] = useState('');
  const [log, setLog] = useState('');
  const offset = useRef(0);
  const logBox = useRef(null);
  const [files, setFiles] = useState(null);
  const [diff, setDiff] = useState({ name: '', text: '' });
  const [modal, setModal] = useState(null); // 'note' | 'delete'
  const [noteStage, setNoteStage] = useState('');
  const [noteText, setNoteText] = useState('');
  const [confirmName, setConfirmName] = useState('');
  const [busy, setBusy] = useState('');
  // 타겟의 지금 상태 · 공인 IP (Tumblebug) — suspend 뒤 다시 켜면 IP 가 바뀌어, 기록된 IP 대신 이 값으로 화면을 연다
  const [live, setLive] = useState(null);

  const running = d && d.status.key === 'running';

  useEffect(() => {
    let alive = true;
    offset.current = 0;
    setLog('');
    const loadDetail = () => api.run(caseName, runId).then((x) => alive && setD(x)).catch((e) => alive && setErr(e.message));
    const loadLog = () =>
      api.log(caseName, runId, offset.current).then((r) => {
        if (!alive || !r.text) return;
        offset.current = r.offset;
        setLog((prev) => (prev + r.text).slice(-400000));
      }).catch(() => {});
    loadDetail();
    loadLog();
    const t1 = setInterval(loadDetail, 3000);
    const t2 = setInterval(loadLog, 2000);
    return () => {
      alive = false;
      clearInterval(t1);
      clearInterval(t2);
    };
  }, [caseName, runId]);

  useEffect(() => {
    if (logBox.current) logBox.current.scrollTop = logBox.current.scrollHeight;
  }, [log]);

  useEffect(() => {
    if (d && !running) api.files(caseName, runId).then(setFiles).catch(() => {});
  }, [d && d.status.key, d && d.stages.map((s) => s.status).join()]);

  const hasTarget = !!(d && d.target.infra);
  function loadLive() {
    setLive({ checking: true });
    api.targetLive(caseName, runId).then(setLive).catch((e) => setLive({ err: e.message }));
  }
  useEffect(() => {
    if (d && !running && hasTarget) loadLive();
  }, [d && d.status.key, d && d.target.infra, d && d.stages.map((s) => s.status).join()]);

  async function act(name, fn) {
    setErr('');
    setBusy(name);
    try {
      await fn();
      setModal(null);
      setD(await api.run(caseName, runId));
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy('');
    }
  }

  async function openDiff(name) {
    try {
      setDiff({ name, text: await api.fileText(caseName, runId, `verify/evidence/golden-diff/${name}`) });
    } catch (e) {
      setErr(e.message);
    }
  }

  if (!d) return err ? <div className="err">{err}</div> : <div className="muted"><span className="spin" /> 불러오는 중…</div>;

  const [src, dst] = routeOf(caseName);
  const cleanup = d.stages.find((s) => s.name === 'cleanup');
  const verify = d.stages.find((s) => s.name === 'verify');
  const maxSec = Math.max(1, ...d.stages.map((s) => s.seconds || 0));
  const c = d.compare || {};
  const t1 = (c.tests || []).filter((t) => t.test.startsWith('T1'));
  const t2 = (c.tests || []).find((t) => t.test === 'T2');
  const goldenOk = (c.golden || []).filter((g) => g.result === 'PASS').length;
  // 타겟이 지금 Terminated · 없음이면 도구 밖(MapUI · Tumblebug 직접)에서 지운 것 — run 기록의 「정리 대기」보다 이쪽을 따른다
  const goneStatus = live && !live.checking && !live.err && /^(Terminat|NotFound)/.test(live.status || '') ? live.status : '';
  const waitingCleanup = !running && !goneStatus && verify && verify.status === 'done' && cleanup && cleanup.status === 'pending';
  const leftover = goneStatus && goneStatus !== 'NotFound'; // Terminated 는 인프라 기록 · vNet · 보안 그룹 · 키가 남는다
  const webPort = /lexai/.test(d.workload || '') && /shell|native/.test(d.workload || '') ? ':3000' : '';
  // 열 IP: 지금 Running 이면 Tumblebug 의 IP, 상태를 못 읽었으면 기록된 IP, 타겟 이름이 없는 run 은 기록된 IP
  const liveChecked = live && !live.checking && !live.err;
  const openIP = !hasTarget || (live && live.err) ? d.target.publicIP : liveChecked && live.status === 'Running' ? live.publicIP : null;
  const ipChanged = liveChecked && live.publicIP && d.target.publicIP && live.publicIP !== d.target.publicIP;
  const fileHref = (path) => `/api/runs/${caseName}/${runId}/file?path=${encodeURIComponent(path)}`;
  const evidence = files && classifyFiles(files.files);
  if (evidence) evidence.coreCount = evidence.core.reduce((n, g) => n + g.files.length, 0);
  const judged = d.verdict && d.verdict.result !== 'DRY-RUN'; // 판정까지 갔는데 골든 비교(T2)가 없으면 워크로드가 응답을 대조하지 않는 것

  return (
    <>
      <div className="head">
        <div>
          <Link className="back" to="/">← 이관 목록</Link>
          <h1>
            <span className="mono" style={{ fontSize: 18 }}>{runId}</span>
            <StatusPill status={d.status} />
            {waitingCleanup && <span className="pill p-wait">정리 전 — 타겟 확인 대기</span>}
            {goneStatus && cleanup && cleanup.status !== 'done' && <span className="pill p-stopped">타겟이 도구 밖에서 {goneStatus === 'NotFound' ? '삭제됨' : '종료됨'}</span>}
          </h1>
          <p>
            {src && <><Csp name={src} /> → <Csp name={dst} /> · </>}
            <span className="mono">{caseName}</span> · 시작 {fmtTime(d.started)} · {running ? '경과' : '소요'} {fmtSec(d.elapsed)}
          </p>
        </div>
        <div className="btns">
          {!running && openIP && <a className="btn" href={`http://${openIP}${webPort}/`} target="_blank" rel="noreferrer">타겟 화면 열기</a>}
          {!running && !openIP && hasTarget && (
            <button className="btn" type="button" disabled title={live && live.checking ? '타겟 상태 확인 중' : '타겟이 Running 일 때만 열 수 있습니다'}>타겟 화면 열기</button>
          )}
          <button className="btn" type="button" onClick={() => setModal('note')}>사람 개입 기록</button>
          {running && <button className="btn danger" type="button" disabled={!!busy} onClick={() => act('stop', () => api.stop(caseName, runId))}>중지</button>}
          {waitingCleanup && <button className="btn" type="button" disabled={!!busy} onClick={() => act('cleanup', () => api.cleanup(caseName, runId))}>정리 단계 실행</button>}
          {!running && d.target.infra && goneStatus !== 'NotFound' && (
            <button className="btn danger" type="button" onClick={() => { setConfirmName(''); setModal('delete'); }}
              title={leftover ? 'VM 은 이미 종료됨 — 남은 인프라 기록 · vNet · 보안 그룹 · SSH 키를 지운다' : undefined}>{leftover ? '남은 자원 삭제' : '타겟 삭제'}</button>
          )}
        </div>
      </div>
      {err && <div className="err">{err}</div>}

      <ol className="steps" aria-label="단계">
        {d.stages.map((s) => (
          <li key={s.name} className={s.status}>
            <em>{STATE_LABEL[s.status] || s.status}</em>
            <b>{STAGE_LABEL[s.name]}</b>
            <span>{s.status === 'failed' ? (s.error || '').slice(0, 80) : s.seconds !== null ? fmtSec(s.seconds) : s.name === 'cleanup' && verify && verify.status === 'done' ? '판정 뒤 멈춤' : '—'}</span>
          </li>
        ))}
      </ol>

      {d.verdict && (
        <>
          <div className="grid2">
            <section className="card">
              <h2>테스트1 — 이관 동등성 {t1.length > 0 && <span className={`pill ${t1.every((t) => t.result === 'PASS') ? 'p-pass' : 'p-fail'}`}>{t1.every((t) => t.result === 'PASS') ? 'PASS' : 'FAIL'}</span>}</h2>
              {t1.map((t) => (
                <div className="line" key={t.test}><span>{t.test} {t.item}</span><span className="muted">{t.pass} PASS{t.fail ? ` · ${t.fail} FAIL` : ''}{t.warn ? ` · WARN ${t.warn}` : ''}{t.info ? ` · INFO ${t.info}` : ''}</span></div>
              ))}
              {t1.length === 0 && <span className="muted">{d.verdict.summary}</span>}
              {(c.warns || []).length > 0 && <span className="muted">WARN — {(c.warns || []).slice(0, 4).join(' / ')}</span>}
            </section>
            <section className="card">
              <h2>테스트2 — 질문 응답 동일성 {t2 && <span className={`pill ${t2.result === 'PASS' ? 'p-pass' : 'p-fail'}`}>{t2.result === 'PASS' ? 'PASS' : 'FAIL'} · 일치 {goldenOk} / {(c.golden || []).length}</span>}</h2>
              {(c.golden || []).map((g) => (
                <div className="line" key={g.q}>
                  <span><b className="mono">{g.q}</b> <span className="muted">{g.note}</span></span>
                  {/* WARN — 추천 상품은 같고 환경(GPU · 드라이버 등)이 달라 문장만 다름. 판정은 통과 */}
                  <b style={{ color: g.result === 'PASS' ? 'var(--ok)' : g.result === 'WARN' ? 'var(--warn)' : 'var(--bad)', flex: 'none' }}>{g.result === 'PASS' ? '일치' : g.result === 'WARN' ? '문장만 다름' : '다름'}</b>
                </div>
              ))}
              {!t2 && <span className="muted">골든 비교가 없는 워크로드입니다 — {d.verdict.summary}</span>}
            </section>
          </div>

          {(c.diffs || []).length > 0 && (
            <section className="card">
              <h2>골든 차이 <span className="btns">{c.diffs.map((n) => <button key={n} className={`btn sm ${diff.name === n ? 'pri' : ''}`} type="button" onClick={() => openDiff(n)}>{n.replace('.diff', '')}</button>)}</span></h2>
              {diff.name ? <pre className="code" style={{ background: 'var(--ground)', color: 'var(--fg)' }}>{diff.text}</pre> : <span className="muted">문항을 누르면 소스 · 타겟 답변을 나란히 봅니다</span>}
            </section>
          )}
        </>
      )}

      <div className="row">
        <section className="grow log" aria-label="실행 로그">
          <div className="lh"><b>실행 로그</b><span style={{ color: running ? '#93C5FD' : '#94A3B8' }}>{running ? '● 실시간' : 'log.txt'}</span></div>
          <div className="lb" ref={logBox}>
            {log.split('\n').map((l, i) => <div key={i} className={logClass(l)}>{l}</div>)}
          </div>
        </section>
        <div className="aside" style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
          <section className="card">
            <h2>타겟</h2>
            <dl className="kv tight">
              <dt>인프라</dt><dd className="mono">{d.target.infra || '—'}</dd>
              <dt>존</dt><dd>{d.target.zone || '—'}{d.target.zones.length > 1 && d.target.zone ? ` (${d.target.zones.indexOf(d.target.zone) + 1}/${d.target.zones.length})` : ''}</dd>
              <dt>사양</dt><dd>{d.recommend.spec ? `${d.recommend.spec.name || d.recommend.spec.id}${d.recommend.spec.gpu ? ' · ' + d.recommend.spec.gpu : ''}` : '—'}</dd>
              {hasTarget && !running && (
                <><dt>지금 상태</dt><dd style={{ color: liveChecked ? (live.status === 'Running' ? 'var(--ok)' : 'var(--bad)') : 'var(--mut)', fontWeight: 600 }}>
                  {!live || live.checking ? '확인 중…' : live.err ? '확인 못 함' : live.status}</dd></>
              )}
              <dt>공인 IP</dt><dd className="mono">{(liveChecked && live.publicIP) || d.target.publicIP || '—'}{liveChecked && !live.publicIP && d.target.publicIP ? <span className="muted"> (기록된 값)</span> : null}</dd>
            </dl>
            {ipChanged && <span className="muted">공인 IP 가 바뀌었습니다 (기록 {d.target.publicIP}) — 지금 IP 로 엽니다.</span>}
            {liveChecked && live.status !== 'Running' && (
              <span className="muted">타겟이 {live.status} 상태라 화면을 열 수 없습니다.{/^Suspend/.test(live.status) ? ' MapUI 에서 Resume 하면 공인 IP 가 바뀔 수 있으니, 켠 뒤 「다시 확인」을 누릅니다.' : ''}</span>
            )}
            {live && live.err && <span className="muted">타겟 상태를 확인하지 못해 기록된 IP 로 엽니다 — {live.err}</span>}
            {hasTarget && !running && <div><button className="btn sm" type="button" disabled={live && live.checking} onClick={loadLive}>다시 확인</button></div>}
            {d.target.tries.length > 0 && <span className="muted">자원 없어 넘긴 존: {d.target.tries.map((t) => t.zone).join(', ')}</span>}
          </section>
          <section className="card">
            <h2>단계 시간</h2>
            {d.stages.map((s) => (
              <div className="tbar" key={s.name}>
                <span className="k">{STAGE_LABEL[s.name]}</span>
                <i><b style={{ width: `${s.seconds ? Math.max(2, Math.round((s.seconds / maxSec) * 100)) : 0}%` }} /></i>
                <span className="v">{s.seconds !== null ? fmtSec(s.seconds) : '—'}</span>
              </div>
            ))}
          </section>
          <section className="card">
            <h2>사람 개입 <small>{d.interventions.length}건</small></h2>
            {d.interventions.length === 0 && <span className="muted">아직 없음</span>}
            {d.interventions.map((n, i) => (
              <div className="line" key={i} style={{ flexDirection: 'column', gap: 2 }}>
                <span className="muted">{fmtTime(n.at)} · {n.stage || '—'}</span>
                <span>{n.text}</span>
              </div>
            ))}
          </section>
        </div>
      </div>

      {evidence && (
        <section className="card flush">
          <div className="ch">
            <b>증적 <span className="muted" style={{ fontWeight: 400 }}>핵심 {evidence.coreCount}개 · 전체 {files.files.length}개 · {fmtBytes(files.total)}</span></b>
            <div className="btns">
              <a className="btn" href={fileHref('summary.md')} target="_blank" rel="noreferrer">요약 보고서 (summary.md)</a>
              <a className="btn pri" href={`/api/runs/${caseName}/${runId}/zip`}>증적 ZIP 받기</a>
            </div>
          </div>
          <div className="evg-grid">
            {evidence.core.map((g) => (
              <div className="evg" key={g.key}>
                <h3>{g.title}</h3>
                {g.files.map((f) => <EvidenceFile key={f.path} f={f} href={fileHref(f.path)} />)}
                {g.files.length === 0 && <span className="muted">{g.empty && judged && !t2 ? g.empty : '이 run 에는 없습니다'}</span>}
              </div>
            ))}
          </div>
          {evidence.debug.length > 0 && (
            <details className="evd">
              <summary>문제가 생겼을 때 보는 파일 · {evidence.debug.reduce((n, g) => n + g.files.length, 0)}개</summary>
              {evidence.debug.map((g) => (
                <div className="evg" key={g.title}>
                  <h3>{g.title} <small className="muted">{g.desc}</small></h3>
                  {g.files.map((f) => <EvidenceFile key={f.path} f={f} href={fileHref(f.path)} />)}
                </div>
              ))}
            </details>
          )}
          {evidence.rest.length > 0 && <div className="muted evr">중간 계산 · 중복 파일 {evidence.rest.length}개는 화면에 두지 않았습니다 — 증적 ZIP 에 들어 있습니다</div>}
        </section>
      )}

      {modal === 'note' && (
        <div className="modal" role="dialog" aria-modal="true" aria-label="사람 개입 기록">
          <div className="card">
            <h2>사람 개입 기록</h2>
            <span className="muted">손으로 고친 일 · 도구 밖에서 한 일을 남깁니다. summary.md 의 「사람이 개입한 지점」에 들어갑니다.</span>
            <label className="f">단계
              <select value={noteStage} onChange={(e) => setNoteStage(e.target.value)}>
                <option value="">(지정 안 함)</option>
                {d.stages.map((s) => <option key={s.name} value={s.name}>{STAGE_LABEL[s.name]} ({s.name})</option>)}
              </select>
            </label>
            <label className="f">한 일<textarea value={noteText} onChange={(e) => setNoteText(e.target.value)} /></label>
            <div className="btns" style={{ justifyContent: 'flex-end' }}>
              <button className="btn" type="button" onClick={() => setModal(null)}>닫기</button>
              <button className="btn pri" type="button" disabled={!noteText.trim() || !!busy} onClick={() => act('note', () => api.note(caseName, runId, { stage: noteStage, text: noteText }).then(() => setNoteText('')))}>기록</button>
            </div>
          </div>
        </div>
      )}

      {modal === 'delete' && (
        <div className="modal" role="dialog" aria-modal="true" aria-label="타겟 삭제">
          <div className="card">
            <h2>{leftover ? '남은 자원 삭제' : '타겟 삭제'}</h2>
            {leftover && <span className="muted">VM 은 이미 {goneStatus} 입니다 — 남은 인프라 기록과 아래 자원을 지웁니다.</span>}
            <span>Beetle 삭제 API로 <b className="mono">{d.target.infra}</b> 와 Beetle이 만든 vNet · 보안 그룹 · SSH 키를 지웁니다. 되돌릴 수 없습니다. 증적은 남습니다.</span>
            <label className="f">확인용으로 인프라 이름을 그대로 입력<input type="text" value={confirmName} onChange={(e) => setConfirmName(e.target.value)} /></label>
            <div className="btns" style={{ justifyContent: 'flex-end' }}>
              <button className="btn" type="button" onClick={() => setModal(null)}>닫기</button>
              <button className="btn danger" type="button" disabled={confirmName !== d.target.infra || !!busy} onClick={() => act('delete', () => api.deleteTarget(caseName, runId, confirmName))}>
                {busy === 'delete' ? <><span className="spin" /> 지우는 중…</> : '삭제'}
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}
