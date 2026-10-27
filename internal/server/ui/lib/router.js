// Forgeline web console: a small hash router.
//
// Routes look like '/runs/:id'. A page function receives {params, query} and returns an element,
// optionally with a `cleanup` property that is called when the person navigates away. That is
// where timers and log streams are stopped.

const routes = [];
let current = null;
let host = null;
let renderToken = 0;

export function route(pattern, page) {
  const keys = [];
  const re = new RegExp('^' + pattern.replace(/:([a-z]+)/g, (_, k) => { keys.push(k); return '([^/]+)'; }) + '/?$');
  routes.push({ re, keys, page });
}

export function navigate(path) { location.hash = '#' + path; }

export function currentPath() { return location.hash.slice(1) || '/'; }

export function start(outlet, notFound) {
  host = outlet;
  const run = () => show(notFound);
  addEventListener('hashchange', run);
  run();
}

export function reload() { return show(); }

/** Runs the current page's cleanup (timers, streams) and forgets it. Call before leaving the app shell. */
export function stop() {
  renderToken++; // a page still loading must not appear afterwards
  if (current && current.cleanup) { try { current.cleanup(); } catch (e) { console.error(e); } }
  current = null;
}

async function show(notFound) {
  const token = ++renderToken;
  if (current && current.cleanup) { try { current.cleanup(); } catch (e) { console.error(e); } }
  current = null;
  const [path, qs] = currentPath().split('?');
  const query = Object.fromEntries(new URLSearchParams(qs || ''));
  for (const r of routes) {
    const m = r.re.exec(path);
    if (!m) continue;
    const params = {};
    r.keys.forEach((k, i) => { params[k] = decodeURIComponent(m[i + 1]); });
    let el;
    try { el = await r.page({ params, query }); } catch (e) { console.error(e); el = notFound ? notFound(e) : document.createTextNode(String(e)); }
    if (token !== renderToken) { if (el && el.cleanup) el.cleanup(); return; } // navigated away while loading
    current = el;
    host.replaceChildren(el);
    window.scrollTo(0, 0);
    host.focus({ preventScroll: true });
    return;
  }
  host.replaceChildren(notFound ? notFound() : document.createTextNode('Not found'));
}
