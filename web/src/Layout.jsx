import React from 'react';
import { NavLink, Outlet } from 'react-router-dom';

export default function Layout() {
  return (
    <div className="app">
      <nav className="side" aria-label="주 메뉴">
        <div className="brand">GPU 이관 콘솔</div>
        <div className="sub">Tumblebug · Beetle</div>
        <NavLink to="/" end>이관</NavLink>
        <NavLink to="/new">새 이관</NavLink>
      </nav>
      <main className="main">
        <Outlet />
      </main>
    </div>
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
