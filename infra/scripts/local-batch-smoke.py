#!/usr/bin/env python3
"""Exercise terminal SSE with real FFmpeg and local HTTP artifact storage."""
import json, os, pathlib, subprocess, tempfile, threading, time, urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
work = tempfile.TemporaryDirectory(prefix='.standalone-batch-smoke-', dir=os.getcwd())
root = pathlib.Path(work.name)
image = os.environ.get('VOD_SMOKE_IMAGE', 'localbuild/standalone-vod-validation:v2-local')
uploads = {}
input_data = b''

class Storage(BaseHTTPRequestHandler):

    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Length', str(len(input_data)))
        self.end_headers()
        self.wfile.write(input_data)

    def do_PUT(self):
        uploads.setdefault(self.path, []).append(self.rfile.read(int(self.headers['Content-Length'])))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass
server = ThreadingHTTPServer(('127.0.0.1', 18079), Storage)
threading.Thread(target=server.serve_forever, daemon=True).start()
subprocess.run(['docker', 'run', '--rm', '--user', '0:0', '-v', str(root) + ':/out', '--entrypoint', 'ffmpeg', image, '-hide_banner', '-loglevel', 'error', '-f', 'lavfi', '-i', 'testsrc2=size=64x64:rate=30', '-f', 'lavfi', '-i', 'sine=frequency=1000:sample_rate=48000', '-t', '2', '-c:v', 'libx264', '-threads', '2', '-c:a', 'aac', '-f', 'mp4', '/out/input.mp4'], check=True)
input_data = (root / 'input.mp4').read_bytes()
names = []

def dest(n):
    return {'artifact_uri': 'smoke/' + n, 'upload_url': 'http://127.0.0.1:18079/' + n}

def invoke(base, path, body):
    requestfile = root / 'request.json'
    requestfile.write_text(json.dumps(body))
    head = root / 'response.headers'
    output = root / 'response.sse'
    subprocess.run(['curl', '--silent', '--show-error', '--max-time', '60', '-D', str(head), '-o', str(output), '-H', 'Content-Type: application/json', '-H', 'Accept: text/event-stream', '--data-binary', '@' + str(requestfile), base + path], check=True)
    headers = head.read_text()
    events = []
    for block in output.read_text().split('\n\n'):
        if 'event: result' in block or 'event: error' in block:
            events.append(json.loads(next((l[6:] for l in block.splitlines() if l.startswith('data: ')))))
    assert len(events) == 1, 'missing or duplicate terminal: ' + output.read_text()
    result = events[0]
    assert result['outcome'] == 'succeeded', result
    assert 'X-Livepeer-Work-Units: ' + str(result['usage']['units']) in headers, headers
    return result
try:
    for kind, port in [('vod', 18076), ('abr', 18077)]:
        name = 'standalone-' + kind + '-smoke-' + str(os.getpid())
        names.append(name)
        package = 'transcode-runner' if kind == 'vod' else 'abr-runner'
        base = 'http://127.0.0.1:' + str(port)
        subprocess.run(['docker', 'run', '-d', '--rm', '--name', name, '--network', 'host', '-e', 'RUNNER_ADDR=127.0.0.1:' + str(port), '-e', 'PRESETS_FILE=/smoke-presets.yaml', '-v', str(pathlib.Path(package + '/testdata/image-smoke-presets.yaml').resolve()) + ':/smoke-presets.yaml:ro', os.environ.get(kind.upper() + '_SMOKE_IMAGE', 'localbuild/standalone-' + kind + '-validation:v2-local')], check=True, stdout=subprocess.DEVNULL)
        for _ in range(40):
            try:
                urllib.request.urlopen(base + '/healthz', timeout=1).read()
                break
            except Exception:
                time.sleep(0.25)
        else:
            raise Exception(kind + ' runner failed readiness')
        common = {'schema': 'video-transcode-' + kind + '/v2', 'workload_id': 'smoke-' + kind + '-' + str(os.getpid()), 'input': {'download_url': 'http://127.0.0.1:18079/input.mp4'}}
        if kind == 'vod':
            common.update({'rendition': {'name': 'tiny', 'width': 64, 'height': 64, 'fps': 30, 'codec': 'h264'}, 'output': {'stream': dest('vod.mp4')}})
            path = '/v1/video/transcode'
        else:
            common.update({'ladder': {'preset': 'smoke-audio-only'}, 'output': {'manifest': dest('master.m3u8'), 'renditions': {'audio-only': {'playlist': dest('audio.m3u8'), 'stream': dest('audio.mp4')}}}})
            path = '/v1/video/transcode/abr'
        result = invoke(base, path, common)
        count = sum(map(len, uploads.values()))
        assert invoke(base, path, common) == result
        assert sum(map(len, uploads.values())) == count, 'replay duplicated upload'
        if kind == 'vod':
            assert result['usage']['units'] > 0 and result['video']['actual_frames'] == 60, result
        else:
            assert result['usage']['units'] == 0
        assert all((len(v[0]) > 0 for v in uploads.values())), 'empty upload'
        changed = json.loads(json.dumps(common))
        changed['input']['download_url'] += '?changed=1'
        try:
            urllib.request.urlopen(urllib.request.Request(base + path, data=json.dumps(changed).encode(), headers={'Content-Type': 'application/json', 'Accept': 'text/event-stream'}))
            raise Exception('changed workload accepted')
        except urllib.error.HTTPError as e:
            assert e.code == 409
        print('PASS', kind, 'real FFmpeg terminal SSE + usage trailer + artifact upload + identical replay + changed ID rejection; units=', result['usage']['units'])
finally:
    for name in names:
        subprocess.run(['docker', 'stop', '--time', '10', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    server.shutdown()
    work.cleanup()
