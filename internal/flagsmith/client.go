// Package flagsmith is a small client for the parts of the Flagsmith API that
// Flagbreaker uses. Every endpoint here exists in open-source, self-hosted
// Flagsmith; none needs an Enterprise licence.
//
// It uses plain net/http on purpose. The official Go admin client calls the
// older feature-state endpoints, which return 400 when an environment uses
// feature versioning v2. The /api/__future__ flag endpoint used here works
// with and without versioning.
package flagsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrChangeRequestsEnabled means the environment requires change requests
// (four-eyes approval), so the flag can't be changed directly. Flagbreaker
// never tries to get around that.
var ErrChangeRequestsEnabled = errors.New("the environment requires change requests, so the flag can't be changed directly")

// ErrNotFound means Flagsmith returned 404.
var ErrNotFound = errors.New("not found in Flagsmith")

// StatusError is any other error response from Flagsmith.
type StatusError struct {
	Method, Path string
	Code         int
	Body         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: Flagsmith returned %d: %s", e.Method, e.Path, e.Code, truncate(e.Body, 300))
}

// IsRejected reports whether Flagsmith refused a change as such (400, or a
// 409 other than change requests), for example a change to an experiment's
// system-segment override. A timeout or a 5xx is not a rejection: the same
// request may succeed later.
func IsRejected(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Code == http.StatusBadRequest || se.Code == http.StatusConflict)
}

// Client talks to one Flagsmith instance.
type Client struct {
	// BaseURL is where the API lives, without /api/v1, for example
	// http://flagsmith-api.flagsmith:8000.
	BaseURL string
	// APIKey is an organisation API key, sent as "Authorization: Api-Key".
	APIKey string
	// HealthWebhookPath is the secret path of the project's Feature Health
	// "Webhook" provider, starting with /api/v1/feature-health/.
	HealthWebhookPath string
	// HTTP is the client used for requests. nil means a 10 second timeout.
	HTTP *http.Client
}

// Environment is the part of a Flagsmith environment Flagbreaker needs.
type Environment struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ProjectID int64  `json:"project"`
}

// Value is a typed flag value, as the __future__ endpoint returns it.
type Value struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// State is what a flag serves somewhere: the environment default or a
// segment override.
type State struct {
	Enabled bool   `json:"enabled"`
	Value   *Value `json:"value"`
}

// SegmentOverride is what a flag serves to one segment.
type SegmentOverride struct {
	Segment  struct{ ID int64 } `json:"segment"`
	Priority int                `json:"priority"`
	Enabled  bool               `json:"enabled"`
	Value    *Value             `json:"value"`
}

// Flag is what a feature serves in one environment.
type Flag struct {
	EnvironmentDefault State             `json:"environment_default"`
	SegmentOverrides   []SegmentOverride `json:"segment_overrides"`
}

// EnabledSegmentIDs lists the segment overrides that are switched on.
func (f Flag) EnabledSegmentIDs() []int64 {
	var ids []int64
	for _, o := range f.SegmentOverrides {
		if o.Enabled {
			ids = append(ids, o.Segment.ID)
		}
	}
	return ids
}

// Environment looks up an environment by its client-side key.
func (c *Client) Environment(ctx context.Context, environmentKey string) (Environment, error) {
	var env Environment
	err := c.do(ctx, http.MethodGet, "/api/v1/environments/"+url.PathEscape(environmentKey)+"/", nil, &env)
	return env, err
}

