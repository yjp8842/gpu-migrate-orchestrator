import React, { useEffect, useRef, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { api, fmtBytes, fmtSec, fmtTime, STAGE_LABEL } from '../api.js';
import { Csp, StatusPill, routeOf } from '../Layout.jsx';

const STATE_LABEL = { done: '✓ 완료', running: '● 진행 중', failed: '✕ 실패', pending: '대기' };

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
  const waitingCleanup = !running && verify && verify.status === 'done' && cleanup && cleanup.status === 'pending';

  return (
    <>
      <div className="head">
        <div>
          <Link className="back" to="/">← 이관 목록</Link>
          <h1>
            <span className="mono" style={{ fontSize: 18 }}>{runId}</span>
            <StatusPill status={d.status} />
            {waitingCleanup && <span className="pill p-wait">정리 전 — 타겟 확인 대기</span>}
          </h1>
          <p>
            {src && <><Csp name={src} /> → <Csp name={dst} /> · </>}
            <span className="mono">{caseName}</span> · 시작 {fmtTime(d.started)} · {running ? '경과' : '소요'} {fmtSec(d.elapsed)}
          </p>
        </div>
        <div className="btns">
          {d.target.publicIP && !running && <a className="btn" href={`http://${d.target.publicIP}${/lexai/.test(d.workload || '') && /shell|native/.test(d.workload || '') ? ':3000' : ''}/`} target="_blank" rel="noreferrer">타겟 화면 열기</a>}
          <button className="btn" type="button" onClick={() => setModal('note')}>사람 개입 기록</button>
          {running && <button className="btn danger" type="button" disabled={!!busy} onClick={() => act('stop', () => api.stop(caseName, runId))}>중지</button>}
          {waitingCleanup && <button className="btn" type="button" disabled={!!busy} onClick={() => act('cleanup', () => api.cleanup(caseName, runId))}>정리 단계 실행</button>}
          {!running && d.target.infra && <button className="btn danger" type="button" onClick={() => { setConfirmName(''); setModal('delete'); }}>타겟 삭제</button>}
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
              <h2>테스트2 — 골든 응답 완전 일치 {t2 && <span className={`pill ${t2.result === 'PASS' ? 'p-pass' : 'p-fail'}`}>{goldenOk} / {(c.golden || []).length}</span>}</h2>
              {(c.golden || []).map((g) => (
                <div className="line" key={g.q}>
                  <span><b className="mono">{g.q}</b> <span className="muted">{g.note}</span></span>
                  <b style={{ color: g.result === 'PASS' ? 'var(--ok)' : 'var(--bad)', flex: 'none' }}>{g.result === 'PASS' ? '일치' : '다름'}</b>
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
              <dt>공인 IP</dt><dd className="mono">{d.target.publicIP || '—'}</dd>
            </dl>
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

      {files && (
        <section className="card flush">
          <div className="ch">
            <b>증적 <span className="muted" style={{ fontWeight: 400 }}>자동 회수 · {fmtBytes(files.total)} · {files.files.length}개 파일</span></b>
            <div className="btns">
              <a className="btn" href={`/api/runs/${caseName}/${runId}/file?path=summary.md`} target="_blank" rel="noreferrer">요약 보고서 (summary.md)</a>
              <a className="btn pri" href={`/api/runs/${caseName}/${runId}/zip`}>증적 ZIP 받기</a>
            </div>
          </div>
          <div className="files">
            {files.files.slice(0, 60).map((f) => (
              <a key={f.path} href={`/api/runs/${caseName}/${runId}/file?path=${encodeURIComponent(f.path)}`} target="_blank" rel="noreferrer">
                <span className="mono" title={f.path}>{f.path}</span><small>{fmtBytes(f.size)}</small>
              </a>
            ))}
          </div>
          {files.files.length > 60 && <div className="muted" style={{ padding: '10px 18px' }}>나머지 {files.files.length - 60}개는 ZIP에 들어 있습니다</div>}
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
            <h2>타겟 삭제</h2>
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
