import React, { useEffect, useRef, useState } from 'react';
import { NavLink, Outlet } from 'react-router-dom';

export default function Layout() {
  return (
    <div className="app">
      <nav className="side" aria-label="주 메뉴">
        <div className="brand">GPU 이관 콘솔</div>
        <div className="sub">Tumblebug · Beetle</div>
        <NavLink to="/" end>전체 이관 목록</NavLink>
        <NavLink to="/new">새 이관</NavLink>
      </nav>
      <main className="main">
        <Outlet />
      </main>
    </div>
  );
}

// Help 는 물음표 표시 — 마우스를 올리거나 키보드로 고르거나 누르면 설명이 뜬다.
// 설명 상자는 화면 기준(fixed)으로 띄워 좁은 화면에서도 잘리지 않게 좌우를 맞추고, 아래가 모자라면 위로 띄운다.
export function Help({ text }) {
  const ref = useRef(null);
  const [pos, setPos] = useState(null);
  const show = () => {
    const r = ref.current.getBoundingClientRect();
    const width = Math.min(320, window.innerWidth - 24);
    const left = Math.min(Math.max(12, r.left - 8), window.innerWidth - width - 12);
    const spaceBelow = window.innerHeight - r.bottom - 16;
    const spaceAbove = r.top - 16;
    setPos(spaceBelow >= spaceAbove
      ? { left, width, top: r.bottom + 8, maxHeight: spaceBelow }
      : { left, width, bottom: window.innerHeight - r.top + 8, maxHeight: spaceAbove });
  };
  const hide = () => setPos(null);
  useEffect(() => {
    if (!pos) return undefined;
    window.addEventListener('scroll', hide, true);
    window.addEventListener('resize', hide);
    return () => {
      window.removeEventListener('scroll', hide, true);
      window.removeEventListener('resize', hide);
    };
  }, [pos]);
  return (
    <>
      <span ref={ref} className="help" tabIndex={0} role="button" aria-label={'도움말: ' + text}
        onMouseEnter={show} onMouseLeave={hide} onFocus={show} onBlur={hide} onClick={show}>
        <svg viewBox="0 0 24 24" width="100%" height="100%" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.8">
          <circle cx="12" cy="12" r="10.2" />
          <path d="M9.2 9.3a2.8 2.8 0 1 1 4.1 2.5c-.8.4-1.3 1-1.3 1.9v.8" strokeLinecap="round" />
          <circle cx="12" cy="17.4" r=".4" fill="currentColor" strokeWidth="1.4" />
        </svg>
      </span>
      {pos && <span className="tip" role="tooltip" style={{ left: pos.left, top: pos.top, bottom: pos.bottom, maxWidth: pos.width, maxHeight: pos.maxHeight }}>{text}</span>}
    </>
  );
}

export function Csp({ name }) {
  const n = (name || '').toLowerCase();
  const cls = ['aws', 'gcp', 'ncp'].includes(n) ? `csp-${n}` : 'csp-other';
  return <span className={`csp ${cls}`}>{n || '?'}</span>;
}

export function StatusPill({ status }) {
  if (!status) return null;
  return <span className={`pill p-${status.key}`}>{status.label}</span>;
}

// 사례 이름에서 경로를 읽는다: aws-to-gcp-widgetrag-a-… → [aws, gcp]
export function routeOf(caseName) {
  const m = /^([a-z]+)-to-([a-z]+)-/.exec(caseName || '');
  return m ? [m[1], m[2]] : [null, null];
}
