import React, { useEffect, useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api, fmtSec, fmtTime, STAGE_LABEL } from '../api.js';
import { Csp, StatusPill, routeOf } from '../Layout.jsx';

const BAR = { running: 'var(--acc)', pass: 'var(--ok)', fail: 'var(--ok)', failed: 'var(--bad)', stopped: '#9AA3AD', partial: '#9AA3AD' };

export default function Runs() {
  const [runs, setRuns] = useState(null);
  const [err, setErr] = useState('');
  const [filter, setFilter] = useState('all');
  const nav = useNavigate();

  useEffect(() => {
    let alive = true;
    const load = () => api.runs().then((r) => alive && setRuns(r)).catch((e) => alive && setErr(e.message));
    load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  const shown = useMemo(() => (runs || []).filter((r) => filter === 'all' || r.status.key === filter), [runs, filter]);
  const count = (k) => (runs || []).filter((r) => r.status.key === k).length;

  return (
    <>
      <div className="head">
        <div>
          <h1>이관</h1>
          <p>소스 VM을 다른 클라우드로 옮기고 같은 서비스가 뜨는지 판정합니다.</p>
        </div>
        <Link className="btn pri" to="/new">+ 새 이관</Link>
      </div>

      <div className="grid4">
        <div className="stat"><span>진행 중</span><b>{count('running')}</b></div>
        <div className="stat"><span>전체 run</span><b>{runs ? runs.length : '…'}</b></div>
        <div className="stat"><span>PASS</span><b>{count('pass')}</b></div>
        <div className="stat"><span>사람 개입</span><b>{(runs || []).reduce((a, r) => a + r.interventions, 0)}건</b></div>
      </div>

      {err && <div className="err">{err}</div>}

      <div className="card flush">
        <div className="ch">
          <b>이관 run</b>
          <label className="f" style={{ flexDirection: 'row', alignItems: 'center' }}>
            상태
            <select value={filter} onChange={(e) => setFilter(e.target.value)}>
              <option value="all">전체</option>
              <option value="running">진행 중</option>
              <option value="pass">PASS</option>
              <option value="fail">FAIL</option>
              <option value="failed">중단</option>
            </select>
          </label>
        </div>
        <div className="tbox">
          <table style={{ minWidth: 860 }}>
            <thead>
              <tr><th>run</th><th>사례</th><th>경로</th><th>타겟</th><th>진행</th><th>상태</th><th>시작</th><th>소요</th></tr>
            </thead>
            <tbody>
              {runs === null && <tr><td colSpan={8} className="muted">불러오는 중…</td></tr>}
              {runs && shown.length === 0 && <tr><td colSpan={8} className="muted">run 이 없습니다 — 「새 이관」으로 시작합니다.</td></tr>}
              {shown.map((r) => {
                const [src, dst] = routeOf(r.caseName);
                return (
                  <tr key={r.caseName + r.runId} className="click" onClick={() => nav(`/runs/${r.caseName}/${r.runId}`)}>
                    <td className="nw"><Link className="mono" to={`/runs/${r.caseName}/${r.runId}`}>{r.runId}</Link></td>
                    <td className="mono nw" style={{ fontSize: 11.5 }}>{r.caseName}</td>
                    <td className="nw">{src ? <><Csp name={src} /> → <Csp name={dst} /></> : '—'}</td>
                    <td className="muted nw">{r.targetInfra || '—'}{r.zone && <><br />{r.zone}</>}</td>
                    <td>
                      <span className="prog">
                        <i><b style={{ width: `${r.progress}%`, background: BAR[r.status.key] || 'var(--acc)' }} /></i>
                        {STAGE_LABEL[r.stage] || r.stage}
                      </span>
                    </td>
                    <td><StatusPill status={r.status} /></td>
                    <td className="muted nw">{fmtTime(r.started)}</td>
                    <td className="muted nw">{fmtSec(r.elapsed)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>
    </>
  );
}