// FeatureID finds a feature's ID by its exact name.
func (c *Client) FeatureID(ctx context.Context, projectID int64, name string) (int64, error) {
	var page struct {
		Results []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"results"`
	}
	path := fmt.Sprintf("/api/v1/projects/%d/features/?search=%s", projectID, url.QueryEscape(name))
	if err := c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
		return 0, err
	}
	// search matches substrings, so pick the exact name.
	for _, f := range page.Results {
		if f.Name == name {
			return f.ID, nil
		}
	}
	return 0, fmt.Errorf("feature %q: %w", name, ErrNotFound)
}

// GetFlag reads what a feature serves in an environment.
func (c *Client) GetFlag(ctx context.Context, environmentKey string, featureID int64) (Flag, error) {
	var flag Flag
	err := c.do(ctx, http.MethodGet, flagPath(environmentKey, featureID), nil, &flag)
	return flag, err
}

// SwitchOff turns off the environment default and the given segment
// overrides, in one versioned change. Overrides not listed are left alone.
// It can't change identity overrides or experiment (system segment)
// overrides; the caller reports those as still exposed.
func (c *Client) SwitchOff(ctx context.Context, environmentKey string, featureID int64, segmentIDs []int64) (Flag, error) {
	off := false
	return c.UpdateFlag(ctx, environmentKey, featureID, Update{Default: &off, Segments: segmentIDs})
}

// Update is a change to a flag's enabled state.
type Update struct {
	// Default, when set, is the new enabled state of the environment default.
	Default *bool
	// Segments are the segment overrides to change. Flagsmith creates an
	// override that doesn't exist, so list only existing ones.
	Segments []int64
	// SegmentsEnabled is the new enabled state of those overrides.
	SegmentsEnabled bool
}

// UpdateFlag applies an Update in one versioned change. Anything it doesn't
// list is left alone.
func (c *Client) UpdateFlag(ctx context.Context, environmentKey string, featureID int64, u Update) (Flag, error) {
	type segmentRef struct {
		ID int64 `json:"id"`
	}
	type overrideChange struct {
		Segment segmentRef `json:"segment"`
		Enabled bool       `json:"enabled"`
	}
	type state struct {
		Enabled bool `json:"enabled"`
	}
	body := struct {
		EnvironmentDefault *state           `json:"environment_default,omitempty"`
		SegmentOverrides   []overrideChange `json:"segment_overrides,omitempty"`
	}{}
	if u.Default != nil {
		body.EnvironmentDefault = &state{Enabled: *u.Default}
	}
	for _, id := range u.Segments {
		body.SegmentOverrides = append(body.SegmentOverrides, overrideChange{Segment: segmentRef{ID: id}, Enabled: u.SegmentsEnabled})
	}
	var flag Flag
	if err := c.do(ctx, http.MethodPatch, flagPath(environmentKey, featureID), body, &flag); err != nil {
		return flag, err
	}
	// Flagsmith answers with the flag as it now is. Trust that, not the 200.
	if u.Default != nil && flag.EnvironmentDefault.Enabled != *u.Default {
		return flag, fmt.Errorf("PATCH %s: Flagsmith answered, but the environment default is still enabled=%v", flagPath(environmentKey, featureID), flag.EnvironmentDefault.Enabled)
	}
	return flag, nil
}

// HealthStatus is a Feature Health status.
type HealthStatus string

const (
	Healthy   HealthStatus = "healthy"
	Unhealthy HealthStatus = "unhealthy"
)

// TextBlock is a paragraph of a Feature Health reason.
type TextBlock struct {
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
}

// URLBlock is a link in a Feature Health reason.
type URLBlock struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url"`
}

// HealthEvent is one event for the Feature Health "Webhook" provider.
type HealthEvent struct {
	Feature string       `json:"feature"`
	Status  HealthStatus `json:"status"`
	// Environment must match an environment name exactly. Flagsmith stores an
	// unknown name as an event for no environment, which Release Pipelines
	// treat as every environment, so callers must pass a name they looked up.
	Environment string `json:"environment"`
	Reason      struct {
		// Flagsmith 2.295 rejects a reason without both lists, so both are
		// always sent, even when empty.
		TextBlocks []TextBlock `json:"text_blocks"`
		URLBlocks  []URLBlock  `json:"url_blocks"`
	} `json:"reason"`
}

// ReportHealth posts an event to the project's Feature Health webhook.
// Flagsmith answers 400 when it creates no event, for example when no feature
// has that name.
func (c *Client) ReportHealth(ctx context.Context, event HealthEvent) error {
	if c.HealthWebhookPath == "" {
		return errors.New("no Feature Health webhook path configured")
	}
	if event.Reason.TextBlocks == nil {
		event.Reason.TextBlocks = []TextBlock{}
	}
	if event.Reason.URLBlocks == nil {
		event.Reason.URLBlocks = []URLBlock{}
	}
	// The path is the credential, so no API key is sent.
	return c.doWithAuth(ctx, http.MethodPost, c.HealthWebhookPath, event, nil, false)
}

// Provider names Flagsmith uses for Feature Health events.
const (
	ProviderWebhook = "Webhook"
	ProviderGrafana = "Grafana"
)

// DismissedPrefix starts the reason of the Healthy event Flagsmith writes
// when a person dismisses an Unhealthy event in the dashboard.
const DismissedPrefix = "Manually dismissed by"

// HealthEventRecord is a stored Feature Health event.
type HealthEventRecord struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// Environment is the environment's ID, or nil for every environment.
	Environment  *int64 `json:"environment"`
	Feature      int64  `json:"feature"`
	ProviderName string `json:"provider_name"`
	// Type is UNHEALTHY or HEALTHY.
	Type   string `json:"type"`
	Reason *struct {
		TextBlocks []TextBlock `json:"text_blocks"`
		URLBlocks  []URLBlock  `json:"url_blocks"`
	} `json:"reason"`
}

