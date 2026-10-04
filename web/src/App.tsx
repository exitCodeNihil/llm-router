import { lazy, Suspense } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import { Center } from "@astryxdesign/core/Center";

import { ApiError, token } from "./lib/api";
import { useMe } from "./lib/hooks";
import Shell from "./components/Shell";
import { Loading } from "./components/Page";
import Login from "./pages/Login";

// Recharts is heavy; keep it out of the login/critical bundle.
const Dashboard = lazy(() => import("./pages/Dashboard"));
const Observability = lazy(() => import("./pages/Observability"));

import Keys from "./pages/Keys";
import Teams from "./pages/Teams";
import Users from "./pages/Users";
import Providers from "./pages/Providers";
import Models from "./pages/Models";
import ModelsAvailable from "./pages/ModelsAvailable";
import Pricing from "./pages/Pricing";
import EdgeNodes from "./pages/EdgeNodes";
import Settings from "./pages/Settings";
// Both pull in the chart bundle too.
const Analytics = lazy(() => import("./pages/Analytics"));
const Requests = lazy(() => import("./pages/Requests"));
import Account from "./pages/Account";
import Chat from "./pages/chat";
import Workspaces from "./pages/Workspaces";
import WorkspaceSettings from "./pages/WorkspaceSettings";
import WorkspaceIDE from "./pages/workspace";

export default function App() {
  // /auth/logout can clear the cookie but not a bootstrap token held here.
  if (new URLSearchParams(window.location.search).has("signed_out")) {
    token.clear();
    window.history.replaceState(null, "", "/");
  }
  const me = useMe();

  if (me.isLoading) {
    return (
      <Center minHeight="100dvh">
        <Loading label="Connecting" />
      </Center>
    );
  }

  const unauthorized = me.error instanceof ApiError && me.error.status === 401;
  if (unauthorized || !me.data) return <Login onSignedIn={() => me.refetch()} />;

  const isAdmin = me.data.is_admin;

  return (
    <Shell me={me.data}>
      <Suspense fallback={<Loading />}>
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/chat" element={<Chat />} />
          <Route path="/workspaces" element={<Workspaces />} />
          {isAdmin && <Route path="/workspaces/settings" element={<WorkspaceSettings />} />}
          <Route path="/workspaces/:id" element={<WorkspaceIDE />} />
          <Route path="/analytics" element={<Analytics />} />
          <Route path="/requests" element={<Requests />} />
          <Route path="/keys" element={<Keys me={me.data} />} />
          <Route path="/teams" element={<Teams me={me.data} />} />
          <Route path="/account" element={<Account me={me.data} />} />
          {isAdmin && <Route path="/users" element={<Users />} />}
          {isAdmin && <Route path="/providers" element={<Providers />} />}
          <Route path="/models" element={isAdmin ? <Models /> : <ModelsAvailable />} />
          {isAdmin && <Route path="/deployments" element={<Navigate to="/models" replace />} />}
          {isAdmin && <Route path="/pricing" element={<Pricing />} />}
          {isAdmin && <Route path="/edge-nodes" element={<EdgeNodes />} />}
          {isAdmin && <Route path="/observability" element={<Observability />} />}
          {isAdmin && <Route path="/settings" element={<Settings />} />}
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Suspense>
    </Shell>
  );
}
