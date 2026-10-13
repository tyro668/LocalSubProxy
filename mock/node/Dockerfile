FROM node:20-alpine
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund
COPY lsp2.mjs gateway.mjs ./
ENV LSP2_LISTEN=0.0.0.0:8080
EXPOSE 8080
USER node
ENTRYPOINT ["node", "gateway.mjs"]
