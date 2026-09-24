import { test } from 'node:test';
import assert from 'node:assert/strict';
import { consumeTerminal } from './sse.mjs';
async function* chunks(text) { for (let i = 0; i < text.length; i += 3) yield Buffer.from(text.slice(i, i + 3)); }
const success = 'event: result\ndata: {"outcome":"succeeded","usage":{"unit":"video-frame-megapixel","units":123}}\n\n';
test('fragmented terminal stream returns measured units', async () => assert.equal((await consumeTerminal(chunks(': keepalive\n\n' + success))).usage.units, 123));
test('missing terminal fails', async () => assert.rejects(consumeTerminal(chunks(': keepalive\n\n')), /without terminal/));
test('multiple terminal results fail', async () => assert.rejects(consumeTerminal(chunks(success + success)), /multiple terminal/));
test('terminal error fails the smoke run', async () => assert.rejects(consumeTerminal(chunks('event: error\ndata: {"outcome":"failed","error":{"code":"encode_failed"}}\n\n')), /encode_failed/));
test('CRLF split across chunks is accepted', async () => assert.equal((await consumeTerminal(chunks(success.replaceAll('\n', '\r\n')))).usage.units, 123));
