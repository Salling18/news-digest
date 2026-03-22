package embedclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Client is an HTTP client for the Python embed microservice.
type Client struct {
	baseURL string
	http    *http.Client
}

// New creates a Client that talks to the embed service at baseURL.
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// embedRequest is the JSON body for POST /embed.
type embedRequest struct {
	Texts []string `json:"texts"`
}

// embedResponse is the JSON response from POST /embed.
type embedResponse struct {
	Vectors [][]float32 `json:"vectors"`
}

// Embed sends texts to the embed service and returns their vector embeddings.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("embedclient.Embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedclient.Embed: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedclient.Embed: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedclient.Embed: unexpected status %d: %s", resp.StatusCode, respBody)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embedclient.Embed: decode response: %w", err)
	}
	return result.Vectors, nil
}

// clusterRequest is the JSON body for POST /cluster.
type clusterRequest struct {
	Vectors        [][]float32 `json:"vectors"`
	ArticleIDs     []int64     `json:"article_ids"`
	MinClusterSize int         `json:"min_cluster_size"`
}

// clusterResponse is the JSON response from POST /cluster.
type clusterResponse struct {
	Assignments map[string]int `json:"assignments"`
}

// Cluster sends vectors and article IDs to the clustering endpoint and returns
// a mapping of article ID to cluster label.
func (c *Client) Cluster(ctx context.Context, vectors [][]float32, articleIDs []int64, minClusterSize int) (map[int64]int, error) {
	body, err := json.Marshal(clusterRequest{
		Vectors:        vectors,
		ArticleIDs:     articleIDs,
		MinClusterSize: minClusterSize,
	})
	if err != nil {
		return nil, fmt.Errorf("embedclient.Cluster: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/cluster", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedclient.Cluster: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedclient.Cluster: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedclient.Cluster: unexpected status %d: %s", resp.StatusCode, respBody)
	}

	var result clusterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embedclient.Cluster: decode response: %w", err)
	}

	assignments := make(map[int64]int, len(result.Assignments))
	for k, v := range result.Assignments {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("embedclient.Cluster: parse article id %q: %w", k, err)
		}
		assignments[id] = v
	}
	return assignments, nil
}

// Health checks whether the embed service is reachable.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("embedclient.Health: create request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("embedclient.Health: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("embedclient.Health: unexpected status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
