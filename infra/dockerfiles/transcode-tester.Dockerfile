ARG NODE_VERSION=22

FROM node:${NODE_VERSION}-alpine AS build
WORKDIR /app
COPY transcode-tester/package.json transcode-tester/package-lock.json ./
RUN npm ci --omit=dev
COPY transcode-tester/*.mjs transcode-tester/*.json ./

FROM node:${NODE_VERSION}-alpine
WORKDIR /app
RUN adduser -D -H runner
USER runner
COPY --from=build --chown=runner /app /app
ENV TRANSCODE_BASE_URL=http://localhost:8086 \
    ABR_BASE_URL=http://localhost:8087
CMD ["node", "test-transcode.mjs", "presets"]
