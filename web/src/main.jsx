import React from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter, Routes, Route } from 'react-router-dom';
import './styles.css';
import Layout from './Layout.jsx';
import Runs from './pages/Runs.jsx';
import NewMigration from './pages/NewMigration.jsx';
import Preview from './pages/Preview.jsx';
import RunPage from './pages/RunPage.jsx';

createRoot(document.getElementById('root')).render(
  <BrowserRouter>
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={<Runs />} />
        <Route path="/new" element={<NewMigration />} />
        <Route path="/confirm/:caseName/:runId" element={<Preview />} />
        <Route path="/runs/:caseName/:runId" element={<RunPage />} />
      </Route>
    </Routes>
  </BrowserRouter>
);
