import { Show, createEffect, createSignal } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { useAuth } from "../context/auth";

export default function Login() {
  const auth = useAuth();
  const nav = useNavigate();
  const [username, setUsername] = createSignal("");
  const [password, setPassword] = createSignal("");
  const [error, setError] = createSignal<string | null>(null);
  const [busy, setBusy] = createSignal(false);

  createEffect(() => {
    if (auth.session()) nav("/clients", { replace: true });
  });

  async function onSubmit(e: Event) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await auth.login(username().trim(), password());
      nav("/clients", { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div class="min-h-screen flex items-center justify-center px-4">
      <form
        onSubmit={onSubmit}
        class="w-full max-w-[400px] rounded-xl border border-[var(--color-border)] bg-[var(--color-panel)] p-6 sm:p-8"
      >
        <h1 class="text-2xl font-semibold mb-6">Lujiang 鹭江</h1>
        <label class="block mb-3">
          <span class="text-sm text-neutral-300">用户名</span>
          <input
            class="mt-1 w-full rounded-md bg-black/30 border border-[var(--color-border)] px-3 py-2 outline-none focus:border-[var(--color-accent)]"
            autocomplete="username"
            value={username()}
            onInput={(e) => setUsername(e.currentTarget.value)}
            required
          />
        </label>
        <label class="block mb-5">
          <span class="text-sm text-neutral-300">密码</span>
          <input
            type="password"
            class="mt-1 w-full rounded-md bg-black/30 border border-[var(--color-border)] px-3 py-2 outline-none focus:border-[var(--color-accent)]"
            autocomplete="current-password"
            value={password()}
            onInput={(e) => setPassword(e.currentTarget.value)}
            required
          />
        </label>
        <button
          type="submit"
          disabled={busy()}
          class="w-full rounded-md bg-[var(--color-accent)] text-black font-medium py-2 disabled:opacity-60"
        >
          {busy() ? "登录中…" : "登录"}
        </button>
        <Show when={error()}>
          <p class="mt-4 text-sm text-red-400">{error()}</p>
        </Show>
      </form>
    </div>
  );
}
