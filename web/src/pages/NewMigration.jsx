import React, { useEffect, useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api } from '../api.js';
import { Csp, Help } from '../Layout.jsx';

const PREFERRED_REGION = { aws: 'ap-northeast-2', gcp: 'asia-northeast3', ncp: 'kr' };
const CSPS = ['aws', 'gcp', 'ncp'];
// open_ports: null 이면 워크로드 포트 하나를 내 IP 에만 연다 (서버 기본값)
const DEFAULT_POLICY = { gpu: 'same-model-first', image: 'source', root_disk_gb: 100, strategy: 'direct', cleanup: 'suspend', open_ports: null };
// 「내 IP」는 사례 파일에 이 자리표시자로 남고, migrate 가 실행할 때 콘솔 PC .env 의 값으로 바꾼다
const MY_IP = '${GMP_WEB_ALLOW_CIDR}';
const GPU_OPTS = [['same-model-first', '같은 칩 우선, 없으면 VRAM 이상'], ['vram-at-least', 'VRAM 이상 중 가장 싼 것']];
const IMAGE_OPTS = [['source', '소스와 같은 OS (추천 이미지 그대로)'], ['keep', 'Beetle 추천 그대로'], ['ubuntu-24.04', 'Ubuntu 24.04 고정'], ['ubuntu-22.04', 'Ubuntu 22.04 고정']];
const STRATEGY_OPTS = [['direct', 'Beetle (소스 → 타겟 직접 · direct)'], ['relay', 'Beetle (Beetle 서버 경유 · relay)']];
const CLEANUP_OPTS = [['suspend', '멈춤 (suspend)'], ['terminate', '삭제 (terminate)'], ['keep', '그대로 (keep)']];
const POLICY_HELP = [
  '추천 결과를 고르고 고치는 규칙입니다.',
  '· GPU 고르기: 소스와 같은 칩 중 가장 싼 것, 없으면 VRAM이 소스 이상인 것',
  '· 이미지: 소스와 같은 OS · 버전 (추천 이미지가 그 OS면 그대로)',
  '· 루트 디스크: 타겟 디스크 최소 크기 — GPU VM은 100GB 이상 권장',
  '· 추가 열 포트: 타겟 보안 그룹의 기본 규칙에 더해 추가로 여는 인바운드 규칙 — 기본은 워크로드 포트를 내 IP(콘솔 PC .env 의 GMP_WEB_ALLOW_CIDR)에만 엽니다',
  '· 데이터 이관: 늘 Beetle 이 옮깁니다 — direct 는 소스 → 타겟 바로, relay 는 Beetle 서버를 거쳐',
  '· 정리 단계에서 타겟: 판정 뒤 멈춤 · 삭제 · 그대로'
].join('\n');
const PREFIX = { 'widgetrag-a': 'wr', 'widgetrag-b': 'wrc', 'lexai-a': 'lx', 'lexai-b': 'lxc' };

const sameAs = (a, b) => Object.keys(b).every((k) => a[k] === b[k]);
const defaultPorts = (port) => [{ port, src: 'mine', cidr: '' }];
const isDefaultPorts = (rules, port) => rules.length === 1 && rules[0].src === 'mine' && rules[0].port === port;
// 대역 모양만 본다 — 네트워크 주소인지 · 겹치는지는 서버가 다시 검사한다
const cidrOk = (c) => {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d{1,2})$/.exec(c.trim());
  return !!m && m.slice(1, 5).every((o) => Number(o) <= 255) && Number(m[5]) >= 16 && Number(m[5]) <= 32;
};
const ruleOk = (r, myIp) => Number.isInteger(r.port) && r.port >= 1 && r.port <= 65535 && (r.src === 'mine' ? !!myIp : cidrOk(r.cidr));
const toApiPorts = (rules) => rules && rules.map((r) => ({ port: r.port, cidr: r.src === 'mine' ? MY_IP : r.cidr.trim() }));

// 정책 한 줄 — 왼쪽 이름 칸, 오른쪽 값 칸. 바꾸는 중에도 줄 모양은 그대로이고 값 자리만 입력 칸으로 바뀐다.
function PolicyRow({ label, children }) {
  return (
    <div className="prow">
      <span className="pl">{label}</span>
      <div className="pv">{children}</div>
    </div>
  );
}

function Choice({ edit, label, opts, value, shown, onChange }) {
  if (!edit) return <span className="pt">{(opts.find((o) => o[0] === shown) || [shown, shown])[1]}</span>;
  return (
    <select value={value} aria-label={label} onChange={(e) => onChange(e.target.value)}>
      {opts.map(([v, t]) => <option key={v} value={v}>{t}</option>)}
    </select>
  );
}

