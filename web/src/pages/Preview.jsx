import React, { useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { api } from '../api.js';
import { Csp, routeOf } from '../Layout.jsx';

// 생성 가능 사전 검사 (Beetle 검증) 코드 → 사람이 읽는 말
const CHECK_TEXT = {
  SPEC_IMAGE_INCOMPATIBLE: '사양과 이미지가 서로 맞지 않습니다',
  SPEC_OR_IMAGE_LOOKUP_FAILED: '사양 또는 이미지를 Tumblebug에서 찾지 못했습니다',
  INVALID_CONNECTION_NAME: '연결 이름이 올바르지 않습니다',
  CONNECTION_MISMATCH: '기존 자원의 CSP · 리전이 맞지 않습니다',
  REQUIRED_FIELD_MISSING: '생성 요청에 필수 값이 비어 있습니다',
  REFERENTIAL_INTEGRITY: '생성 요청 안의 참조가 맞지 않습니다',
  RESOURCE_NOT_AVAILABLE: '쓸 수 없는 자원이 있습니다',
  INSUFFICIENT_SUBNETS: '서브넷이 부족합니다',
  INVALID_ROOT_DISK_SIZE: '루트 디스크 크기가 올바르지 않습니다',
  RESOURCE_ALREADY_EXISTS: '같은 이름이 이미 있습니다 (생성 때 run 이름이 앞에 붙어 달라지므로 문제없음)'
};

// 추천 실패 문장에서 핵심만 — Beetle 이 준 이유는 우리말로 풀고, 끝의 파일 안내는 뗀다
function failReason(msg) {
  const m = (msg || '').replace(/\s*— 후보 원문은.*$/, '');
  if (/No compatible VM spec and OS image pairs/.test(m)) return 'Beetle이 이 소스에 맞는 사양 · 이미지 조합을 찾지 못했습니다 (No compatible VM spec and OS image pairs).';
  return m.replace(/^Beetle 이 추천하지 못했습니다 \([^)]*\):\s*/, 'Beetle: ');
}

