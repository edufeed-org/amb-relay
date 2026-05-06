import os
import sys

import pytest
from fastapi.testclient import TestClient

# Hard-coded token used by tests; must match TEST_TOKEN in test_embed_service.py.
TEST_TOKEN = "test-token-xyz"


@pytest.fixture(scope="session")
def client():
    os.environ["EMBED_TOKEN"] = TEST_TOKEN
    sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    import embed_service  # noqa: WPS433 — runtime import after env setup is intentional

    return TestClient(embed_service.app)
