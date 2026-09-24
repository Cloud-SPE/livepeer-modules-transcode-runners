#!/usr/bin/env node
import { runSmoke } from './sse.mjs';
runSmoke(process.env.TRANSCODE_BASE_URL || 'http://localhost:8086', '/v1/video/transcode', 'video-transcode-vod/v2')
  .catch(e => { console.error(e.message); process.exitCode = 1; });
