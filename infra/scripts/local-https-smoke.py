#!/usr/bin/env python3
"""Validate the HTTPS edge routes locally with a disposable, untrusted test cert."""
import json
import os
from pathlib import Path
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class Upstream(BaseHTTPRequestHandler):
    def do_GET(self):
        self.reply()
    def do_POST(self):
        self.reply()
    def do_OPTIONS(self):
        self.reply()
    def reply(self):
        protected = self.path.endswith('/stream-keys') or self.path.endswith('/status')
        self.send_response(401 if protected and self.headers.get('Authorization') != 'Bearer test-grant' else 200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps({'path': self.path, 'method': self.command, 'range': self.headers.get('Range')}).encode())
    def log_message(self, *args):
        pass

server = ThreadingHTTPServer(('127.0.0.1', 18488), Upstream)
threading.Thread(target=server.serve_forever, daemon=True).start()
name = 'live-https-smoke-' + str(os.getpid())
# Only this local test trusts the disposable internal certificate.
context = ssl._create_unverified_context()
def request(path, method='GET', headers=None):
    req = urllib.request.Request('https://localhost:18443' + path, method=method, headers=headers or {})
    try:
        response = urllib.request.urlopen(req, context=context, timeout=2)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.read()

try:
    with tempfile.TemporaryDirectory() as directory:
        config = Path(directory) / 'Caddyfile'
        config.write_text('{\n http_port 18480\n https_port 18443\n}\n' + Path('infra/proxy/Caddyfile.live').read_text())
        subprocess.run(['docker', 'run', '--rm', '-d', '--name', name, '--network', 'host',
                        '-e', 'LIVE_HLS_HOST=localhost', '-e', 'LIVE_HLS_UPSTREAM=127.0.0.1:18488',
                        '-v', str(config) + ':/etc/caddy/Caddyfile:ro', 'caddy:2-alpine'], check=True, stdout=subprocess.DEVNULL)
        for _ in range(50):
            try:
                status, _ = request('/healthz')
                break
            except (OSError, urllib.error.URLError):
                time.sleep(0.1)
        else:
            raise RuntimeError('HTTPS edge did not start')
        path = '/v1/public/sessions/runner_test/720p/video1_stream.m3u8?_HLS_msn=3&_HLS_part=1'
        status, body = request(path, headers={'Range': 'bytes=0-99'})
        assert status == 200 and json.loads(body) == {'path': path, 'method': 'GET', 'range': 'bytes=0-99'}
        assert request('/v1/public/sessions/runner_test/master.m3u8', 'OPTIONS')[0] == 200
        for path, method in [('/v1/sessions/runner_test/stream-keys', 'POST'), ('/v1/public/sessions/runner_test/status', 'GET')]:
            assert request(path, method)[0] == 401, 'edge bypassed upstream grant enforcement'
            assert request(path, method, {'Authorization': 'Bearer test-grant'})[0] == 200
        for path, method in [('/v1/sessions', 'POST'), ('/v1/sessions/runner_test', 'DELETE'), ('/metrics', 'GET')]:
            assert request(path, method)[0] == 404, 'edge exposed a private route'
        print('PASS: HTTPS HLS, Range/query preservation, preflight, runtime grants, private-route exclusion')
finally:
    subprocess.run(['docker', 'stop', '--time', '2', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    server.shutdown()
