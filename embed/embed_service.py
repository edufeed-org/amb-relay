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
# different prefixes. Arctic v2.0 instead uses a built-in query prompt and
# encodes passages bare. Model-family knowledge lives only in the helpers below.
E5_PREFIXES = {"query": "query: ", "passage": "passage: "}


def _is_arctic(name: str) -> bool:
    return "arctic" in name.lower()


def _load_model(name: str) -> SentenceTransformer:
    # Arctic v2.0 needs the de-risk-proven xformers-free load (eager attention,
    # no memory-efficient attention / input unpadding) so it runs on CPU without
    # the optional xformers dependency.
    if _is_arctic(name):
        return SentenceTransformer(
            name,
            trust_remote_code=True,
            model_kwargs={"attn_implementation": "eager"},
            config_kwargs={
                "use_memory_efficient_attention": False,
                "unpad_inputs": False,
            },
        )
    return SentenceTransformer(name)


def encode_texts(model, model_name: str, texts: list[str], input_type: str):
    # All paths normalize so cosine == dot. Arctic: query uses the model's
    # built-in "query" prompt, passages encode bare. e5: prepend string prefix.
    if _is_arctic(model_name):
        if input_type == "query":
            return model.encode(texts, prompt_name="query", normalize_embeddings=True)
        return model.encode(texts, normalize_embeddings=True)
    prefix = E5_PREFIXES.get(input_type, E5_PREFIXES["passage"])
    return model.encode([prefix + t for t in texts], normalize_embeddings=True)


app = FastAPI()
_model = _load_model(MODEL_NAME)
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
    vectors = encode_texts(_model, MODEL_NAME, req.texts, req.input_type)
    return EmbedResponse(
        embeddings=vectors.tolist(),
        model=MODEL_NAME,
        dimensions=_dim,
    )


@app.get("/health")
def health() -> dict:
    return {"status": "ok", "model": MODEL_NAME, "dimensions": _dim}
