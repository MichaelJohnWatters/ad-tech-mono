package fraud

import (
	"context"
	"io"
	"net/http"
)

// AppAdsTxtRecord is a persisted app-ads.txt result for one app developer
// domain. Mirrors the app_ads_txt_cache table. The line format is identical to
// ads.txt, so it reuses AdsTxtEntry / ParseAdsTxt.
type AppAdsTxtRecord struct {
	DeveloperDomain string
	Entries         []AdsTxtEntry
	Status          string // valid | missing | error
}

// FetchAppAdsTxt GETs https://{developerDomain}/app-ads.txt and parses it.
// app-ads.txt (IAB Tech Lab) is the in-app counterpart of ads.txt and is served
// from the app developer's website domain — the same one listed in the app
// store — not from the app bundle id. Status mirrors FetchAdsTxt: "valid"
// (HTTP 200 + parsed), "missing" (404 — a legitimate state), or "error"
// (network failure / unexpected status). The crawler (cmd/appadstxt) upserts it.
func FetchAppAdsTxt(ctx context.Context, client *http.Client, developerDomain string) AppAdsTxtRecord {
	url := "https://" + developerDomain + "/app-ads.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return AppAdsTxtRecord{DeveloperDomain: developerDomain, Status: "error"}
	}
	resp, err := client.Do(req)
	if err != nil {
		return AppAdsTxtRecord{DeveloperDomain: developerDomain, Status: "error"}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return AppAdsTxtRecord{DeveloperDomain: developerDomain, Status: "missing"}
	case resp.StatusCode != http.StatusOK:
		return AppAdsTxtRecord{DeveloperDomain: developerDomain, Status: "error"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap per IAB guidance
	if err != nil {
		return AppAdsTxtRecord{DeveloperDomain: developerDomain, Status: "error"}
	}
	return AppAdsTxtRecord{DeveloperDomain: developerDomain, Entries: ParseAdsTxt(string(body)), Status: "valid"}
}
