# A stand-in for the GitHub API, for trying the console's GitHub integration without a real
# token. Usage: python3 fake-github.py PORT STATUS_LOG. See README.md.
import http.server, io, json, sys, tarfile

TOKEN = "ghp_demo_token_value_1234"
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

def tarball(sha):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        top = "acme-shop-%s/" % sha[:7]
        d = tarfile.TarInfo(top); d.type = tarfile.DIRTYPE; d.mode = 0o755; tf.addfile(d)
        for name, data in (("forgeline.yaml", PIPELINE), ("README.md", b"# acme/shop\n")):
            ti = tarfile.TarInfo(top + name); ti.size = len(data); ti.mode = 0o644
            tf.addfile(ti, io.BytesIO(data))
    return buf.getvalue()

class Handler(http.server.BaseHTTPRequestHandler):
    def _auth(self):
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            self.send_response(401); self.end_headers(); self.wfile.write(b'{"message":"Bad credentials"}')
            return False
        return True

    def do_GET(self):
        if not self._auth(): return
        parts = self.path.strip("/").split("/")
        if len(parts) == 3 and parts[0] == "repos":
            if parts[1] + "/" + parts[2] == "acme/shop":
                self.send_response(200); self.end_headers(); self.wfile.write(b'{"full_name":"acme/shop"}')
            else:
                self.send_response(404); self.end_headers(); self.wfile.write(b'{"message":"Not Found"}')
        elif len(parts) == 5 and parts[3] == "tarball":
            self.send_response(200); self.end_headers(); self.wfile.write(tarball(parts[4]))
        else:
            self.send_response(404); self.end_headers()

    def do_POST(self):
        if not self._auth(): return
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        parts = self.path.strip("/").split("/")
        with open(sys.argv[2], "a") as f:
            f.write(json.dumps({"repo": parts[1] + "/" + parts[2], "sha": parts[4][:7], "state": body.get("state"),
                                "description": body.get("description"), "target_url": body.get("target_url")}) + "\n")
        self.send_response(201); self.end_headers(); self.wfile.write(b"{}")

    def log_message(self, *a): pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
