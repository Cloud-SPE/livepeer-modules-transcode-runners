#!/usr/bin/env python3
"""Exercise real local RTMP/HLS through a Docker runner; never calls LOC."""
import base64, json, os, re, secrets, subprocess, threading, time, urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urljoin
image = os.environ.get('LIVE_SMOKE_IMAGE', 'localbuild/standalone-live-validation:v2-local')
name = 'standalone-live-media-smoke-' + str(os.getpid())
base = 'http://127.0.0.1:18088'
events = []

class Callback(BaseHTTPRequestHandler):

    def do_POST(self):
        events.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
        self.send_response(204)
        self.end_headers()

    def log_message(self, *args):
        pass
server = ThreadingHTTPServer(('127.0.0.1', 18089), Callback)
threading.Thread(target=server.serve_forever, daemon=True).start()
token = secrets.token_hex(32)
env = {'LIVE_RUNNER_ADDR': '127.0.0.1:18088', 'LIVE_RUNNER_METRICS_ADDR': '127.0.0.1:19091', 'LIVE_RUNNER_MASTER_KEY': base64.b64encode(secrets.token_bytes(32)).decode(), 'LIVE_RUNNER_BROKER_TOKEN': token, 'LIVE_RUNNER_INTERNAL_MEDIA_TOKEN': secrets.token_hex(32), 'LIVEPEER_PUBLIC_URL': base, 'LIVEPEER_PUBLIC_RTMP_URL': 'rtmp://127.0.0.1:11935', 'LIVE_RUNNER_MEDIAMTX_RTMP_ADDR': '127.0.0.1:11935', 'LIVE_RUNNER_MEDIAMTX_HLS_ADDR': '127.0.0.1:18888', 'LIVE_RUNNER_MEDIAMTX_API_ADDR': '127.0.0.1:19997', 'LIVE_RUNNER_MEDIAMTX_METRICS_ADDR': '127.0.0.1:19998', 'LIVE_RUNNER_ROUTER_RTMP_BASE': 'rtmp://127.0.0.1:11935', 'LIVE_RUNNER_MEDIAMTX_AUTH_URL': base + '/internal/mediamtx/auth', 'LIVE_RUNNER_HARDWARE': 'cpu', 'LIVE_RUNNER_INITIAL_PUBLISH_TIMEOUT': '15s', 'LIVE_RUNNER_RECONNECT_GRACE': '10s'}

def request(url, method='GET', data=None, bearer=None):
    headers = {'Content-Type': 'application/json', 'Origin': 'https://portal.example'}
    if bearer:
        headers['Authorization'] = 'Bearer ' + bearer
    req = urllib.request.Request(url, data=None if data is None else json.dumps(data).encode(), method=method, headers=headers)
    with urllib.request.urlopen(req, timeout=10) as r:
        if '/v1/public/sessions/' in url and not url.endswith('/status'):
            assert r.headers.get('Access-Control-Allow-Origin') == '*', 'missing HLS CORS'
        return r.read()

def js(url, method='GET', data=None, bearer=None):
    return json.loads(request(url, method, data, bearer))
