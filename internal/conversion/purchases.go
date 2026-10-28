package conversion

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrVerificationUnavailable = errors.New("purchase verification unavailable")
var errInvalidPurchase = errors.New("invalid purchase")

// StoreVerifier never logs credentials, purchase tokens, API bodies or signed transactions.
type StoreVerifier struct {
	Client      *http.Client
	mu          sync.Mutex
	accessToken string
	expires     time.Time
	// Private overrides are used only by package tests, not deployment configuration.
	googleBase, oauthURL string
	appleRoots           *x509.CertPool
}

func (v *StoreVerifier) client() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (v *StoreVerifier) Verify(ctx context.Context, app App, p MobilePurchase, now time.Time) (VerifiedPurchase, error) {
	if app.OSType == "IOS" {
		if p.PurchaseToken != "" {
			return VerifiedPurchase{}, errInvalidPurchase
		}
		return v.verifyApple(app, p, now)
	}
	if p.SignedTransaction != "" || len(p.PurchaseToken) < 10 || len(p.PurchaseToken) > 8192 {
		return VerifiedPurchase{}, errInvalidPurchase
	}
	return v.verifyGoogle(ctx, app, p, now)
}

func (v *StoreVerifier) googleAccessToken(ctx context.Context, now time.Time) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.accessToken != "" && now.Before(v.expires.Add(-time.Minute)) {
		return v.accessToken, nil
	}
	var account struct {
		Email string `json:"client_email"`
		Key   string `json:"private_key"`
	}
	raw := os.Getenv("GOOGLE_PLAY_SERVICE_ACCOUNT_JSON")
	if raw == "" || json.Unmarshal([]byte(raw), &account) != nil || account.Email == "" {
		return "", ErrVerificationUnavailable
	}
	block, _ := pem.Decode([]byte(account.Key))
	if block == nil {
		return "", ErrVerificationUnavailable
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", ErrVerificationUnavailable
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return "", ErrVerificationUnavailable
	}
	endpoint := v.oauthURL
	if endpoint == "" {
		endpoint = "https://oauth2.googleapis.com/token"
	}
	claims, _ := json.Marshal(map[string]interface{}{"iss": account.Email, "scope": "https://www.googleapis.com/auth/androidpublisher", "aud": endpoint, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", ErrVerificationUnavailable
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client().Do(req)
	if err != nil {
		return "", ErrVerificationUnavailable
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token) != nil || token.AccessToken == "" || token.ExpiresIn < 120 {
		return "", ErrVerificationUnavailable
	}
	v.accessToken, v.expires = token.AccessToken, now.Add(time.Duration(token.ExpiresIn)*time.Second)
	return token.AccessToken, nil
}

func (v *StoreVerifier) googleGet(ctx context.Context, path, token string, dest interface{}) error {
	base := v.googleBase
	if base == "" {
		base = "https://androidpublisher.googleapis.com/androidpublisher/v3/applications/"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.client().Do(req)
	if err != nil {
		return ErrVerificationUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == 400 || resp.StatusCode == 410 {
		return errInvalidPurchase
	}
	// 404 can occur briefly after purchase; retry rather than lose a real payment.
	if resp.StatusCode != 200 {
		return ErrVerificationUnavailable
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dest) != nil {
		return ErrVerificationUnavailable
	}
	return nil
}

func (v *StoreVerifier) verifyGoogle(ctx context.Context, app App, p MobilePurchase, now time.Time) (VerifiedPurchase, error) {
	var result VerifiedPurchase
	token, err := v.googleAccessToken(ctx, now)
	if err != nil {
		return result, err
	}
	var purchase struct {
		Timestamp string `json:"purchaseTimeMillis"`
		State     *int   `json:"purchaseState"`
		Type      *int   `json:"purchaseType"`
		OrderID   string `json:"orderId"`
		ProductID string `json:"productId"`
		Quantity  int    `json:"quantity"`
	}
	prefix := url.PathEscape(app.Mobile.BundleID)
	err = v.googleGet(ctx, prefix+"/purchases/products/"+url.PathEscape(p.ProductID)+"/tokens/"+url.PathEscape(p.PurchaseToken), token, &purchase)
	if err != nil {
		return result, err
	}
	if purchase.State != nil && *purchase.State == 2 {
		return result, ErrVerificationUnavailable
	}
	if purchase.State == nil || *purchase.State != 0 || purchase.Type != nil || purchase.OrderID == "" || (purchase.ProductID != "" && purchase.ProductID != p.ProductID) {
		return result, errInvalidPurchase
	}
	var order struct {
		ID    string `json:"orderId"`
		Token string `json:"purchaseToken"`
		State string `json:"state"`
		Total *struct {
			Currency string `json:"currencyCode"`
			Units    string `json:"units"`
			Nanos    int64  `json:"nanos"`
		} `json:"total"`
		Items []struct {
			ProductID string `json:"productId"`
		} `json:"lineItems"`
	}
	if err := v.googleGet(ctx, prefix+"/orders/"+url.PathEscape(purchase.OrderID), token, &order); err != nil {
		return result, err
	}
	if order.State == "PENDING" {
		return result, ErrVerificationUnavailable
	}
	if order.ID != purchase.OrderID || order.Token != p.PurchaseToken || order.State != "PROCESSED" || order.Total == nil || len(order.Items) != 1 || order.Items[0].ProductID != p.ProductID {
		return result, errInvalidPurchase
	}
	units := int64(0)
	if order.Total.Units != "" {
		units, err = strconv.ParseInt(order.Total.Units, 10, 64)
	}
	if err != nil || units < 0 || order.Total.Nanos < 0 || order.Total.Nanos >= 1000000000 {
		return result, errInvalidPurchase
	}
	ts, err := strconv.ParseInt(purchase.Timestamp, 10, 64)
	if err != nil || ts < 946684800000 || ts > now.Add(5*time.Minute).UnixMilli() {
		return result, errInvalidPurchase
	}
	quantity := purchase.Quantity
	if quantity == 0 {
		quantity = 1
	}
	if quantity < 1 || !currencyPattern.MatchString(order.Total.Currency) {
		return result, errInvalidPurchase
	}
	return VerifiedPurchase{ID: purchase.OrderID, ProductID: p.ProductID, Currency: order.Total.Currency, Timestamp: ts, Value: float64(units) + float64(order.Total.Nanos)/1e9, Quantity: quantity}, nil
}
