FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY lsp2.py gateway.py ./
ENV LSP2_LISTEN=0.0.0.0:8080 PYTHONUNBUFFERED=1
EXPOSE 8080
USER nobody
ENTRYPOINT ["python", "gateway.py"]
