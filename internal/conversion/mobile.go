package conversion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Mobile is explicitly opt-in. It never grants access to the authenticated API.
// Installation and tutorial events are client observations, not device attestation.
type MobileConfig struct {
	BundleID   string   `json:"bundle_id"`
	ProductIDs []string `json:"product_ids"`
}

type MobileEvent struct {
	InstallationID string          `json:"installation_id"`
	EventName      string          `json:"event_name"`
	Timestamp      int64           `json:"conversion_timestamp"`
	Consent        bool            `json:"consent"`
	ATTStatus      string          `json:"att_status,omitempty"`
	DeviceID       string          `json:"device_id,omitempty"`
	Purchase       *MobilePurchase `json:"purchase,omitempty"`
}

type MobilePurchase struct {
	ProductID         string `json:"product_id"`
	PurchaseToken     string `json:"purchase_token,omitempty"`
	SignedTransaction string `json:"signed_transaction,omitempty"`
}

type VerifiedPurchase struct {
	ID, ProductID, Currency string
	Timestamp               int64
	Value                   float64
	Quantity                int
}

type PurchaseVerifier interface {
	Verify(context.Context, App, MobilePurchase, time.Time) (VerifiedPurchase, error)
}

type rateEntry struct {
	start time.Time
	count int
}

type MobileAPI struct {
	Config   Configuration
	Store    *Store
	Verifier PurchaseVerifier
	proxies  []*net.IPNet
	mu       sync.Mutex
	rates    map[string]rateEntry
}

func NewMobileAPI(config Configuration, store *Store, trustedProxies string) (*MobileAPI, error) {
	m := &MobileAPI{Config: config, Store: store, Verifier: &StoreVerifier{}, rates: map[string]rateEntry{}}
	for _, raw := range strings.Split(trustedProxies, ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("CONVERSION_TRUSTED_PROXY_CIDRS contains an invalid CIDR")
		}
		m.proxies = append(m.proxies, cidr)
	}
	return m, nil
}

func (m *MobileAPI) trusted(ip net.IP) bool {
	for _, cidr := range m.proxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (m *MobileAPI) clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	peer := net.ParseIP(host)
	if !m.trusted(peer) {
		return peer
	}
	// Walk right to left; never accept the leftmost user-supplied value blindly.
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(forwarded) > 16 {
		return nil
	}
	for i := len(forwarded) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(forwarded[i]))
		if ip == nil {
			return nil
		}
		if !m.trusted(ip) {
			return ip
		}
	}
	return nil
}

func (m *MobileAPI) allow(ip string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rates == nil {
		m.rates = map[string]rateEntry{}
	}
	if len(m.rates) >= 10000 {
		for key, value := range m.rates {
			if now.Sub(value.start) >= time.Hour {
				delete(m.rates, key)
			}
		}
	}
	e, ok := m.rates[ip]
	if !ok && len(m.rates) >= 10000 {
		return false
	}
	if now.Sub(e.start) >= time.Hour {
		e = rateEntry{start: now}
	}
	e.count++
	m.rates[ip] = e
	return e.count <= 120
}

func (m *MobileAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] != "mobile" || parts[2] != "apps" || parts[4] != "events" {
		fail(w, 404, "not_found")
		return
	}
	app, ok := m.Config.Find(parts[3])
	if !ok || app.Mobile == nil {
		fail(w, 404, "mobile_not_enabled")
		return
	}
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		fail(w, 415, "use_application_json")
		return
	}
	ip, now := m.clientIP(r), time.Now().UTC()
	if ip == nil {
		fail(w, 503, "client_ip_unavailable")
		return
	}
	if !m.allow(ip.String(), now) {
		w.Header().Set("Retry-After", "3600")
		fail(w, 429, "rate_limited")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var input MobileEvent
	if decodeJSON(r.Body, &input) != nil {
		fail(w, 400, "invalid_json")
		return
	}
	if !input.Consent {
		fail(w, 422, "consent_required")
		return
	}
	if !devicePattern.MatchString(input.InstallationID) || input.InstallationID == "00000000-0000-0000-0000-000000000000" {
		fail(w, 422, "invalid_installation_id")
		return
	}
	if input.EventName != "INSTALL" && input.EventName != "TUTORIAL_COMPLETE" && input.EventName != "PURCHASE" {
		fail(w, 422, "unsupported_event")
		return
	}
	// Reject before expensive receipt verification, including when an event is disabled.
	allowed := false
	for _, name := range app.Events {
		if name == input.EventName {
			allowed = true
		}
	}
	if !allowed {
		fail(w, 422, "event_disabled")
		return
	}
	limited := false
	event := Event{EventName: input.EventName, ConversionTimestamp: input.Timestamp, LimitAdTracking: &limited}
	if app.OSType == "IOS" {
		if input.ATTStatus != "authorized" || input.DeviceID == "" {
			fail(w, 422, "att_authorization_required")
			return
		}
		event.ATTStatus = input.ATTStatus
		event.Identifiers.DeviceID = input.DeviceID
	} else {
		// Never send the reverse proxy's private IP as the user's identifier.
		if !ip.IsGlobalUnicast() || ip.IsPrivate() {
			fail(w, 503, "configure_trusted_proxy")
			return
		}
		event.Identifiers.IPAddress, event.Identifiers.UserAgent = ip.String(), r.UserAgent()
	}
	id := "mobile:" + strings.ToLower(input.InstallationID) + ":" + input.EventName
	if input.EventName == "PURCHASE" {
		if input.Purchase == nil {
			fail(w, 422, "purchase_required")
			return
		}
		productAllowed := false
		for _, product := range app.Mobile.ProductIDs {
			if input.Purchase.ProductID == product {
				productAllowed = true
			}
		}
		if !productAllowed {
			fail(w, 422, "product_not_allowed")
			return
		}
		purchase, err := m.Verifier.Verify(r.Context(), app, *input.Purchase, now)
		if err != nil {
			if err == ErrVerificationUnavailable {
				fail(w, 503, "purchase_verification_unavailable")
			} else {
				fail(w, 422, "purchase_not_verified")
			}
			return
		}
		digest := sha256.Sum256([]byte(purchase.ID))
		id = "mobile:purchase:" + hex.EncodeToString(digest[:])
		event.ConversionTimestamp, event.Value, event.PriceCurrency = purchase.Timestamp, &purchase.Value, purchase.Currency
		event.NumberItems = purchase.Quantity
		event.Contents = []Content{{ContentID: purchase.ProductID, ContentType: "product", NumItems: purchase.Quantity}}
	} else if input.Purchase != nil {
		fail(w, 422, "unexpected_purchase")
		return
	}
	event.EventID = id
	payload, err := event.Normalize(app, now)
	if err != nil {
		fail(w, 422, "invalid_event_or_identifiers")
		return
	}
	if _, err := m.Store.EnqueueMobile(app.Key, id, payload, now); err != nil {
		fail(w, 503, "outbox_unavailable")
		return
	}
	// No event status, identifiers or receipt data exposed through the mobile API.
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}
