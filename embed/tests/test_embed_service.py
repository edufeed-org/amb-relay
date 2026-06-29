TEST_TOKEN = "test-token-xyz"


def test_health_returns_768_dim(client):
    r = client.get("/health")
    assert r.status_code == 200
    body = r.json()
    assert body["dimensions"] == 768


def test_embed_returns_one_vector_per_input(client):
    r = client.post(
        "/embed",
        headers={"Authorization": f"Bearer {TEST_TOKEN}"},
        json={"texts": ["hallo welt", "guten morgen"]},
    )
    assert r.status_code == 200
    body = r.json()
    assert "embeddings" in body
    assert len(body["embeddings"]) == 2
    for vec in body["embeddings"]:
        assert isinstance(vec, list)
        assert len(vec) == 768
        assert all(isinstance(x, float) for x in vec)


def test_embed_rejects_missing_token(client):
    r = client.post("/embed", json={"texts": ["x"]})
    assert r.status_code == 401


def test_embed_rejects_bad_token(client):
    r = client.post(
        "/embed",
        headers={"Authorization": "Bearer not-the-right-token"},
        json={"texts": ["x"]},
    )
    assert r.status_code == 401


def test_embed_applies_passage_prefix_by_default(client, monkeypatch):
    import numpy as np
    import embed_service

    captured = {}

    def fake_encode(texts, normalize_embeddings=True):
        captured["texts"] = list(texts)
        return np.zeros((len(texts), embed_service._dim), dtype="float32")

    monkeypatch.setattr(embed_service._model, "encode", fake_encode)
    r = client.post(
        "/embed",
        headers={"Authorization": f"Bearer {TEST_TOKEN}"},
        json={"texts": ["hallo welt"]},
    )
    assert r.status_code == 200
    assert captured["texts"] == ["passage: hallo welt"]


def test_embed_applies_query_prefix(client, monkeypatch):
    import numpy as np
    import embed_service

    captured = {}

    def fake_encode(texts, normalize_embeddings=True):
        captured["texts"] = list(texts)
        return np.zeros((len(texts), embed_service._dim), dtype="float32")

    monkeypatch.setattr(embed_service._model, "encode", fake_encode)
    r = client.post(
        "/embed",
        headers={"Authorization": f"Bearer {TEST_TOKEN}"},
        json={"texts": ["hallo welt"], "input_type": "query"},
    )
    assert r.status_code == 200
    assert captured["texts"] == ["query: hallo welt"]
