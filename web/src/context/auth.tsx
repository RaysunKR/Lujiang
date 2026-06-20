import {
  createContext,
  createSignal,
  onCleanup,
  useContext,
  type JSX,
} from "solid-js";

export type Session = { username: string };

type AuthContext = {
  session: () => Session | null;
  ready: () => boolean;
  check: () => Promise<void>;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
};

const Ctx = createContext<AuthContext>();

export function AuthProvider(props: { children: JSX.Element }) {
  const [session, setSession] = createSignal<Session | null>(null);
  const [ready, setReady] = createSignal(false);

  async function check() {
    try {
      const r = await fetch("/api/me", { credentials: "include" });
      if (r.ok) {
        const j = (await r.json()) as Session;
        setSession(j);
      } else {
        setSession(null);
      }
    } catch {
      setSession(null);
    } finally {
      setReady(true);
    }
  }

  async function login(username: string, password: string) {
    const r = await fetch("/api/login", {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    if (!r.ok) {
      throw new Error(r.status === 401 ? "用户名或密码错误" : `登录失败 (${r.status})`);
    }
    const j = (await r.json()) as Session;
    setSession(j);
  }

  async function logout() {
    await fetch("/api/logout", { method: "POST", credentials: "include" });
    setSession(null);
  }

  const ctx: AuthContext = { session, ready, check, login, logout };
  return <Ctx.Provider value={ctx}>{props.children}</Ctx.Provider>;
}

export function useAuth(): AuthContext {
  const v = useContext(Ctx);
  if (!v) throw new Error("useAuth must be used inside <AuthProvider>");
  return v;
}

// 防止 onCleanup 未使用警告（占位）。
void onCleanup;
