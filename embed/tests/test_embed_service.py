TEST_TOKEN = "test-token-xyz"


def test_health_returns_384_dim(client):
    r = client.get("/health")
    assert r.status_code == 200
    body = r.json()
    assert body["dimensions"] == 384


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
        assert len(vec) == 384
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
