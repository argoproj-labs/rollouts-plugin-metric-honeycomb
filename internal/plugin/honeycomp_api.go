package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/argoproj/argo-rollouts/utils/defaults"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	HoneycombSecret = "honeycomb"
	HoneycombAPIKey = "api-key"
	HoneycombURL    = "https://api.honeycomb.io"
)

type Calculation struct {
	Op     string  `json:"op"`
	Column *string `json:"column"`
}

type Filter struct {
	Op     string      `json:"op"`
	Column *string     `json:"column"`
	Value  interface{} `json:"value"`
}

type Order struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Order  string `json:"order"`
}

type Having struct {
	CalculateOp string  `json:"calculate_op"`
	Column      *string `json:"column"`
	Op          string  `json:"op"`
	Value       float64 `json:"value"`
}

type Query struct {
	ID           string        `json:"id"`
	Breakdowns   []string      `json:"breakdowns"`
	Calculations []Calculation `json:"calculations"`
	Filters      []Filter      `json:"filters"`
	FilterCombo  string        `json:"filter_combination"`
	Granularity  int           `json:"granularity"`
	Orders       []Order       `json:"orders"`
	Limit        int           `json:"limit"`
	EndTime      int           `json:"end_time"`
	TimeRange    int           `json:"time_range"`
	Havings      []Having      `json:"havings"`
}

type SeriesDatum struct {
	Time string      `json:"time"`
	Data interface{} `json:"data"`
}

type ResultsDatum struct {
	Data map[string]interface{} `json:"data"`
}

type QueryResultData struct {
	Series  []SeriesDatum  `json:"series"`
	Results []ResultsDatum `json:"results"`
}

type QueryResult struct {
	Query    Query           `json:"query"`
	ID       string          `json:"id"`
	Complete bool            `json:"complete"`
	Data     QueryResultData `json:"data"`
	Links    struct {
		QueryURL      string `json:"query_url"`
		GraphImageURL string `json:"graph_image_url"`
	} `json:"links"`
}

// HoneycombAPI is the interface to query Honeycomb
type honeycombAPI interface {
	CreateQuery(ctx context.Context, query string, dataset string) (*Query, error)
	GetQueryResult(ctx context.Context, queryID string, dataset string) (*QueryResult, error)
}

type honeycombClient struct {
	apiKey string
	client *http.Client
}

var _ honeycombAPI = &honeycombClient{}

func newHoneycombAPI(logCtx log.Entry, kubeclientset kubernetes.Interface) (honeycombAPI, error) {
	var apiKey string

	ns := defaults.Namespace()
	secret, err := kubeclientset.CoreV1().Secrets(ns).Get(context.Background(), HoneycombSecret, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	apiKey = string(secret.Data[HoneycombAPIKey])

	tr := &http.Transport{
		MaxIdleConns:       10,
		IdleConnTimeout:    30 * time.Second,
		DisableCompression: true,
	}
	client := &http.Client{
		Transport: tr,
	}

	return &honeycombClient{
		apiKey: apiKey,
		client: client,
	}, nil
}

type errorResponse struct {
	Error string `json:"error"`
}

func (c *honeycombClient) CreateQuery(ctx context.Context, query string, dataset string) (*Query, error) {
	if dataset == "" {
		dataset = "__all__"
	}

	requestBody := bytes.NewBuffer([]byte(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, HoneycombURL+"/1/queries/"+dataset, requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("X-Honeycomb-Team", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var q Query
		if err := json.Unmarshal(bodyBytes, &q); err != nil {
			return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
		}
		return &q, nil
	}

	var e errorResponse
	if err := json.Unmarshal(bodyBytes, &e); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
	}

	return nil, fmt.Errorf("failed to create query: %s", e.Error)
}

type createQueryResultRequest struct {
	QueryID       string `json:"query_id"`
	DisableSeries bool   `json:"disable_series"`
	Limit         int    `json:"limit"`
}

func (c *honeycombClient) GetQueryResult(ctx context.Context, queryID string, dataset string) (*QueryResult, error) {
	if queryID == "" {
		return nil, errors.New("query ID cannot be empty")
	}

	if dataset == "" {
		dataset = "__all__"
	}

	// first, create the query result
	reqPayload := createQueryResultRequest{
		QueryID:       queryID,
		DisableSeries: false,
		Limit:         10000,
	}
	reqBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	bodyReader := bytes.NewReader(reqBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, HoneycombURL+"/1/query_results/"+dataset, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Add("X-Honeycomb-Team", c.apiKey)
	req.Header.Add("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	// now check for the query result
	location := resp.Header.Get("Location")
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, HoneycombURL+location, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Add("X-Honeycomb-Team", c.apiKey)
	req.Header.Add("Content-Type", "application/json")

	var qr QueryResult

	ticker := time.NewTicker(1 * time.Second)
	timer := time.NewTimer(10 * time.Second)
	defer ticker.Stop()
	defer timer.Stop()

	// query results cannot take longer than 10 seconds to run
	// ref: https://docs.honeycomb.io/api/tag/Query-Data
loop:
	for {
		select {
		case <-timer.C:
			return nil, errors.New("timed out waiting for query result")

		case <-ticker.C:
			resp, err := c.client.Do(req)
			if err != nil {
				return nil, fmt.Errorf("failed to execute request: %w", err)
			}
			defer resp.Body.Close()

			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to read response body: %w", err)
			}

			if resp.StatusCode == http.StatusOK {
				if err := json.Unmarshal(bodyBytes, &qr); err != nil {
					return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
				}

				if qr.Complete {
					break loop
				}
			}

			var e errorResponse
			if err := json.Unmarshal(bodyBytes, &e); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
			}
		}
	}

	return &qr, nil
}
