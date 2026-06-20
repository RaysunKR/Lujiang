// fs.ts —— 浏览器侧 fs API 客户端封装。所有路径都走 query string。
// 路径区分：
//   - 显示路径：来自 list response.path（绝对路径，POSIX-style）
//   - 请求路径：发给 client 的本地路径，Windows 上是反斜杠（见下）

// list response.path 是 POSIX-style（filepath.ToSlash 结果），但发回 client 时
// 需要原样（client 端 filepath.Abs 兼容）。Windows 下 ToSlash 把 "C:\" 变成 "C:/"，
// filepath.Abs("C:/foo") 仍能正确解析，因此往返一致。

export type FSEntryType = "file" | "dir" | "symlink";

export type FSEntry = {
  name: string;
  type: FSEntryType;
  size: number;
  mod_time: number;
};

export type FSListRes = {
  path: string;
  entries: FSEntry[];
};

export type FSReadRes = {
  encoding: "utf8" | "base64";
  content: string;
  size: number;
};

async function jsonOrThrow(r: Response): Promise<any> {
  if (!r.ok) {
    let msg = `${r.status}`;
    try {
      const j = await r.json();
      if (j && j.error) msg = j.error;
    } catch {}
    throw new Error(msg);
  }
  return r.json();
}

export async function fsList(clientId: string, path: string): Promise<FSListRes> {
  const r = await fetch(
    `/api/fs/${encodeURIComponent(clientId)}/list?path=${encodeURIComponent(path)}`,
    { credentials: "include" },
  );
  return jsonOrThrow(r);
}

export async function fsRead(clientId: string, path: string): Promise<FSReadRes> {
  const r = await fetch(
    `/api/fs/${encodeURIComponent(clientId)}/read?path=${encodeURIComponent(path)}`,
    { credentials: "include" },
  );
  return jsonOrThrow(r);
}

export async function fsWrite(
  clientId: string,
  path: string,
  content: string,
  encoding: "utf8" | "base64" = "utf8",
): Promise<{ size: number }> {
  const r = await fetch(
    `/api/fs/${encodeURIComponent(clientId)}/write`,
    {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path, content, encoding }),
    },
  );
  return jsonOrThrow(r);
}

export async function fsMkdir(clientId: string, path: string): Promise<void> {
  const r = await fetch(
    `/api/fs/${encodeURIComponent(clientId)}/mkdir?path=${encodeURIComponent(path)}`,
    { method: "POST", credentials: "include" },
  );
  await jsonOrThrow(r);
}

export async function fsRemove(
  clientId: string,
  path: string,
  recursive = false,
): Promise<void> {
  const r = await fetch(
    `/api/fs/${encodeURIComponent(clientId)}/remove?path=${encodeURIComponent(path)}&recursive=${recursive}`,
    { method: "POST", credentials: "include" },
  );
  await jsonOrThrow(r);
}
