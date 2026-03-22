"""Minimal embedding and clustering microservice."""

import logging

import numpy as np
from fastapi import FastAPI
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer
from sklearn.cluster import HDBSCAN

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = FastAPI(title="Embed Service")

logger.info("Loading sentence-transformers model...")
model = SentenceTransformer("all-MiniLM-L6-v2")
logger.info("Model loaded.")


# ---------------------------------------------------------------------------
# Request / response models
# ---------------------------------------------------------------------------


class EmbedRequest(BaseModel):
    texts: list[str]


class EmbedResponse(BaseModel):
    vectors: list[list[float]]


class ClusterRequest(BaseModel):
    vectors: list[list[float]]
    article_ids: list[int]
    min_cluster_size: int = 3


class ClusterResponse(BaseModel):
    assignments: dict[str, int]


class HealthResponse(BaseModel):
    status: str


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------


@app.post("/embed", response_model=EmbedResponse)
def embed(req: EmbedRequest) -> EmbedResponse:
    """Compute sentence embeddings for a list of texts."""
    vectors = model.encode(req.texts, convert_to_numpy=True).astype("float32").tolist()
    return EmbedResponse(vectors=vectors)


@app.post("/cluster", response_model=ClusterResponse)
def cluster(req: ClusterRequest) -> ClusterResponse:
    """Cluster vectors using HDBSCAN and return article-ID to label mapping."""
    matrix = np.array(req.vectors, dtype=np.float32)
    labels = HDBSCAN(
        metric="cosine", min_cluster_size=req.min_cluster_size
    ).fit_predict(matrix)
    assignments = {
        str(aid): int(label) for aid, label in zip(req.article_ids, labels)
    }
    return ClusterResponse(assignments=assignments)


@app.get("/health", response_model=HealthResponse)
def health() -> HealthResponse:
    """Simple liveness check."""
    return HealthResponse(status="ok")