publisher = None
try:
    cmd = ['docker', 'run', '-d', '--rm', '--name', name, '--network', 'host']
    for k, v in env.items():
        cmd += ['-e', k + '=' + v]
    subprocess.run(cmd + [image], check=True, stdout=subprocess.DEVNULL)
    for _ in range(60):
        try:
            request(base + '/ready')
            break
        except Exception:
            time.sleep(0.5)
    else:
        raise Exception('runner not ready')
    body = json.load(open('live-runner/testdata/contracts/v1/create-request.json'))
    body['session_id'] = 'smoke-' + str(os.getpid())
    body['callback_url'] = 'http://127.0.0.1:18089/events'
    body['callback_token'] = secrets.token_hex(32)
    created = js(base + '/v1/sessions', 'POST', body, token)
    runtime = created['runtime']
    coords = runtime['public']
    grant = runtime['grants'][0]['secret']
    rid = created['runner_session_id']
    assert js(base + '/v1/sessions', 'POST', body, token) == created, 'create replay changed descriptor'
    key = js(coords['key_issue_url'], 'POST', {'request_id': 'smoke-first', 'audience': 'gateway-relay'}, grant)
    assert js(coords['key_issue_url'], 'POST', {'request_id': 'smoke-first', 'audience': 'gateway-relay'}, grant) == key
    publishurl = coords['rtmp_url'] + '/' + key['stream_key']

    def publish(url, duration):
        return subprocess.Popen(['docker', 'run', '--rm', '--network', 'host', '--entrypoint', 'ffmpeg', image, '-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=size=640x360:rate=30', '-f', 'lavfi', '-i', 'sine=frequency=1000:sample_rate=48000', '-t', str(duration), '-c:v', 'libx264', '-preset', 'ultrafast', '-tune', 'zerolatency', '-threads', '2', '-g', '30', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-f', 'flv', url], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    publisher = publish(publishurl, 40)
    playable = False
    for _ in range(80):
        time.sleep(0.5)
        try:
            master = request(coords['hls_url']).decode()
            variants = [l for l in master.splitlines() if l and (not l.startswith('#'))]
            if not variants:
                continue
            if len(variants) < 2:
                continue
            for variant in variants:
                varianturl = urljoin(coords['hls_url'], variant)
                playlist = request(varianturl).decode()
                media = [l for l in playlist.splitlines() if l and not l.startswith('#')]
                media += re.findall(r'URI="([^"\n]+\.m3u8)"', playlist)
                assert media, 'no media playlists'
                for uri in media:
                    mediaurl = urljoin(varianturl, uri)
                    media_playlist = request(mediaurl).decode()
                    segments = [l for l in media_playlist.splitlines() if l and not l.startswith('#')]
                    if not segments:
                        raise TimeoutError('waiting for finalized segments')
                    segment = request(urljoin(mediaurl, segments[-1]))
                    assert len(segment) > 0
            playable = True
            print('HLS master + rendition + finalized media segment fetched:', len(segment), 'bytes')
            break
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError):
            pass
    if not playable:
        status = js(base + '/v1/sessions/' + rid, bearer=token)
        raise Exception('no playable HLS; safe status=' + json.dumps(status))
    status = js(base + '/v1/sessions/' + rid, bearer=token)
    assert status['usage']['unit'] == 'output_seconds'
    for _ in range(20):
        if status['usage']['total'] > 0:
            break
        time.sleep(0.5)
        status = js(base + '/v1/sessions/' + rid, bearer=token)
    assert status['usage']['total'] > 0, 'no measured usage'
    before = status['usage']['total']
    rotated = js(coords['key_issue_url'], 'POST', {'request_id': 'smoke-rotate', 'audience': 'gateway-relay'}, grant)
    assert rotated['stream_key'] != key['stream_key'], 'rotation reused key'
    publisher.communicate(timeout=10)
    old = publish(publishurl, 2)
    _, err = old.communicate(timeout=10)
    assert old.returncode != 0, 'old stream key still accepted'
    publisher = publish(coords['rtmp_url'] + '/' + rotated['stream_key'], 15)
    for _ in range(45):
        time.sleep(0.5)
        status = js(base + '/v1/sessions/' + rid, bearer=token)
        if status['usage']['total'] > before:
            break
    assert status['usage']['total'] > before, 'rotated publisher did not resume measured media'
    ended = js(base + '/v1/sessions/' + rid, 'DELETE', {'reason': 'gateway_close'}, token)
    assert js(base + '/v1/sessions/' + rid, 'DELETE', {'reason': 'gateway_close'}, token) == ended
    try:
        request(coords['hls_url'])
        raise Exception('HLS still served after close')
    except urllib.error.HTTPError as e:
        assert e.code >= 400
    publisher.communicate(timeout=10)
    # A fresh session that never publishes must end with zero usage.
    body['session_id'] += '-never'
    never = js(base + '/v1/sessions', 'POST', body, token)
    def wait_ended(session_id, timeout=25):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            status = js(base + '/v1/sessions/' + session_id, bearer=token)
            if status['state'] == 'ended':
                assert status['close_reason'] == 'publisher_disconnect'
                return status
            time.sleep(0.25)
        raise Exception('automatic termination did not arrive')
    assert wait_ended(never['runner_session_id'])['usage']['total'] == 0
    body['session_id'] += '-disconnect'
    disconnected = js(base + '/v1/sessions', 'POST', body, token)
    public = disconnected['runtime']['public']
    key = js(public['key_issue_url'], 'POST', {'request_id':'smoke-idle', 'audience':'gateway-relay'}, disconnected['runtime']['grants'][0]['secret'])
    publisher = publish(public['rtmp_url'] + '/' + key['stream_key'], 20)
    publisher.communicate(timeout=30)
    final = wait_ended(disconnected['runner_session_id'])
    assert final['usage']['total'] > 0, 'disconnect did not preserve measured usage'
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline and not any(e.get('event_type') == 'session.ended' and e.get('close_reason') == 'publisher_disconnect' for e in events):
        time.sleep(0.1)
    assert any(e.get('event_type') == 'session.ended' and e.get('close_reason') == 'publisher_disconnect' for e in events), 'no automatic end callback'
    print('PASS: never-published and disconnected auto termination, final measured usage, terminal callbacks')
    assert events, 'no callback events'
    print('PASS: replay, RTMP publish, advancing HLS, measured output_seconds, key rotation/revocation, resumed output, idempotent terminate; callbacks:', len(events))
finally:
    if publisher and publisher.poll() is None:
        publisher.terminate()
    subprocess.run(['docker', 'stop', '--time', '10', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    server.shutdown()
