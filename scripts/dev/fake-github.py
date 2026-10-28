# A stand-in for GitHub, for trying the console's GitHub integration without a real account.
# Usage: python3 fake-github.py PORT STATUS_LOG. See README.md.
#
# Two ways to connect:
#   * a personal token (ghp_demo_token_value_1234), as before;
#   * a GitHub App created from the console (the "manifest flow"): this script serves the page
#     GitHub would show, hands back an app id, a webhook secret and a private key, lists one
#     installation and issues installation tokens. It checks the app token's shape and expiry but
#     does not verify its signature (the Go tests do that against the real public key).
import base64, http.server, io, json, os, subprocess, sys, tarfile, tempfile, time, urllib.parse

TOKEN = "ghp_demo_token_value_1234"
APP_ID, INSTALL_ID, APP_SLUG = 4242, 77, "forgeline-demo"
WEBHOOK_SECRET = "demo-webhook-secret-from-fake-github-0123"
INSTALL_TOKENS = {}
CODES = {}
PIPELINE = b"""version: v1alpha1
name: shop-ci
steps:
  - id: checkout-info
    image: alpine:3.24
    run: |
      echo "commit built from GitHub"
      ls /workspace
  - id: unit
    image: alpine:3.24
    needs: [checkout-info]
    run: echo "shop tests pass" && sleep 1
"""

def make_key():
    d = tempfile.mkdtemp()
    p = os.path.join(d, "k.pem")
    subprocess.run(["openssl", "genrsa", "-traditional", "-out", p, "2048"], check=True, capture_output=True)
    return open(p).read()

PEM = make_key()

def tarball(sha):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        top = "acme-shop-%s/" % sha[:7]
        d = tarfile.TarInfo(top); d.type = tarfile.DIRTYPE; d.mode = 0o755; tf.addfile(d)
        for name, data in (("forgeline.yaml", PIPELINE), ("README.md", b"# acme/shop\n")):
            ti = tarfile.TarInfo(top + name); ti.size = len(data); ti.mode = 0o644
            tf.addfile(ti, io.BytesIO(data))
    return buf.getvalue()

def jwt_ok(auth):
    try:
        h, c, s = auth.split(" ", 1)[1].split(".")
        pad = lambda x: x + "=" * (-len(x) % 4)
        claims = json.loads(base64.urlsafe_b64decode(pad(c)))
        return claims["iss"] == str(APP_ID) and claims["exp"] > time.time() and claims["iat"] <= time.time() and bool(s)
    except Exception:
        return False

class Handler(http.server.BaseHTTPRequestHandler):
    def _send(self, code, body=b"", ctype="application/json", extra=None):
        if not isinstance(body, bytes): body = json.dumps(body).encode()
        self.send_response(code); self.send_header("Content-Type", ctype); self.send_header("Content-Length", str(len(body)))
        for k, v in (extra or {}).items(): self.send_header(k, v)
        self.end_headers(); self.wfile.write(body)

    def _auth(self):
        a = self.headers.get("Authorization", "")
        if a == "Bearer " + TOKEN or a.removeprefix("Bearer ") in INSTALL_TOKENS: return True
        self._send(401, {"message": "Bad credentials"}); return False

    def do_GET(self):
        u = urllib.parse.urlparse(self.path); parts = u.path.strip("/").split("/")
        a = self.headers.get("Authorization", "")
        if u.path == "/app/installations":
            return self._send(200, [{"id": INSTALL_ID, "account": {"login": "acme"}, "repository_selection": "selected"}]) if jwt_ok(a) else self._send(401, {"message": "bad app token"})
        if u.path == "/app/installations/%d" % INSTALL_ID:
            return self._send(200, {"id": INSTALL_ID}) if jwt_ok(a) else self._send(401, {"message": "bad app token"})
        if u.path.startswith("/app/installations/"):
            return self._send(404, {"message": "Not Found"})
        if not self._auth(): return
        if u.path == "/installation/repositories":
            return self._send(200, {"repositories": [{"full_name": "acme/shop", "default_branch": "main", "private": True}]})
        if len(parts) == 3 and parts[0] == "repos":
            if parts[1] + "/" + parts[2] == "acme/shop": self._send(200, {"full_name": "acme/shop"})
            else: self._send(404, {"message": "Not Found"})
        elif len(parts) == 5 and parts[3] == "tarball":
            self._send(200, tarball(parts[4]), "application/gzip")
        else:
            self._send(404, {"message": "Not Found"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0)); raw = self.rfile.read(n) or b""
        u = urllib.parse.urlparse(self.path); parts = u.path.strip("/").split("/")
        # What GitHub shows when you POST a manifest: an approval page. Choosing Create sends you
        # back to the manifest's redirect_url with a one-time code and your state.
        if u.path in ("/settings/apps/new",) or (len(parts) == 5 and parts[0] == "organizations" and parts[3:] == ["apps", "new"]):
            form = urllib.parse.parse_qs(raw.decode()); manifest = json.loads(form["manifest"][0]); state = urllib.parse.parse_qs(u.query).get("state", [""])[0]
            code = "code-" + base64.urlsafe_b64encode(os.urandom(9)).decode(); CODES[code] = manifest
            back = manifest["redirect_url"] + "?" + urllib.parse.urlencode({"code": code, "state": state})
            page = ("<!doctype html><title>Fake GitHub: register app</title><h1>Create GitHub App &ldquo;%s&rdquo;?</h1>"
                    "<p>Permissions: %s</p><p>Webhook active: %s</p><p><a id='create' href='%s'>Create GitHub App</a></p>") % (
                    manifest["name"], json.dumps(manifest.get("default_permissions")), manifest.get("hook_attributes", {}).get("active"), back.replace("'", "%27"))
            return self._send(200, page.encode(), "text/html")
        if len(parts) == 3 and parts[0] == "app-manifests" and parts[2] == "conversions":
            m = CODES.pop(parts[1], None)
            if not m: return self._send(404, {"message": "Not Found"})
            return self._send(201, {"id": APP_ID, "slug": APP_SLUG, "name": m["name"], "html_url": "http://127.0.0.1:%d/apps/%s" % (PORT, APP_SLUG),
                                    "webhook_secret": WEBHOOK_SECRET, "pem": PEM, "client_secret": "not-kept"})
        if u.path == "/app/installations/%d/access_tokens" % INSTALL_ID:
            if not jwt_ok(self.headers.get("Authorization", "")): return self._send(401, {"message": "bad app token"})
            tok = "ghs_" + base64.urlsafe_b64encode(os.urandom(18)).decode(); INSTALL_TOKENS[tok] = 1
            return self._send(201, {"token": tok, "expires_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 3600))})
        if not self._auth(): return
        body = json.loads(raw or b"{}")
        with open(sys.argv[2], "a") as f:
            f.write(json.dumps({"repo": parts[1] + "/" + parts[2], "sha": parts[4][:7], "state": body.get("state"),
                                "description": body.get("description"), "target_url": body.get("target_url"),
                                "auth": "installation-token" if self.headers.get("Authorization", "").removeprefix("Bearer ") in INSTALL_TOKENS else "personal-token"}) + "\n")
        self._send(201, {})

    def log_message(self, *a): pass

PORT = int(sys.argv[1])
http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
