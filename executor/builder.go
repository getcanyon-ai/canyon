package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// RunImage asks which run image a /build now builds onto and a /rebase rebases onto (contracts/executor.md,
// GET /builder's run_image): "" from an executor that predates it, which answers builder alone or no
// /builder at all (404), so the caller plans no rebase there.
func (c *Client) RunImage(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/builder", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("executor: /builder answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res struct {
		RunImage string `json:"run_image"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&res); err != nil {
		return "", fmt.Errorf("executor: /builder: %w", err)
	}
	return res.RunImage, nil
}