// PortRules 는 열 포트 규칙 — 규칙 하나가 한 줄(프로토콜 · 포트 · 출발지 종류 · 대역)이고, 바꾸는 중에는 같은 칸이 입력 칸이 된다.
function PortRules({ edit, rules, myIp, onChange }) {
  const mine = myIp || '.env 에 GMP_WEB_ALLOW_CIDR 없음';
  const set = (i, patch) => onChange(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const portOk = (p) => Number.isInteger(p) && p >= 1 && p <= 65535;
  return (
    <div className={`rules ${edit ? 'edit' : ''}`}>
      {rules.length === 0 && <span className="pt muted">추가로 열지 않음</span>}
      {rules.map((r, i) => (
        <div className="rule" key={i}>
          <span className="pt">TCP</span>
          {edit
            ? <input type="number" min={1} max={65535} value={Number.isNaN(r.port) ? '' : r.port} aria-label={`규칙 ${i + 1} 포트`} className={Number.isNaN(r.port) || portOk(r.port) ? '' : 'bad'}
                onChange={(e) => set(i, { port: e.target.value === '' ? NaN : Number(e.target.value) })} />
            : <span className="pt">{r.port}</span>}
          <span className="pt muted" aria-hidden="true">←</span>
          {edit
            ? <select value={r.src} aria-label={`규칙 ${i + 1} 출발지`} onChange={(e) => set(i, { src: e.target.value })}>
                <option value="mine">내 IP</option>
                <option value="custom">직접 입력</option>
              </select>
            : <span className="pt"><em className="tag">{r.src === 'mine' ? '내 IP' : '직접 입력'}</em></span>}
          {edit && r.src === 'custom'
            ? <input type="text" className={r.cidr === '' || cidrOk(r.cidr) ? 'mono' : 'mono bad'} placeholder="예: 203.0.113.10/32" value={r.cidr} aria-label={`규칙 ${i + 1} 출발지 대역`} onChange={(e) => set(i, { cidr: e.target.value })} />
            : <span className={`pt ${edit ? 'mono' : ''} ${r.src === 'mine' && !myIp ? 'bad-t' : ''}`}>{r.src === 'mine' ? mine : r.cidr}</span>}
          {edit && <button className="btn sm" type="button" onClick={() => onChange(rules.filter((_, j) => j !== i))}>삭제</button>}
        </div>
      ))}
      {edit && (
        <div className="radd">
          <button className="btn sm" type="button" disabled={rules.length >= 10} onClick={() => onChange([...rules, { port: NaN, src: 'custom', cidr: '' }])}>+ 규칙 추가</button>
          <span className="muted">내 IP는 콘솔 PC .env 값 · 직접 입력은 /16 보다 좁은 IPv4 대역</span>
        </div>
      )}
    </div>
  );
}

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
  const [draft, setDraft] = useState(DEFAULT_POLICY);

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
  const diskOk = draft.root_disk_gb >= 30 && draft.root_disk_gb <= 2000;
  const port = wl ? wl.port : 80;
  const myIp = info && info.webAllowCidr;
  const shownPorts = policy.open_ports || defaultPorts(port);
  const draftPorts = draft.open_ports || defaultPorts(port);
  const portsOk = draftPorts.every((r) => ruleOk(r, myIp));
  const portMissing = !shownPorts.some((r) => r.port === port);
  const ready = src && region && name && wl && wl.ready && !editPolicy;

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
        policy: { ...policy, open_ports: toApiPorts(policy.open_ports) }
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
          <p>워크로드 · 소스 · 타겟 리전을 고르고 「사양 추천 받기」를 누르면, 추천된 타겟을 확인한 뒤 이관합니다.</p>
        </div>
      </div>
      {err && <div className="err">{err}</div>}

      <div className="row">
        <div className="grow" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <section className="card">
            <h2><span className="ttl">1. 워크로드 <Help text={"옮길 서비스와 설치 형태입니다.\n네이티브 설치는 OS에 직접 설치해 띄우고, Docker Compose는 컨테이너로 띄웁니다.\n형태마다 패키지 · 기동 · 판정 방법이 다릅니다."} /></span></h2>
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
              <span className="ttl">2. 소스 <Help text={"옮길 원본 VM입니다. 고른 네임스페이스의 Tumblebug 인프라 가운데서 고릅니다.\n이관하려면 Running이어야 하고, Suspended는 사양 추천까지만 됩니다."} /></span>
              <span className="btns" style={{ alignItems: 'center' }}>
                <label className="f inline">
                  <span className="lt">네임스페이스 <Help text={"Tumblebug에서 인프라를 나눠 담는 공간입니다. 타겟도 같은 네임스페이스에 만들어집니다."} /></span>
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
            <span className="muted">Suspended 소스도 사양 추천까지는 되지만, 이관하려면 Running이어야 합니다.</span>
          </section>

          <section className="card">
            <h2><span className="ttl">3. 타겟 <Help text={"옮겨 갈 클라우드와 리전입니다.\n사양은 고르지 않습니다 — 소스 GPU · 메모리를 보고 Beetle이 추천합니다.\n존을 비우면 기본 존부터 시도하고, 자원이 없으면 다음 존으로 넘어갑니다."} /></span></h2>
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
              <span className="ttl">4. 정책 <Help text={POLICY_HELP} /></span> <small>{editPolicy ? '바꾸는 중' : sameAs(policy, DEFAULT_POLICY) ? '기본값' : '바꾼 값'}</small>
            </h2>
            <div className="pset">
              <PolicyRow label="GPU 고르기">
                <Choice edit={editPolicy} label="GPU 고르기" opts={GPU_OPTS} value={draft.gpu} shown={policy.gpu} onChange={(v) => setDraft({ ...draft, gpu: v })} />
              </PolicyRow>
              <PolicyRow label="이미지">
                <Choice edit={editPolicy} label="이미지" opts={IMAGE_OPTS} value={draft.image} shown={policy.image} onChange={(v) => setDraft({ ...draft, image: v })} />
              </PolicyRow>
              <PolicyRow label="루트 디스크">
                {editPolicy
                  ? <span className="unit"><input type="number" min={30} max={2000} value={draft.root_disk_gb} aria-label="루트 디스크 (GB)" className={diskOk ? '' : 'bad'} onChange={(e) => setDraft({ ...draft, root_disk_gb: Number(e.target.value) })} />GB 이상</span>
                  : <span className="pt">{policy.root_disk_gb}GB 이상</span>}
              </PolicyRow>
              <PolicyRow label="데이터 이관">
                <Choice edit={editPolicy} label="데이터 이관" opts={STRATEGY_OPTS} value={draft.strategy} shown={policy.strategy} onChange={(v) => setDraft({ ...draft, strategy: v })} />
              </PolicyRow>
              <PolicyRow label="정리 단계에서 타겟">
                <Choice edit={editPolicy} label="정리 단계에서 타겟" opts={CLEANUP_OPTS} value={draft.cleanup} shown={policy.cleanup} onChange={(v) => setDraft({ ...draft, cleanup: v })} />
              </PolicyRow>
              <PolicyRow label="추가 열 포트 (인바운드)">
                <PortRules edit={editPolicy} rules={editPolicy ? draftPorts : shownPorts} myIp={myIp} onChange={(rules) => setDraft({ ...draft, open_ports: rules })} />
              </PolicyRow>
            </div>
            {!editPolicy && portMissing && <div className="note">추가 열 포트에 워크로드 포트 {port} 규칙이 없습니다 — 브라우저로 타겟 화면을 열 수 없습니다.</div>}
            {editPolicy ? (
              <div className="btns">
                <button className="btn sm pri" type="button" disabled={!diskOk || !portsOk}
                  onClick={() => { setPolicy({ ...draft, open_ports: isDefaultPorts(draftPorts, port) ? null : draftPorts }); setEditPolicy(false); }}>확인</button>
                <button className="btn sm" type="button" onClick={() => { setDraft(policy); setEditPolicy(false); }}>취소</button>
                <button className="btn sm" type="button" onClick={() => setDraft(DEFAULT_POLICY)}>기본값으로</button>
                {!diskOk && <span className="muted">루트 디스크는 30~2000GB 사이로 적습니다.</span>}
                {!portsOk && <span className="muted">추가 열 포트 규칙에 빈 칸이나 잘못된 값이 있습니다 (포트 1~65535 · 출발지 대역{myIp ? '' : ' · .env 의 내 IP'}).</span>}
              </div>
            ) : (
              <div>
                <button className="btn sm" type="button" onClick={() => { setDraft(policy); setEditPolicy(true); }}>정책 바꾸기</button>
              </div>
            )}
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
            {busy ? <><span className="spin" /> 사양 추천 받는 중…</> : '사양 추천 받기 →'}
          </button>
          <span className="hint">{editPolicy ? '정책을 바꾸는 중입니다 — 확인 또는 취소를 먼저 누릅니다' : '아직 자원은 만들지 않습니다'}</span>
        </aside>
      </div>
    </>
  );
}
