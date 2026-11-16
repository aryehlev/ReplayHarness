package discover

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

// Places where frameworks commonly serve their OpenAPI document.
var probePaths = []string{
	"/openapi.json", "/openapi.yaml", "/openapi.yml", "/swagger.json", "/swagger.yaml",
	"/v3/api-docs", "/v2/api-docs", "/api-docs", "/api-docs.json", "/swagger/v1/swagger.json",
	"/swagger/doc.json", "/api/openapi.json", "/api/swagger.json", "/docs/openapi.json",
	"/api/v1/openapi.json", "/openapi",
}

// FetchSpec finds the OpenAPI document of a running server. raw may point at
// the document itself or at the server, in which case common locations are
// tried. It returns the API and the URL the document was found at.
func FetchSpec(ctx context.Context, client *http.Client, raw string) (*model.API, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("invalid URL %q", raw)
	}
	var candidates []string
	lower := strings.ToLower(u.Path)
	if strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") ||
		strings.Contains(lower, "openapi") || strings.Contains(lower, "swagger") || strings.Contains(lower, "api-docs") {
		candidates = append(candidates, u.String())
	} else {
		bases := []string{strings.TrimRight(u.Path, "/")}
		if bases[0] != "" {
			bases = append(bases, "")
		}
		for _, base := range bases {
			for _, p := range probePaths {
				c := *u
				c.Path, c.RawQuery = base+p, ""
				candidates = append(candidates, c.String())
			}
		}
	}
	for _, c := range candidates {
		if api, err := fetchOne(ctx, client, c); err == nil {
			return api, c, nil
		}
	}
	return nil, "", fmt.Errorf("no OpenAPI document found at %s (tried %d locations). Point ReplayHarness at a spec file or a source tree instead, or record real traffic with `replayharness record`", raw, len(candidates))
}

func fetchOne(ctx context.Context, client *http.Client, u string) (*model.API, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/yaml;q=0.9, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return ParseSpec(data, u)
}
