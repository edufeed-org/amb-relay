"""Self-hosted embedding service for amb-relay / amb-indexer.

Loads a single sentence-transformers model at startup and exposes
POST /embed and GET /health. Auth is a single bearer token from the
EMBED_TOKEN env var; bad or missing tokens return 401.

Wire contract (must match amb-indexer/embed.go):
  POST /embed
    body:    {"texts": ["...", "..."], "input_type": "query"|"passage"}
    response:{"embeddings": [[float, ...], ...], "model": str, "dimensions": int}
"""

import os

from fastapi import Depends, FastAPI, HTTPException, Request
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

MODEL_NAME = os.environ.get(
    "EMBED_MODEL", "intfloat/multilingual-e5-base"
)
EXPECTED_TOKEN = os.environ.get("EMBED_TOKEN", "")

# e5 models are asymmetric: a query and the passage it should match get
# different prefixes. This is the only place model-specific prefix knowledge
# lives — a prefix-free model (e.g. bge-m3) would set both to "".
E5_PREFIXES = {"query": "query: ", "passage": "passage: "}

app = FastAPI()
_model = SentenceTransformer(MODEL_NAME)
_dim = _model.get_sentence_embedding_dimension()


def verify_bearer(request: Request) -> None:
    auth = request.headers.get("Authorization", "")
    if not auth.startswith("Bearer "):
        raise HTTPException(status_code=401, detail="missing bearer token")
    token = auth.removeprefix("Bearer ")
    if not EXPECTED_TOKEN or token != EXPECTED_TOKEN:
        raise HTTPException(status_code=401, detail="invalid token")


class EmbedRequest(BaseModel):
    texts: list[str]
    input_type: str = "passage"


class EmbedResponse(BaseModel):
    embeddings: list[list[float]]
    model: str
    dimensions: int


@app.post("/embed", response_model=EmbedResponse, dependencies=[Depends(verify_bearer)])
def embed(req: EmbedRequest) -> EmbedResponse:
    prefix = E5_PREFIXES.get(req.input_type, E5_PREFIXES["passage"])
    prefixed = [prefix + t for t in req.texts]
    vectors = _model.encode(prefixed, normalize_embeddings=True)
    return EmbedResponse(
        embeddings=vectors.tolist(),
        model=MODEL_NAME,
        dimensions=_dim,
    )


@app.get("/health")
def health() -> dict:
    return {"status": "ok", "model": MODEL_NAME, "dimensions": _dim}
