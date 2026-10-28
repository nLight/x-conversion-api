package conversion

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const xBaseURL = "https://ads-api.x.com/12/accounts/"

type Delivery struct {
	State      string
	Code       string
	HTTPStatus int
	DebugID    string
	RetryAfter time.Duration
}

type XClient struct {
	http    *http.Client
	baseURL string
}

func NewXClient() *XClient {
	return &XClient{http: &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, baseURL: xBaseURL}
}

var debugPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// Send never retries a request whose outcome might already have been accepted by X.
// Local event_id deduplication cannot guarantee deduplication at an external API.
func (x *XClient) Send(ctx context.Context, app App, conversions []Conversion) Delivery {
	if len(conversions) < 1 || len(conversions) > MaxBatch {
		return Delivery{State: "failed", Code: "invalid_batch_size"}
	}
	for _, conversion := range conversions {
		if conversion.AppID != app.AppID || conversion.OSType != app.OSType {
			return Delivery{State: "failed", Code: "app_configuration_mismatch"}
		}
	}
	token := strings.TrimSpace(os.Getenv(app.XTokenEnv))
	if token == "" {
		return Delivery{State: "pending", Code: "x_token_missing", RetryAfter: time.Minute}
	}
	body, err := json.Marshal(struct {
		Conversions []Conversion `json:"conversions"`
	}{conversions})
	if err != nil {
		return Delivery{State: "failed", Code: "invalid_conversion"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.baseURL+app.XAccountID+"/measurement/app_conversions", bytes.NewReader(body))
	if err != nil {
		return Delivery{State: "failed", Code: "invalid_x_request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-App-Conversion-Token", token)
	response, err := x.http.Do(req)
	if err != nil {
		return Delivery{State: "uncertain", Code: "x_transport_error"}
	}
	defer response.Body.Close()
	result := Delivery{HTTPStatus: response.StatusCode}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		result.State, result.Code = "pending", "x_rejected_retryable"
		result.RetryAfter = 5 * time.Minute
		if response.StatusCode == http.StatusTooManyRequests {
			result.RetryAfter = time.Minute
		}
		if delay, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && delay > 0 {
			result.RetryAfter = time.Duration(delay) * time.Second
		} else if when, err := http.ParseTime(response.Header.Get("Retry-After")); err == nil && time.Until(when) > 0 {
			result.RetryAfter = time.Until(when)
		}
		return result
	}
	if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnprocessableEntity {
		result.State, result.Code = "failed", "x_rejected"
		return result
	}
	if response.StatusCode != http.StatusOK {
		result.State, result.Code = "uncertain", "x_unexpected_status"
		return result
	}
	var acknowledgement struct {
		Data struct {
			Processed *int   `json:"conversions_processed"`
			DebugID   string `json:"debug_id"`
		} `json:"data"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&acknowledgement)
	if debugPattern.MatchString(acknowledgement.Data.DebugID) {
		result.DebugID = acknowledgement.Data.DebugID
	}
	if decodeErr != nil || acknowledgement.Data.Processed == nil || *acknowledgement.Data.Processed != len(conversions) {
		result.State, result.Code = "uncertain", "x_incomplete_acknowledgement"
		return result
	}
	result.State = "sent"
	return result
}

type Dispatcher struct {
	Config Configuration
	Store  *Store
	Client *XClient
}

func (d *Dispatcher) DispatchOnce(ctx context.Context, now time.Time) error {
	for _, app := range d.Config.Apps {
		if ctx.Err() != nil {
			return nil
		}
		// Keep setup events pending without incrementing delivery attempts.
		if strings.TrimSpace(os.Getenv(app.XTokenEnv)) == "" {
			continue
		}
		records, err := d.Store.Claim(app.Key, now)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			continue
		}
		var payloads []Conversion
		for _, record := range records {
			payloads = append(payloads, *record.Payload)
		}
		result := d.Client.Send(ctx, app, payloads)
		if err := d.Store.Finish(records, result, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}
