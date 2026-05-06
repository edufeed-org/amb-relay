FROM python:3.11-slim

WORKDIR /app

# Install Python deps. CPU-only torch wheel from pytorch.org cuts ~2 GB vs.
# the default GPU build pulled from PyPI.
COPY requirements.txt .
RUN pip install --no-cache-dir \
        --extra-index-url https://download.pytorch.org/whl/cpu \
        -r requirements.txt

COPY embed_service.py .

# HuggingFace cache lives here; mount a volume to persist the model
# (~120 MB for paraphrase-multilingual-MiniLM-L12-v2) across container restarts.
ENV HF_HOME=/cache/huggingface

EXPOSE 8100

# Single worker keeps memory footprint at ~1.5 GB. The model is the
# bottleneck; adding workers multiplies RAM without helping latency
# (sentence-transformers is GIL-friendly and CPU-bound per call).
CMD ["uvicorn", "embed_service:app", "--host", "0.0.0.0", "--port", "8100"]
