# Build from the repository root: docker build -f mock/node/Dockerfile -t lsp2-mock-node .
FROM node:20-alpine AS sdk
WORKDIR /app/sdk/node
COPY sdk/node/package.json sdk/node/package-lock.json sdk/node/tsconfig.json ./
RUN npm ci --no-audit --no-fund --ignore-scripts --omit=optional
COPY sdk/node/src ./src
COPY sdk/node/vendor ./vendor
RUN npx tsc -p tsconfig.json && npm prune --omit=dev --omit=optional

FROM node:20-alpine
COPY --from=sdk /app/sdk/node /app/sdk/node
WORKDIR /app/node
RUN mkdir -p node_modules && ln -s /app/sdk/node node_modules/lsp2-server
COPY mock/node/gateway.mjs ./
COPY mock/web/chat.html mock/web/chat.js mock/web/qrcode.js /app/web/
ENV LSP2_LISTEN=0.0.0.0:8080
EXPOSE 8080
USER node
ENTRYPOINT ["node", "gateway.mjs"]
