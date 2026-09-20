package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// QueryPrometheus runs one instant query and returns the series it yields.
// Anything that is not a clean "success" is an error: the caller must be able
// to tell "no series" (all is well) from "no answer" (we do not know).
func QueryPrometheus(ctx context.Context, client *http.Client, baseURL, query string) ([]Sample, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("HTTP %d, not a Prometheus answer: %w", resp.StatusCode, err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, parsed.Error)
	}

	switch parsed.Data.ResultType {
	case "vector":
		var series []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(parsed.Data.Result, &series); err != nil {
			return nil, err
		}
		samples := make([]Sample, 0, len(series))
		for _, s := range series {
			v, err := sampleValue(s.Value[1])
			if err != nil {
				return nil, err
			}
			samples = append(samples, Sample{Labels: s.Metric, Value: v})
		}
		return samples, nil

	case "scalar":
		var pair [2]any
		if err := json.Unmarshal(parsed.Data.Result, &pair); err != nil {
			return nil, err
		}
		v, err := sampleValue(pair[1])
		if err != nil {
			return nil, err
		}
		return []Sample{{Labels: map[string]string{}, Value: v}}, nil
	}
	return nil, fmt.Errorf("query must return an instant vector or scalar, got %q", parsed.Data.ResultType)
}

func sampleValue(raw any) (float64, error) {
	s, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("unexpected sample value %v", raw)
	}
	return strconv.ParseFloat(s, 64)
}
