# Build from the repository root: docker build -f mock/python/Dockerfile -t lsp2-mock-python .
FROM python:3.12-slim
WORKDIR /app
COPY sdk/python /app/sdk/python
RUN pip install --no-cache-dir "/app/sdk/python[server]" "websockets>=12"
COPY mock/python/gateway.py /app/python/
COPY mock/web/chat.html mock/web/chat.js mock/web/qrcode.js /app/web/
WORKDIR /app/python
ENV LSP2_LISTEN=0.0.0.0:8080 PYTHONUNBUFFERED=1
EXPOSE 8080
USER nobody
ENTRYPOINT ["python", "gateway.py"]
