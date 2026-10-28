// Forgeline web console: API client.

export class ApiError extends Error {
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body || {};
    this.problems = (body && body.problems) || [];
  }
}

const listeners = new Set();
/** Called with no arguments when the server says the session ended. */
export function onSignedOut(fn) { listeners.add(fn); return () => listeners.delete(fn); }

export async function api(method, path, body) {
  const init = { method, headers: {}, credentials: 'same-origin' };
  if (method !== 'GET' && method !== 'HEAD') init.headers['X-Forgeline-Request'] = '1';
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch(path, init);
  } catch (e) {
    throw new ApiError(0, 'Cannot reach the server. Check that it is running and try again.');
  }
  if (resp.status === 204) return null;
  const text = await resp.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
  if (!resp.ok) {
    if (resp.status === 401 && !path.startsWith('/v1/auth/')) listeners.forEach(fn => fn());
    throw new ApiError(resp.status, (data && data.error) || resp.status + ' ' + resp.statusText, data);
  }
  return data;
}

export const get = (p) => api('GET', p);
export const post = (p, b) => api('POST', p, b === undefined ? {} : b);
export const put = (p, b) => api('PUT', p, b);
export const patch = (p, b) => api('PATCH', p, b);
export const del = (p) => api('DELETE', p);

/** Whether a role meets a minimum. Mirrors the server, which is the real check. */
export function atLeast(role, min) {
  const rank = { viewer: 1, developer: 2, admin: 3 };
  return (rank[role] || 0) >= (rank[min] || 99);
}
