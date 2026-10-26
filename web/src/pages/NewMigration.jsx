import React, { useEffect, useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api } from '../api.js';
import { Csp } from '../Layout.jsx';

const PREFERRED_REGION = { aws: 'ap-northeast-2', gcp: 'asia-northeast3', ncp: 'kr' };
const CSPS = ['aws', 'gcp', 'ncp'];
const DEFAULT_POLICY = { gpu: 'same-model-first', image: 'source', root_disk_gb: 100, strategy: 'direct', cleanup: 'suspend' };
const PREFIX = { 'widgetrag-a': 'wr', 'widgetrag-b': 'wrc', 'lexai-a': 'lx', 'lexai-b': 'lxc' };

export default function NewMigration() {
  const nav = useNavigate();
  const [workloads, setWorkloads] = useState([]);
  const [namespaces, setNamespaces] = useState(null);
  const [ns, setNs] = useState('default');
  const [sources, setSources] = useState(null);
  const [regions, setRegions] = useState({});
  const [info, setInfo] = useState(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  const [workload, setWorkload] = useState('widgetrag-a');
  const [source, setSource] = useState('');
  const [csp, setCsp] = useState('gcp');
  const [region, setRegion] = useState('');
  const [zone, setZone] = useState('');
  const [name, setName] = useState('');
  const [nameTouched, setNameTouched] = useState(false);
  const [policy, setPolicy] = useState(DEFAULT_POLICY);
  const [editPolicy, setEditPolicy] = useState(false);

  // 소스 목록은 고른 네임스페이스 기준 — 네임스페이스를 바꾸면 고른 소스도 비운다
  const loadSources = (which = ns) => {
    setSources(null);
    setSource('');
    api.sources(which).then(setSources).catch((e) => {
      setSources([]);
      setErr('소스 목록: ' + e.message);
    });
  };

  useEffect(() => {
    api.workloads().then(setWorkloads).catch((e) => setErr(e.message));
    api.regions().then(setRegions).catch((e) => setErr('리전 목록: ' + e.message));
    api.info().then(setInfo).catch(() => {});
    api.namespaces().then(setNamespaces).catch((e) => {
      setNamespaces([{ id: 'default' }]);
      setErr('네임스페이스 목록: ' + e.message);
    });
  }, []);

  useEffect(() => {
    loadSources(ns);
  }, [ns]);

  const regionList = regions[csp] || [];
  useEffect(() => {
    if (!regionList.length) return;
    const pref = regionList.find((r) => r.region.toLowerCase() === PREFERRED_REGION[csp]);
    setRegion((pref || regionList[0]).region);
    setZone('');
  }, [csp, regions]);

  useEffect(() => {
    if (!nameTouched) setName(`${PREFIX[workload] || 'gmp'}-${csp}`);
  }, [workload, csp, nameTouched]);

  const src = useMemo(() => (sources || []).find((s) => s.id === source), [sources, source]);
  const regionInfo = regionList.find((r) => r.region === region);
  const wl = workloads.find((w) => w.id === workload);
  const ready = src && region && name && wl && wl.ready;

  async function preview() {
    setErr('');
    setBusy(true);
    try {
      const r = await api.preview({
        workload,
        ns,
        sourceInfra: src.id,
        sourceCsp: src.csp,
        csp,
        region: region.toLowerCase(),
        zone: zone || null,
        name,
        policy
      });
      nav(`/confirm/${r.caseName}/${r.runId}`);
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <div className="head">
        <div>
          <Link className="back" to="/">← 이관 목록</Link>
          <h1>새 이관</h1>
          <p>워크로드 · 소스 · 타겟 리전을 고르면, 다음 단계에서 추천된 타겟을 확인하고 이관합니다.</p>
        </div>
      </div>
      {err && <div className="err">{err}</div>}

      <div className="row">
        <div className="grow" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <section className="card">
            <h2>1. 워크로드</h2>
            <div className="grid4 opts">
              {workloads.map((w) => (
                <label key={w.id} className={`opt ${workload === w.id ? 'on' : ''} ${w.ready ? '' : 'off'}`}>
                  <b>
                    <input type="radio" name="workload" checked={workload === w.id} disabled={!w.ready} onChange={() => setWorkload(w.id)} />
                    {w.label}
                  </b>
                  <span>{w.desc} · 포트 {w.port}</span>
                </label>
              ))}
            </div>
          </section>

          <section className="card">
            <h2>
              2. 소스
              <span className="btns" style={{ alignItems: 'center' }}>
                <label className="f inline">
                  네임스페이스
                  <select value={ns} onChange={(e) => setNs(e.target.value)} disabled={!namespaces}>
                    {(namespaces || [{ id: ns }]).map((n) => <option key={n.id} value={n.id}>{n.id}</option>)}
                  </select>
                </label>
                <button className="btn sm" type="button" onClick={() => loadSources()}>Tumblebug에서 다시 읽기</button>
              </span>
            </h2>
            <div className="tbox">
              <table style={{ minWidth: 640 }}>
                <thead>
                  <tr><th><span style={{ position: 'absolute', left: -9999 }}>선택</span></th><th>인프라</th><th>CSP · 리전</th><th>사양 · GPU</th><th>OS</th><th>상태</th></tr>
                </thead>
                <tbody>
                  {sources === null && <tr><td colSpan={6} className="muted"><span className="spin" /> Tumblebug 조회 중…</td></tr>}
                  {sources && sources.length === 0 && <tr><td colSpan={6} className="muted">살아 있는 인프라가 없습니다 — MapUI로 소스를 먼저 만듭니다.</td></tr>}
                  {(sources || []).map((s) => (
                    <tr key={s.id} className={source === s.id ? 'sel' : ''} onClick={() => setSource(s.id)} style={{ cursor: 'pointer' }}>
                      <td><input type="radio" name="source" checked={source === s.id} onChange={() => setSource(s.id)} aria-label={`${s.id} 선택`} /></td>
                      <td className="mono nw">{s.id}{s.madeBy && <span className="muted"> · {s.madeBy}</span>}</td>
                      <td className="nw"><Csp name={s.csp} /> {s.region}</td>
                      <td className="nw">{s.spec}{s.gpu ? ` · ${s.gpu}` : ''}</td>
                      <td className="nw">{s.os || '—'}</td>
                      <td style={{ color: s.status === 'Running' ? 'var(--ok)' : 'var(--mut)', fontWeight: 600 }}>{s.status}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <span className="muted">Suspended 소스도 다음 단계(추천 확인)까지는 되지만, 이관하려면 Running이어야 합니다.</span>
          </section>

          <section className="card">
            <h2>3. 타겟</h2>
            <div className="chips" role="radiogroup" aria-label="클라우드">
              {CSPS.map((c) => (
                <label key={c} className={`chip ${csp === c ? 'on' : ''}`}>
                  <input type="radio" name="csp" checked={csp === c} onChange={() => setCsp(c)} disabled={!regions[c]} />
                  {c.toUpperCase()}
                </label>
              ))}
            </div>
            <div className="grid3">
              <label className="f">리전
                <select value={region} onChange={(e) => { setRegion(e.target.value); setZone(''); }}>
                  {regionList.map((r) => <option key={r.region} value={r.region}>{r.region} ({r.label})</option>)}
                </select>
              </label>
              <label className="f">존
                <select value={zone} onChange={(e) => setZone(e.target.value)}>
                  <option value="">자동 — 기본 존{regionInfo && regionInfo.defaultZone ? ` (${regionInfo.defaultZone})` : ''}부터, 자원 없으면 다음 존</option>
                  {(regionInfo ? regionInfo.zones : []).map((z) => <option key={z} value={z}>{z}</option>)}
                </select>
              </label>
              <label className="f">타겟 이름 (앞부분)
                <input type="text" value={name} onChange={(e) => { setName(e.target.value); setNameTouched(true); }} />
              </label>
            </div>
            <span className="muted">사양은 정하지 않습니다 — 소스 GPU · 메모리를 보고 Beetle이 추천합니다. 이름 뒤에는 run 시각이 붙습니다.</span>
          </section>

          <section className="card">
            <h2>
              4. 정책 <small>{editPolicy ? '바꾸는 중' : '기본값'}</small>
            </h2>
            {editPolicy ? (
              <div className="grid3">
                <label className="f">GPU 고르기
                  <select value={policy.gpu} onChange={(e) => setPolicy({ ...policy, gpu: e.target.value })}>
                    <option value="same-model-first">같은 칩 우선, 없으면 VRAM 이상</option>
                    <option value="vram-at-least">VRAM 이상 중 가장 싼 것</option>
                  </select>
                </label>
                <label className="f">이미지
                  <select value={policy.image} onChange={(e) => setPolicy({ ...policy, image: e.target.value })}>
                    <option value="source">소스와 같은 OS (추천 이미지 그대로)</option>
                    <option value="keep">Beetle 추천 그대로</option>
                    <option value="ubuntu-24.04">Ubuntu 24.04 고정</option>
                    <option value="ubuntu-22.04">Ubuntu 22.04 고정</option>
                  </select>
                </label>
                <label className="f">루트 디스크 (GB)
                  <input type="number" min={30} max={2000} value={policy.root_disk_gb} onChange={(e) => setPolicy({ ...policy, root_disk_gb: Number(e.target.value) })} />
                </label>
                <label className="f">데이터 이관
                  <select value={policy.strategy} onChange={(e) => setPolicy({ ...policy, strategy: e.target.value })}>
                    <option value="direct">소스 → 타겟 직접 (direct)</option>
                    <option value="relay">Beetle 경유 (relay)</option>
                  </select>
                </label>
                <label className="f">정리 단계에서 타겟
                  <select value={policy.cleanup} onChange={(e) => setPolicy({ ...policy, cleanup: e.target.value })}>
                    <option value="suspend">멈춤 (suspend)</option>
                    <option value="terminate">삭제 (terminate)</option>
                    <option value="keep">그대로 (keep)</option>
                  </select>
                </label>
              </div>
            ) : (
              <div className="pol">
                <div className="line"><span className="muted">GPU 고르기</span>같은 칩 우선, 없으면 VRAM 이상</div>
                <div className="line"><span className="muted">이미지</span>소스와 같은 OS (추천 이미지 그대로)</div>
                <div className="line"><span className="muted">루트 디스크</span>{policy.root_disk_gb}GB 이상</div>
                <div className="line"><span className="muted">열 포트</span>{wl ? wl.port : 80} ← {info && info.webAllowCidr ? info.webAllowCidr : '.env GMP_WEB_ALLOW_CIDR'}</div>
                <div className="line"><span className="muted">데이터 이관</span>소스 → 타겟 직접 (direct)</div>
                <div className="line"><span className="muted">정리 단계에서 타겟</span>멈춤 (suspend)</div>
              </div>
            )}
            <div>
              <button className="btn sm" type="button" onClick={() => { if (editPolicy) setPolicy(DEFAULT_POLICY); setEditPolicy(!editPolicy); }}>
                {editPolicy ? '기본값으로 되돌리기' : '정책 바꾸기'}
              </button>
            </div>
          </section>
        </div>

        <aside className="aside card" aria-label="이관 계획">
          <h2>이관 계획</h2>
          <div className="route">
            <span className={`csp-${src ? src.csp : 'other'}`}><b>{src ? src.csp.toUpperCase() : '소스'}</b><em>{src ? src.region : '고르기 전'}</em></span>
            →
            <span className={`csp-${csp}`}><b>{csp.toUpperCase()}</b><em>{region || '—'}</em></span>
          </div>
          <dl className="kv tight">
            <dt>워크로드</dt><dd>{wl ? wl.label : '—'}</dd>
            <dt>네임스페이스</dt><dd className="mono">{ns}</dd>
            <dt>소스</dt><dd className="mono">{src ? src.id : '—'}</dd>
            <dt>소스 GPU</dt><dd>{src ? src.gpu || '—' : '—'}</dd>
            <dt>타겟 생성</dt><dd>Beetle 추천 · 생성</dd>
            <dt>존</dt><dd>{zone || '자동'}</dd>
          </dl>
          {src && src.status !== 'Running' && <div className="note">소스가 {src.status} 입니다. 추천 확인까지는 되지만, 이관 전에 MapUI에서 Resume 하세요.</div>}
          <button className="btn pri" type="button" disabled={!ready || busy} onClick={preview}>
            {busy ? <><span className="spin" /> 타겟 추천 받는 중…</> : '다음 단계 →'}
          </button>
          <span className="hint">아직 자원은 만들지 않습니다</span>
        </aside>
      </div>
    </>
  );
}
