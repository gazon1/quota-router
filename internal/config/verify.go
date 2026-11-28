package config

import (
	"github.com/gazon1/quota-router/internal/settings"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxModelsBody bounds the /v1/models answer. The measured body for four models
// is a few KiB; an answer this large is not the proxy's model list.
const maxModelsBody = 1 << 20

// ErrCatalogueUnavailable means the catalogue could not be checked: the proxy was
// unreachable, refused the request or answered with something that is not a model
// list. That says nothing about the configuration, so a caller should report it
// and carry on, unlike a mismatch.
var ErrCatalogueUnavailable = errors.New("LiteLLM model list unavailable")

// VerifyCatalogue checks every configured alias against the model list LiteLLM
// itself serves, because the configuration is the only source of model ids: an
// alias the proxy does not know would fail at request time, not at startup.
//
// Two failures are distinguished. ErrCatalogueUnavailable is transient and the
// configuration is not at fault. Any other error is a mismatch: the proxy
// answered, and a configured alias is not served, so requests for it cannot
// succeed. Unset litellm.api_key skips the check, because the proxy needs its
// master key for /v1/models.
func (c Config) VerifyCatalogue(ctx context.Context, client *http.Client) error {
	if c.LiteLLM.APIKey == "" {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	known, err := c.liteLLMModels(ctx, client)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCatalogueUnavailable, err)
	}

	var errs []error
	for _, alias := range sortedAliases(c.LiteLLM.ModelAliases) {
		model := c.LiteLLM.ModelAliases[alias].Model
		if !known[model] {
			errs = append(errs, fmt.Errorf("litellm.model_aliases.%s: %q is not in the model list GET /v1/models returns", alias, model))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("catalogue does not match the LiteLLM configuration: %w", errors.Join(errs...))
	}
	return nil
}

// liteLLMModels returns the set of model ids GET /v1/models lists.
func (c Config) liteLLMModels(ctx context.Context, client *http.Client) (map[string]bool, error) {
	// Reuse the poll timeout: this is one bounded request like a poll, and a
	// proxy that does not answer it is the same kind of problem.
	ctx, cancel := context.WithTimeout(ctx, c.Polling.RequestTimeout)
	defer cancel()

	url := strings.TrimSuffix(c.LiteLLM.BaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// LiteLLM authenticates the request with its own bearer master key.
	req.Header.Set("Authorization", "Bearer "+string(c.LiteLLM.APIKey))

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Capped one byte past the limit, so an oversized answer is detected rather
	// than silently truncated into a partial model list.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxModelsBody {
		return nil, fmt.Errorf("response exceeds %d bytes", maxModelsBody)
	}
	if resp.StatusCode != http.StatusOK {
		// The status, never the body: an error body can echo the credential.
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if len(doc.Data) == 0 {
		return nil, errors.New("empty model list; the proxy is not configured")
	}
	known := make(map[string]bool, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID == "" {
			return nil, errors.New("model list entry without an id")
		}
		known[m.ID] = true
	}
	return known, nil
}

// sortedAliases returns catalogue aliases in a stable order.
func sortedAliases(models map[string]Model) []string {
	return settings.SortedAliases(models)
}