// Unhealthy reports whether the event marks the flag unhealthy.
func (e HealthEventRecord) Unhealthy() bool { return strings.EqualFold(e.Type, "unhealthy") }

// Title is the event's first text block title, or its provider name.
func (e HealthEventRecord) Title() string {
	if e.Reason != nil {
		for _, b := range e.Reason.TextBlocks {
			if b.Title != "" {
				return b.Title
			}
		}
	}
	return e.ProviderName
}

// FirstText is the text of the event's first text block.
func (e HealthEventRecord) FirstText() string {
	if e.Reason != nil && len(e.Reason.TextBlocks) > 0 {
		return e.Reason.TextBlocks[0].Text
	}
	return ""
}

// HealthEvents lists a project's latest Feature Health event per provider,
// environment, alert and feature. A resolved or dismissed alert shows up as
// a Healthy event in its slot.
func (c *Client) HealthEvents(ctx context.Context, projectID int64) ([]HealthEventRecord, error) {
	var events []HealthEventRecord
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/feature-health/events/", projectID), nil, &events)
	return events, err
}

func flagPath(environmentKey string, featureID int64) string {
	return fmt.Sprintf("/api/__future__/environments/%s/features/%d/", url.PathEscape(environmentKey), featureID)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithAuth(ctx, method, path, body, out, true)
}

func (c *Client) doWithAuth(ctx context.Context, method, path string, body, out any, auth bool) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return fmt.Errorf("%s %s: invalid request", method, redact(path))
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Api-Key "+c.APIKey)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// A *url.Error repeats the full URL, which for Feature Health holds
		// the secret path. Keep only its cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s %s: %w", method, redact(path), err)
	}
	defer resp.Body.Close()
	// Generous: the Feature Health events list is not paginated.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))

	switch {
	case resp.StatusCode == http.StatusConflict && isChangeRequestError(data):
		return ErrChangeRequestsEnabled
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w", method, redact(path), ErrNotFound)
	case resp.StatusCode >= 300:
		return &StatusError{Method: method, Path: redact(path), Code: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w", method, redact(path), err)
		}
	}
	return nil
}

// httpClient never follows a redirect for anything but GET: Go would turn a
// PATCH or POST into a GET on a 301, and a switch-off would quietly do
// nothing. The redirect comes back as an error instead.
func (c *Client) httpClient() *http.Client {
	base := c.HTTP
	if base == nil {
		base = &http.Client{Timeout: 10 * time.Second}
	}
	hc := *base
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && via[0].Method != http.MethodGet {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &hc
}

// isChangeRequestError recognises Flagsmith's ChangeRequestsEnabledError. DRF
// renders it as {"detail": "Cannot make this change where change requests
// are enabled."}; newer versions may also include the code.
func isChangeRequestError(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "change_requests_enabled") || strings.Contains(s, "change requests are enabled")
}

// redact hides the secret part of a Feature Health webhook path in errors.
func redact(path string) string {
	if strings.HasPrefix(path, "/api/v1/feature-health/") {
		return "/api/v1/feature-health/<redacted>"
	}
	return path
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
