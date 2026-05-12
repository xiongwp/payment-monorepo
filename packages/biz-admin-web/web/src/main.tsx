import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter, Routes, Route, Link } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import ApprovalPage from './pages/ApprovalPage';
import P0PageFull from './pages/P0PageFull';
import AdminPage from './pages/AdminPage';
import './index.css';

const qc = new QueryClient({
  defaultOptions: { queries: { staleTime: 30_000, refetchOnWindowFocus: false } },
});

function App() {
  return (
    <BrowserRouter>
      <nav className="bg-gray-900 text-white px-4 py-2 flex gap-4">
        <span className="font-semibold">biz-admin v2</span>
        <Link className="hover:text-blue-400" to="/">业务监控</Link>
        <Link className="hover:text-blue-400" to="/p0">P0 服务</Link>
        <Link className="hover:text-blue-400" to="/approval">双人复核</Link>
      </nav>
      <main>
        <Routes>
          <Route path="/" element={<AdminPage />} />
          <Route path="/p0" element={<P0PageFull />} />
          <Route path="/approval" element={<ApprovalPage />} />
        </Routes>
      </main>
    </BrowserRouter>
  );
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <QueryClientProvider client={qc}>
      <App />
    </QueryClientProvider>
  </React.StrictMode>,
);
