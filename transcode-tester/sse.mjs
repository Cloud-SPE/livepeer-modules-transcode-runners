import { readFile } from 'node:fs/promises';

export async function consumeTerminal(stream, onProgress = () => {}) {
  const decoder = new TextDecoder();
  let buffer = '', terminal;
  const event = (block) => {
    const lines = block.split('\n');
    const kind = lines.find(l => l.startsWith('event:'))?.slice(6).trim();
    const data = lines.filter(l => l.startsWith('data:')).map(l => l.slice(5).trimStart()).join('\n');
    if (!data) return;
    const value = JSON.parse(data);
    if (kind === 'result' || kind === 'error') {
      if (terminal) throw new Error('multiple terminal events');
      terminal = value;
    } else if (kind === 'progress') onProgress(value);
  };
  for await (const chunk of stream) {
    buffer = (buffer + decoder.decode(chunk, { stream: true })).replace(/\r\n/g, '\n');
    let end;
    while ((end = buffer.indexOf('\n\n')) !== -1) {
      event(buffer.slice(0, end)); buffer = buffer.slice(end + 2);
    }
    if (buffer.length > 1024 * 1024) throw new Error('oversized SSE event');
  }
  buffer += decoder.decode();
  if (buffer.trim()) event(buffer);
  if (!terminal) throw new Error('stream ended without terminal result');
  if (terminal.outcome !== 'succeeded') throw new Error(`runner failed: ${terminal.error?.code ?? 'unknown'}`);
  if (terminal.usage?.unit !== 'video-frame-megapixel' || !Number.isSafeInteger(terminal.usage.units) || terminal.usage.units < 0) throw new Error('invalid terminal usage');
  return terminal;
}

export async function runSmoke(base, path, schema) {
  const mode = process.argv[2] || 'presets';
  if (mode === 'presets' || mode === 'contract') {
    const response = await fetch(base + (mode === 'contract' ? '/.well-known/livepeer-runner' : path + '/presets'));
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    console.log(JSON.stringify(await response.json(), null, 2));
    return;
  }
  if (mode !== 'quick') throw new Error('usage: presets | contract | quick');
  if (!process.env.REQUEST_FILE) throw new Error('REQUEST_FILE must name a v2 JSON request with fresh signed URLs');
  const request = JSON.parse(await readFile(process.env.REQUEST_FILE, 'utf8'));
  if (request.schema !== schema) throw new Error(`request schema must be ${schema}`);
  const response = await fetch(base + path, {
    method: 'POST', headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
    body: JSON.stringify(request), signal: AbortSignal.timeout(Number(process.env.TIMEOUT_MS || 3600000)),
  });
  if (!response.ok) throw new Error(`runner HTTP ${response.status}`);
  if (!response.headers.get('content-type')?.includes('text/event-stream')) throw new Error('runner did not return SSE');
  const result = await consumeTerminal(response.body, p => console.log(`progress ${p.sequence} ${p.phase} ${p.overall_progress}%`));
  console.log(JSON.stringify(result, null, 2));
}
