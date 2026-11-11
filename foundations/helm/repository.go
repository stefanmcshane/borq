package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/repo/v1"
	"sigs.k8s.io/yaml"
)

// RepositoryChart lists the exact versions published for one installable chart.
type RepositoryChart struct {
	// Name is the published chart name, without a local repository alias.
	Name string `json:"name"`
	// Versions contains exact chart versions, newest first.
	Versions []string `json:"versions"`
}

// ListCharts reads a public HTTP(S) repository index without changing Helm's local repositories.
func ListCharts(ctx context.Context, repository string) ([]RepositoryChart, error) {
	location, err := url.Parse(strings.TrimSpace(repository))
	if err != nil || location.Host == "" || location.User != nil || location.RawQuery != "" || location.Fragment != "" || (location.Scheme != "https" && location.Scheme != "http") {
		return nil, errors.New("Enter an HTTP(S) Helm repository URL without credentials, query, or fragment.")
	}
	location.Path = strings.TrimRight(location.Path, "/") + "/index.yaml"
	location.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location.String(), nil)
	if err != nil {
		return nil, errors.New("The repository URL could not be read.")
	}
	client := http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("Could not reach the chart repository. Check the URL and try again.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("The repository returned HTTP %d for index.yaml. Check the repository URL.", response.StatusCode)
	}
	const maximumIndexSize = 32 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumIndexSize+1))
	if err != nil {
		return nil, errors.New("The repository index could not be downloaded. Try again.")
	}
	if len(data) > maximumIndexSize {
		return nil, errors.New("This repository index is too large to browse. Enter the chart and version manually.")
	}
	var index repo.IndexFile
	if err := yaml.Unmarshal(data, &index); err != nil || index.APIVersion != "v1" || index.Entries == nil {
		return nil, errors.New("The repository did not return a valid Helm index.yaml.")
	}
	chartName := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	exactVersion := regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	for name, entries := range index.Entries {
		index.Entries[name] = slices.DeleteFunc(entries, func(entry *repo.ChartVersion) bool {
			return entry == nil || entry.Metadata == nil || entry.Name != name || !exactVersion.MatchString(entry.Version) || len(entry.URLs) == 0 || entry.Type == "library"
		})
	}
	index.SortEntries()
	charts := []RepositoryChart{}
	for name, entries := range index.Entries {
		if !chartName.MatchString(name) {
			continue
		}
		chart := RepositoryChart{Name: name, Versions: []string{}}
		for _, entry := range entries {
			if !slices.Contains(chart.Versions, entry.Version) {
				chart.Versions = append(chart.Versions, entry.Version)
			}
		}
		if len(chart.Versions) != 0 {
			charts = append(charts, chart)
		}
	}
	slices.SortFunc(charts, func(left, right RepositoryChart) int { return strings.Compare(left.Name, right.Name) })
	return charts, nil
}