// 이관 확인 — 「사양 추천 받기」로 추천된 타겟을 확인하고 Migrate. 추천은 이미 받아 둔 상태(자원은 아직 없음).
export default function Preview() {
  const { caseName, runId } = useParams();
  const nav = useNavigate();
  const [d, setD] = useState(null);
  const [plan, setPlan] = useState(null);
  // 소스의 지금 상태 — Running 이 아니면 Migrate 를 막는다 (서버도 다시 확인한다)
  const [srcState, setSrcState] = useState({ checking: true });
  const [err, setErr] = useState('');
  const [stopBefore, setStopBefore] = useState(true);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.run(caseName, runId).then(setD).catch((e) => setErr(e.message));
    loadCase();
  }, [caseName, runId]);

  function loadCase() {
    setSrcState({ checking: true });
    api.caseFile(caseName).then((c) => {
      setPlan(c.plan);
      setSrcState({ status: c.sourceStatus, err: c.sourceStatusErr });
    }).catch((e) => setSrcState({ err: e.message }));
  }

  async function migrate() {
    setErr('');
    setBusy(true);
    try {
      const r = await api.migrate({ caseName, stopBeforeCleanup: stopBefore });
      nav(`/runs/${r.caseName}/${r.runId}`);
    } catch (e) {
      setErr(e.message);
      setBusy(false);
    }
  }

  const [src, dst] = routeOf(caseName);
  const s = d && d.source;
  const rec = d && d.recommend;
  const spec = rec && rec.spec;
  const failed = d && d.stages.find((x) => x.status === 'failed');
  const srcReady = srcState.status === 'Running';
  const gpuChanged = s && spec && spec.gpu && !(s.gpu || '').startsWith(spec.gpu);
  // 생성 가능 사전 검사 (Beetle 검증 API) — 막히면 recommend 가 실패로 끝나 Migrate 도 막힌다
  const check = rec && rec.validation;
  const blockedByCheck = !!(check && !check.valid);
  const blocking = check ? check.issues.filter((i) => i.blocking) : [];
  const notes = check ? check.issues.filter((i) => !i.blocking) : [];

  return (
    <>
      <div className="head">
        <div>
          <Link className="back" to="/new">← 이전 단계</Link>
          <h1>이관 확인</h1>
          <p>아래 구성으로 타겟을 만들고 데이터를 옮긴 뒤, 같은 서비스가 뜨는지 판정합니다.</p>
        </div>
      </div>
      {err && <div className="err">{err}</div>}
      {!d && !err && <div className="muted"><span className="spin" /> 불러오는 중…</div>}
      {failed && (
        <div className="err" style={{ whiteSpace: 'normal' }}>
          <b>{blockedByCheck ? '생성 가능 사전 검사에서 막혔습니다' : '타겟 추천을 받지 못했습니다'}</b>
          <div style={{ marginTop: 4 }}>
            {blockedByCheck
              ? '추천된 타겟을 이대로 만들 수 없어 Migrate 를 막았습니다. 아래 「생성 가능 사전 검사」에서 이유를 봅니다.'
              : failReason(failed.error)}
          </div>
          <details style={{ marginTop: 6 }}>
            <summary style={{ cursor: 'pointer', width: 'max-content' }}>원문 보기</summary>
            <div className="mono" style={{ marginTop: 4, overflowWrap: 'anywhere' }}>{failed.name}: {failed.error}</div>
          </details>
        </div>
      )}

      {d && (
        <>
          <div className="grid2">
            <section className="card">
              <h2>소스</h2>
              <dl className="kv">
                <dt>위치</dt><dd><Csp name={src} /> {s && s.region}</dd>
                <dt>네임스페이스</dt><dd className="mono">{(plan && plan.ns) || 'default'}</dd>
                <dt>인프라</dt><dd className="mono">{(s && s.infra) || (plan && plan.sourceInfra) || '—'}</dd>
                <dt>상태</dt><dd style={{ color: srcReady ? 'var(--ok)' : srcState.status ? 'var(--bad)' : 'var(--mut)', fontWeight: 600 }}>{srcState.checking ? '확인 중…' : srcState.status || '확인 못 함'}</dd>
                <dt>사양</dt><dd>{s ? `${s.spec || '—'} · vCPU ${s.vcpu} · ${s.memoryGB}GiB` : '—'}</dd>
                <dt>GPU</dt><dd>{(s && s.gpu) || '—'}</dd>
                <dt>OS</dt><dd>{(s && s.os) || '—'}</dd>
              </dl>
            </section>
            <section className="card">
              <h2>타겟 <small>Beetle 추천</small></h2>
              {spec ? (
                <dl className="kv">
                  <dt>위치</dt><dd><Csp name={dst} /> {rec.region || (plan && plan.region)} · 존 {rec.zone || '자동'}</dd>
                  <dt>사양</dt><dd><b>{spec.name || spec.id}</b>{spec.vcpu ? ` · vCPU ${spec.vcpu} · ${spec.memoryGiB}GiB` : ''}</dd>
                  <dt>GPU</dt><dd>{spec.gpu ? `${spec.gpu} · ${spec.vram}GB` : '—'}</dd>
                  <dt>OS</dt><dd>{(rec.image && rec.image.os) || '—'}{rec.rootDisk ? ` · 디스크 ${rec.rootDisk}GB` : ''}</dd>
                  <dt>예상 비용</dt><dd>{spec.cost > 0 ? `시간당 $${Number(spec.cost).toFixed(2)}` : spec.cost < 0 ? '가격 미등록 (Tumblebug 에 가격 정보 없음)' : '—'}</dd>
                </dl>
              ) : <span className="muted">추천 결과가 없습니다</span>}
            </section>
          </div>

          <section className="card">
            <h2>생성 가능 사전 검사 <small>Beetle 검증 · 자원은 만들지 않음</small></h2>
            {!check ? (
              <span className="muted">{spec ? '검사 결과가 없습니다 — 생성 단계에서 Beetle 이 다시 검사합니다.' : '추천된 타겟이 없어 검사하지 않았습니다.'}</span>
            ) : (
              <>
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
                  <span className={`pill ${check.valid ? 'p-pass' : 'p-fail'}`}>{check.valid ? '생성 가능' : '생성 불가'}</span>
                  <span style={{ fontSize: 12.5, color: 'var(--mut)' }}>
                    {check.valid ? '사양 · 이미지 호환, 필수 값, 연결 이름을 모두 통과했습니다' : '이대로 Migrate 하면 생성 단계에서 거부됩니다'}
                  </span>
                </div>
                {blocking.map((i, n) => (
                  <div key={n} style={{ borderLeft: '3px solid var(--bad)', paddingLeft: 10 }}>
                    <div style={{ fontWeight: 600, fontSize: 13, color: 'var(--bad)' }}>{CHECK_TEXT[i.code] || i.code}</div>
                    <div className="mono" style={{ fontSize: 11.5, color: 'var(--mut)', overflowWrap: 'anywhere' }}>{i.code} — {i.message}</div>
                  </div>
                ))}
                {notes.map((i, n) => (
                  <div key={`n${n}`} className="muted">참고 · {CHECK_TEXT[i.code] || i.code} — {i.message}</div>
                ))}
              </>
            )}
          </section>

          <section className="card">
            <dl className="kv">
              <dt>워크로드</dt><dd>{plan ? `${plan.workloadLabel} · 포트 ${plan.port}` : '—'}</dd>
            </dl>
            <label style={{ display: 'flex', gap: 10, cursor: 'pointer', alignItems: 'flex-start' }}>
              <input type="checkbox" checked={stopBefore} onChange={(e) => setStopBefore(e.target.checked)} style={{ accentColor: 'var(--acc)', width: 16, height: 16, margin: '3px 0 0' }} />
              <span><b>판정 뒤 정리 전에 멈춤</b><br /><span className="muted">타겟 화면을 사람이 확인한 뒤 정리합니다</span></span>
            </label>
          </section>

          {gpuChanged && <div className="note"><b>GPU가 바뀝니다</b> ({s.gpu.split(' × ')[0]} → {spec.gpu}) — 데이터 · 실행 파일이 같은지는 그대로 판정하고, 골든 답변 문장은 달라질 수 있습니다.</div>}

          {!srcState.checking && !srcReady && (
            <div className="err">
              {srcState.status
                ? <><b>소스가 {srcState.status} 상태라 Migrate 할 수 없습니다</b> — MapUI에서 소스를 Resume 해 Running 이 된 뒤 「상태 다시 확인」을 누릅니다.</>
                : <><b>소스 상태를 확인하지 못했습니다</b> — {srcState.err}</>}
            </div>
          )}

          <div className="btns" style={{ justifyContent: 'flex-end', alignItems: 'center' }}>
            {!srcState.checking && !srcReady && <button className="btn" type="button" onClick={loadCase}>상태 다시 확인</button>}
            <Link className="btn" to="/new">이전 단계</Link>
            <button className="btn pri" type="button" disabled={busy || !!failed || !spec || !srcReady || blockedByCheck} onClick={migrate}
              title={srcReady ? undefined : '소스가 Running 일 때만 Migrate 할 수 있습니다'}>
              {busy ? <><span className="spin" /> 시작 중…</> : 'Migrate'}
            </button>
          </div>
        </>
      )}
    </>
  );
}
