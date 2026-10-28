package conversion

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"regexp"
	"strings"
	"time"
)

const MaxBatch = 500

// Event is submitted only by trusted producers after confirming the underlying action.
// EventID is a stable producer key, not an identifier used for matching by X.
type Event struct {
	EventID             string      `json:"event_id"`
	EventName           string      `json:"event_name"`
	ConversionTimestamp int64       `json:"conversion_timestamp"`
	Identifiers         Identifiers `json:"identifiers"`
	LimitAdTracking     *bool       `json:"limit_ad_tracking,omitempty"`
	ATTStatus           string      `json:"att_status,omitempty"`
	Value               *float64    `json:"value,omitempty"`
	PriceCurrency       string      `json:"price_currency,omitempty"`
	NumberItems         int         `json:"number_items,omitempty"`
	Contents            []Content   `json:"contents,omitempty"`
}

type Identifiers struct {
	DeviceID          string `json:"device_id,omitempty"`
	Email             string `json:"email,omitempty"`
	PhoneNumber       string `json:"phone_number,omitempty"`
	HashedEmail       string `json:"hashed_email,omitempty"`
	HashedPhoneNumber string `json:"hashed_phone_number,omitempty"`
	IPAddress         string `json:"ip_address,omitempty"`
	UserAgent         string `json:"user_agent,omitempty"`
}

type Content struct {
	ContentID   string `json:"content_id"`
	ContentType string `json:"content_type"`
	NumItems    int    `json:"num_items"`
}

type Conversion struct {
	AppID               string              `json:"app_id"`
	OSType              string              `json:"os_type"`
	EventName           string              `json:"event_name"`
	ConversionTimestamp int64               `json:"conversion_timestamp"`
	Identifiers         []map[string]string `json:"identifiers"`
	Value               *float64            `json:"value,omitempty"`
	PriceCurrency       string              `json:"price_currency,omitempty"`
	NumberItems         int                 `json:"number_items,omitempty"`
	Contents            []Content           `json:"contents,omitempty"`
	LimitAdTracking     bool                `json:"limit_ad_tracking"`
}

var eventIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)
var devicePattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hashPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func (e Event) Normalize(app App, now time.Time) (Conversion, error) {
	var result Conversion
	if !eventIDPattern.MatchString(e.EventID) {
		return result, fmt.Errorf("event_id must be 1–200 letters, digits, dots, underscores, colons or hyphens")
	}
	allowed := false
	for _, name := range app.Events {
		allowed = allowed || name == e.EventName
	}
	if !allowed {
		return result, fmt.Errorf("event_name is not enabled for this app")
	}
	if e.ConversionTimestamp < 946684800000 || e.ConversionTimestamp > now.Add(5*time.Minute).UnixMilli() {
		return result, fmt.Errorf("conversion_timestamp must be Unix milliseconds, no more than 5 minutes in the future")
	}
	var limited bool
	if app.OSType == "IOS" {
		// All iOS producers must report ATT. This endpoint does identifier-based matching,
		// so non-authorized users must use an aggregate attribution flow outside this API.
		switch e.ATTStatus {
		case "authorized":
			limited = false
		case "denied", "restricted", "not_determined":
			return result, fmt.Errorf("iOS identifier-based conversions require ATT authorization; use aggregate attribution otherwise")
		default:
			return result, fmt.Errorf("att_status is required for iOS")
		}
	} else {
		if e.LimitAdTracking == nil {
			return result, fmt.Errorf("limit_ad_tracking must be explicitly provided for Android")
		}
		limited = *e.LimitAdTracking
	}
	ids, err := e.Identifiers.Normalize(limited)
	if err != nil {
		return result, err
	}
	currency := strings.ToUpper(strings.TrimSpace(e.PriceCurrency))
	if e.Value != nil {
		if math.IsInf(*e.Value, 0) || math.IsNaN(*e.Value) || *e.Value < 0 || !currencyPattern.MatchString(currency) {
			return result, fmt.Errorf("value must be finite and nonnegative with a three-letter price_currency")
		}
	} else if currency != "" {
		return result, fmt.Errorf("price_currency requires value")
	}
	if e.EventName == "PURCHASE" && (e.Value == nil || e.NumberItems <= 0 || len(e.Contents) == 0) {
		return result, fmt.Errorf("PURCHASE requires verified value, price_currency, number_items and contents")
	}
	if e.NumberItems < 0 || len(e.Contents) > 500 {
		return result, fmt.Errorf("invalid number_items or too many contents")
	}
	for _, content := range e.Contents {
		if strings.TrimSpace(content.ContentID) == "" || strings.TrimSpace(content.ContentType) == "" || content.NumItems < 1 {
			return result, fmt.Errorf("each content requires content_id, content_type and positive num_items")
		}
	}
	return Conversion{AppID: app.AppID, OSType: app.OSType, EventName: e.EventName,
		ConversionTimestamp: e.ConversionTimestamp, Identifiers: ids, LimitAdTracking: limited,
		Value: e.Value, PriceCurrency: currency, NumberItems: e.NumberItems, Contents: e.Contents}, nil
}

func (i Identifiers) Normalize(limited bool) ([]map[string]string, error) {
	var result []map[string]string
	add := func(key, value string) { result = append(result, map[string]string{key: value}) }
	device := strings.TrimSpace(i.DeviceID)
	if device != "" {
		if !devicePattern.MatchString(device) || device == "00000000-0000-0000-0000-000000000000" {
			return nil, fmt.Errorf("device_id must be a raw nonzero advertising UUID")
		}
		if limited {
			return nil, fmt.Errorf("device_id cannot be used when limit_ad_tracking is true")
		}
		add("device_id", device)
	}
	for _, field := range []struct{ raw, hashed, key string }{
		{i.Email, i.HashedEmail, "hashed_email"},
		{i.PhoneNumber, i.HashedPhoneNumber, "hashed_phone_number"},
	} {
		if field.raw != "" && field.hashed != "" {
			return nil, fmt.Errorf("provide either raw or hashed %s, not both", field.key)
		}
		if field.hashed != "" {
			if !hashPattern.MatchString(field.hashed) {
				return nil, fmt.Errorf("%s must be a 64-character SHA-256 hex digest", field.key)
			}
			add(field.key, strings.ToLower(field.hashed))
		} else if field.raw != "" {
			value := strings.ToLower(strings.TrimSpace(field.raw))
			if field.key == "hashed_phone_number" {
				// Require an explicit country code; guessing from a local number is unsafe.
				value = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "").Replace(value)
				if !phonePattern.MatchString(value) {
					return nil, fmt.Errorf("phone_number must include a country code in E.164 form, e.g. +15551234567")
				}
			} else if strings.Count(value, "@") != 1 || strings.ContainsAny(value, " \t\r\n") || strings.HasPrefix(value, "@") || strings.HasSuffix(value, "@") {
				return nil, fmt.Errorf("invalid email")
			}
			hash := sha256.Sum256([]byte(value))
			add(field.key, hex.EncodeToString(hash[:]))
		}
	}
	ip, ua := strings.TrimSpace(i.IPAddress), strings.TrimSpace(i.UserAgent)
	if (ip == "") != (ua == "") {
		return nil, fmt.Errorf("ip_address and user_agent must be provided together")
	}
	if ip != "" {
		if net.ParseIP(ip) == nil || len(ua) > 1024 || strings.ContainsAny(ua, "\r\n") {
			return nil, fmt.Errorf("invalid ip_address or user_agent")
		}
		add("ip_address", ip)
		add("user_agent", ua)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("provide device_id, hashed_email, hashed_phone_number, or both ip_address and user_agent")
	}
	return result, nil
}
