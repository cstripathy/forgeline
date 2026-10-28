#!/usr/bin/env python3
"""A stand-in for AWS IAM Identity Center's sign-in (OIDC) and portal APIs, for trying the
console's "Sign in with AWS" flow without an AWS account. Development only.

    python3 scripts/dev/fake-sso.py 9922
    FORGELINE_DEV_SSO_BASE=http://127.0.0.1:9922 ./bin/forgeline server ...

The code is approved by opening the link the console shows (it is served here). Everything is
plain HTTP on loopback and keeps no state besides what is approved.
"""
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9922
TOKEN = "fake-sso-access-token-" + "x" * 24
approved = set()
ACCOUNTS = [("111122223333", "demo-prod", "prod@example.com"), ("444455556666", "demo-sandbox", "sandbox@example.com")]
ROLES = {"111122223333": ["ReadOnly", "Admin"], "444455556666": ["SandboxAdmin"]}


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, code, obj, ctype="application/json"):
        body = obj if isinstance(obj, bytes) else json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("content-type", ctype)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def body(self):
        n = int(self.headers.get("content-length") or 0)
        return json.loads(self.rfile.read(n) or b"{}")

    def do_POST(self):
        path = urlparse(self.path).path
        if path == "/oidc/client/register":
            return self.send(200, {"clientId": "fake-client", "clientSecret": "fake-secret"})
        if path == "/oidc/device_authorization":
            b = self.body()
            link = f"http://127.0.0.1:{PORT}/verify?user_code=WXYZ-1234"
            return self.send(200, {"deviceCode": "fake-device", "userCode": "WXYZ-1234", "verificationUri": link,
                                   "verificationUriComplete": link, "expiresIn": 600, "interval": 5, "startUrl": b.get("startUrl")})
        if path == "/oidc/token":
            if "fake-device" not in approved:
                return self.send(400, {"error": "authorization_pending"})
            return self.send(200, {"accessToken": TOKEN, "tokenType": "Bearer", "expiresIn": 28800})
        self.send(404, {"message": "not found"})

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        if u.path == "/verify":
            approved.add("fake-device")
            return self.send(200, b"<!doctype html><title>Fake AWS sign-in</title><h1>Access allowed</h1><p>You can return to Forgeline.</p>", "text/html")
        if self.headers.get("x-amz-sso_bearer_token") != TOKEN:
            return self.send(401, {"message": "Session token not found or invalid"})
        if u.path == "/portal/assignment/accounts":
            return self.send(200, {"accountList": [{"accountId": a, "accountName": n, "emailAddress": e} for a, n, e in ACCOUNTS]})
        if u.path == "/portal/assignment/roles":
            return self.send(200, {"roleList": [{"roleName": r, "accountId": q["account_id"][0]} for r in ROLES.get(q["account_id"][0], [])]})
        if u.path == "/portal/federation/credentials":
            return self.send(200, {"roleCredentials": {"accessKeyId": "ASIAFAKEFAKEFAKE", "secretAccessKey": "fake" * 10,
                                                       "sessionToken": "fake-session-token", "expiration": int((time.time() + 3600) * 1000)}})
        self.send(404, {"message": "not found"})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
