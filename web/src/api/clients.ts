// clients.ts —— /api/clients 的浏览器侧封装，用于客户端列表与 PTY 页面取 shells。

export type ClientMeta = {
  id: string;
  hostname: string;
  os: string;
  arch: string;
  shells?: string[];
};

export async function fetchClients(): Promise<ClientMeta[]> {
  const r = await fetch("/api/clients", { credentials: "include" });
  if (!r.ok) throw new Error(`list clients failed (${r.status})`);
  return (await r.json()) as ClientMeta[];
}

export async function fetchClient(id: string): Promise<ClientMeta | null> {
  const all = await fetchClients();
  return all.find((c) => c.id === id) ?? null;
}
