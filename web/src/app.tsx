import { Navigate, Route } from "@solidjs/router";
import { Show, createEffect } from "solid-js";
import { useAuth } from "./context/auth";
import Login from "./pages/login";
import Clients from "./pages/clients";
import ClientPage from "./pages/client";
import ProjectPage from "./pages/project";

export default function App() {
  const auth = useAuth();
  createEffect(() => {
    // 启动时拉一次 /api/me 决定是否已登录。
    auth.check();
  });
  return (
    <>
      <Route path="/login" component={Login} />
      <Route
        path="/clients"
        component={() => (
          <Show when={auth.ready()} fallback={<div class="p-4 text-neutral-400">加载中…</div>}>
            <Show when={auth.session()} fallback={<Navigate href="/login" />}>
              <Clients />
            </Show>
          </Show>
        )}
      />
      <Route
        path="/client/:id"
        component={() => (
          <Show when={auth.ready()} fallback={<div class="p-4 text-neutral-400">加载中…</div>}>
            <Show when={auth.session()} fallback={<Navigate href="/login" />}>
              <ClientPage />
            </Show>
          </Show>
        )}
      />
      <Route
        path="/project/:clientID"
        component={() => (
          <Show when={auth.ready()} fallback={<div class="p-4 text-neutral-400">加载中…</div>}>
            <Show when={auth.session()} fallback={<Navigate href="/login" />}>
              <ProjectPage />
            </Show>
          </Show>
        )}
      />
      <Route
        path="*"
        component={() => <Navigate href={auth.session() ? "/clients" : "/login"} />}
      />
    </>
  );
}
