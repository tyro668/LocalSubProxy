# Build from the repository root: docker build -f mock/python/Dockerfile -t lsp2-mock-python .
FROM python:3.12-slim
WORKDIR /app/python
COPY mock/python/requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY mock/python/lsp2.py mock/python/gateway.py ./
COPY mock/web/chat.html mock/web/chat.js mock/web/qrcode.js /app/web/
ENV LSP2_LISTEN=0.0.0.0:8080 PYTHONUNBUFFERED=1
EXPOSE 8080
USER nobody
ENTRYPOINT ["python", "gateway.py"]
