# Build from the repository root: docker build -f mock/node/Dockerfile -t lsp2-mock-node .
FROM node:20-alpine
WORKDIR /app/node
COPY mock/node/package.json mock/node/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund
COPY mock/node/lsp2.mjs mock/node/gateway.mjs ./
COPY mock/web/chat.html mock/web/chat.js mock/web/qrcode.js /app/web/
ENV LSP2_LISTEN=0.0.0.0:8080
EXPOSE 8080
USER node
ENTRYPOINT ["node", "gateway.mjs"]
