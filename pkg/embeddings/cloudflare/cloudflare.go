package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pkg/errors"

	chttp "github.com/guiperry/chroma-go_cerebras/pkg/commons/http"
	"github.com/guiperry/chroma-go_cerebras/pkg/embeddings"
)

// Docs:  https://developers.cloudflare.com/workers-ai/ (Cloudflare Workers AI) and https://developers.cloudflare.com/workers-ai/models/embedding/ (Embedding API)

const (
	defaultBaseAPI = "https://api.cloudflare.com/client/v4/"
	// https://developers.cloudflare.com/workers-ai/models/bge-small-en-v1.5/#api-schema (Input JSON Schema)
	defaultMaxSize = 100
)

type CloudflareClient struct {
	BaseAPI        string
	endpoint       string
	APIToken       string
	AccountID      string
	DefaultModel   embeddings.EmbeddingModel
	IsGateway      bool
	MaxBatchSize   int
	DefaultHeaders map[string]string
	Client         *http.Client
}

func applyDefaults(c *CloudflareClient) {
	if c.Client == nil {
		c.Client = http.DefaultClient
	}
	if c.BaseAPI == "" {
		c.BaseAPI = defaultBaseAPI
	}
	if !strings.HasSuffix(c.BaseAPI, "/") {
		c.BaseAPI += "/"
	}
	if c.MaxBatchSize == 0 {
		c.MaxBatchSize = defaultMaxSize
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "@cf/baai/bge-base-en-v1.5"
	}
	if c.IsGateway {
		c.endpoint = fmt.Sprintf("%s%s", c.BaseAPI, c.DefaultModel)
	} else {
		c.endpoint = fmt.Sprintf("%saccounts/%s/ai/run/%s", c.BaseAPI, c.AccountID, c.DefaultModel)
	}
}

func validate(c *CloudflareClient) error {
	if c.APIToken == "" {
		return errors.Errorf("API key is required")
	}
	if c.AccountID == "" && !c.IsGateway {
		return errors.Errorf("account ID is required")
	}
	if c.MaxBatchSize <= 0 {
		return errors.Errorf("max batch size must be greater than 0")
	}
	if c.MaxBatchSize > defaultMaxSize {
		return errors.Errorf("max batch size must be less than %d", defaultMaxSize)
	}
	return nil
}

func NewCloudflareClient(opts ...Option) (*CloudflareClient, error) {
	client := &CloudflareClient{}

	for _, opt := range opts {
		err := opt(client)
		if err != nil {
			return nil, errors.Wrap(err, "failed to initialize CloudflareClient")
		}
	}
	applyDefaults(client)
	if err := validate(client); err != nil {
		return nil, errors.Wrap(err, "failed to validate CloudflareClient configuration")
	}
	return client, nil
}

type CreateEmbeddingRequest struct {
	Text []string `json:"text"`
}
type Result struct {
	Shape []int       `json:"shape"`
	Data  [][]float32 `json:"data"`
}
type CreateEmbeddingResponse struct {
	Success  bool   `json:"success"`
	Messages []any  `json:"messages"`
	Errors   []any  `json:"errors"`
	Result   Result `json:"result"`
}

func (c *CreateEmbeddingRequest) JSON() (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *CloudflareClient) CreateEmbedding(ctx context.Context, req *CreateEmbeddingRequest) (*CreateEmbeddingResponse, error) {
	reqJSON, err := req.JSON()
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewBufferString(reqJSON))
	if err != nil {
		return nil, err
	}
	for k, v := range c.DefaultHeaders {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", chttp.ChromaGoClientUserAgent)
	httpReq.Header.Set("Authorization", "Bearer "+c.APIToken)
	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return nil, errors.Wrap(err, "failed send request to Cloudflare API")
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read response body")
	}
	// we also process any embedding errors in the response
	var embeddings CreateEmbeddingResponse
	if err := json.Unmarshal(respData, &embeddings); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response body")
	}
	if resp.StatusCode != http.StatusOK || len(embeddings.Errors) > 0 {
		return nil, errors.Errorf("unexpected code [%v] while making a request to %v. errors: %v\n%v", resp.Status, c.endpoint, embeddings.Errors, string(respData))
	}

	return &embeddings, nil
}

var _ embeddings.EmbeddingFunction = (*CloudflareEmbeddingFunction)(nil)

type CloudflareEmbeddingFunction struct {
	apiClient *CloudflareClient
}

func NewCloudflareEmbeddingFunction(opts ...Option) (*CloudflareEmbeddingFunction, error) {
	client, err := NewCloudflareClient(opts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize CloudflareClient")
	}

	return &CloudflareEmbeddingFunction{apiClient: client}, nil
}

func (e *CloudflareEmbeddingFunction) EmbedDocuments(ctx context.Context, documents []string) ([]embeddings.Embedding, error) {
	if len(documents) > e.apiClient.MaxBatchSize {
		return nil, errors.Errorf("number of documents exceeds the maximum batch size %v", e.apiClient.MaxBatchSize)
	}
	if len(documents) == 0 {
		return embeddings.NewEmptyEmbeddings(), nil
	}
	req := &CreateEmbeddingRequest{
		Text: documents,
	}
	response, err := e.apiClient.CreateEmbedding(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to embed documents")
	}
	return embeddings.NewEmbeddingsFromFloat32(response.Result.Data)
}

func (e *CloudflareEmbeddingFunction) EmbedQuery(ctx context.Context, document string) (embeddings.Embedding, error) {
	req := &CreateEmbeddingRequest{
		Text: []string{document},
	}
	response, err := e.apiClient.CreateEmbedding(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to embed query")
	}
	return embeddings.NewEmbeddingFromFloat32(response.Result.Data[0]), nil
}
