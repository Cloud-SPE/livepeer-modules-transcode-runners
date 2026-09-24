#!/usr/bin/env node
import { runSmoke } from './sse.mjs';
runSmoke(process.env.ABR_BASE_URL || 'http://localhost:8087', '/v1/video/transcode/abr', 'video-transcode-abr/v2')
  .catch(e => { console.error(e.message); process.exitCode = 1; });
