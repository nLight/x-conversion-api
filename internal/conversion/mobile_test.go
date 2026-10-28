package conversion

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const mobileID = "11111111-2222-4333-8444-555555555555"

func mobileFixture(t *testing.T) (*MobileAPI, App) {
	t.Helper()
	app := App{Key: "example", AppID: "com.example.app", OSType: "ANDROID", Events: []string{"INSTALL", "TUTORIAL_COMPLETE", "PURCHASE"}, Mobile: &MobileConfig{BundleID: "com.example.app", ProductIDs: []string{"full"}}}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m, err := NewMobileAPI(Configuration{Apps: []App{app}}, store, "10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	return m, app
}

func mobileRequest(m *MobileAPI, input MobileEvent, peer, forwarded string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", "/v1/mobile/apps/example/events", bytes.NewReader(body))
	r.RemoteAddr = peer
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "Example/1.0 (Android)")
	r.Header.Set("X-Forwarded-For", forwarded)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

func TestMobileConsentProxyAndIdempotency(t *testing.T) {
	m, app := mobileFixture(t)
	e := MobileEvent{InstallationID: mobileID, EventName: "INSTALL", Timestamp: time.Now().UnixMilli()}
	if w := mobileRequest(m, e, "10.0.0.1:80", "203.0.113.8"); w.Code != 422 {
		t.Fatal(w.Code)
	}
	e.Consent = true
	if w := mobileRequest(m, e, "192.168.1.1:80", "203.0.113.8"); w.Code != 503 {
		t.Fatal("untrusted proxy accepted", w.Code)
	}
	if w := mobileRequest(m, e, "10.0.0.1:80", "203.0.113.8"); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	e.Timestamp++
	if w := mobileRequest(m, e, "10.0.0.1:80", "203.0.113.9"); w.Code != 202 {
		t.Fatal(w.Code)
	}
	claimed, err := m.Store.Claim(app.Key, time.Now().Add(time.Minute))
	if err != nil || len(claimed) != 1 {
		t.Fatal(len(claimed), err)
	}
	if claimed[0].Payload.Identifiers[0]["ip_address"] != "203.0.113.8" {
		t.Fatal("retry replaced the first observation")
	}
	// The public collector must not expose status/retry operations or trusted ingestion.
	api := &API{Config: m.Config, Store: m.Store, Mobile: m}
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("POST", "/v1/apps/example/events", strings.NewReader(`{}`)))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestMobileATTDisabledModeAndRateLimits(t *testing.T) {
	m, _ := mobileFixture(t)
	m.Config.Apps[0].OSType = "IOS"
	e := MobileEvent{InstallationID: mobileID, EventName: "INSTALL", Timestamp: time.Now().UnixMilli(), Consent: true, DeviceID: mobileID, ATTStatus: "denied"}
	if w := mobileRequest(m, e, "203.0.113.8:80", ""); w.Code != 422 {
		t.Fatal(w.Code)
	}
	e.ATTStatus = "authorized"
	if w := mobileRequest(m, e, "203.0.113.8:80", ""); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	m.Config.Apps[0].Mobile = nil
	if w := mobileRequest(m, e, "203.0.113.8:80", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	for i := 0; i < 120; i++ {
		if !m.allow("test", time.Now()) {
			t.Fatal(i)
		}
	}
	if m.allow("test", time.Now()) {
		t.Fatal("rate limit bypassed")
	}
	if !m.allow("test", time.Now().Add(time.Hour)) {
		t.Fatal("expired limit not reset")
	}
}

func TestGooglePurchaseUsesVerifiedPriceAndRejectsTestOrders(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	account, _ := json.Marshal(map[string]string{"client_email": "example@example.invalid", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	t.Setenv("GOOGLE_PLAY_SERVICE_ACCOUNT_JSON", string(account))
	testPurchase := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth" {
			w.Write([]byte(`{"access_token":"test-access","expires_in":3600}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-access" {
			t.Error("missing authorization")
		}
		if strings.Contains(r.URL.Path, "/purchases/products/") {
			p := map[string]interface{}{"purchaseState": 0, "orderId": "order-1", "productId": "full", "quantity": 1, "purchaseTimeMillis": "1792576800000"}
			if testPurchase {
				p["purchaseType"] = 0
			}
			json.NewEncoder(w).Encode(p)
			return
		}
		w.Write([]byte(`{"orderId":"order-1","purchaseToken":"purchase-token-test","state":"PROCESSED","total":{"currencyCode":"USD","units":"9","nanos":990000000},"lineItems":[{"productId":"full"}]}`))
	}))
	defer server.Close()
	v := &StoreVerifier{Client: server.Client(), oauthURL: server.URL + "/oauth", googleBase: server.URL + "/"}
	_, app := mobileFixture(t)
	p := MobilePurchase{ProductID: "full", PurchaseToken: "purchase-token-test"}
	got, err := v.Verify(context.Background(), app, p, time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
	if err != nil || got.Value != 9.99 || got.Currency != "USD" || got.ID != "order-1" {
		t.Fatal(got, err)
	}
	testPurchase = true
	if _, err = v.Verify(context.Background(), app, p, time.Now()); err != errInvalidPurchase {
		t.Fatal("test purchase accepted", err)
	}
	t.Setenv("GOOGLE_PLAY_SERVICE_ACCOUNT_JSON", "")
	if _, err = (&StoreVerifier{}).Verify(context.Background(), app, p, time.Now()); err != ErrVerificationUnavailable {
		t.Fatal(err)
	}
}

func TestAppleSignatureChainBundleAndEnvironment(t *testing.T) {
	now := time.Now().UTC()
	keys := make([]*ecdsa.PrivateKey, 3)
	certs := make([][]byte, 3)
	for i := 2; i >= 0; i-- {
		keys[i], _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		template := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)), Subject: pkix.Name{CommonName: "test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
		if i > 0 {
			template.IsCA = true
			template.KeyUsage |= x509.KeyUsageCertSign
		}
		if i == 0 {
			template.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}, Value: []byte{5, 0}}}
		}
		if i == 1 {
			template.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}, Value: []byte{5, 0}}}
		}
		parent := template
		signer := keys[i]
		if i < 2 {
			parent, _ = x509.ParseCertificate(certs[i+1])
			signer = keys[i+1]
		}
		var err error
		certs[i], err = x509.CreateCertificate(rand.Reader, template, parent, &keys[i].PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
	}
	root, _ := x509.ParseCertificate(certs[2])
	pool := x509.NewCertPool()
	pool.AddCert(root)
	v := &StoreVerifier{appleRoots: pool}
	app := App{OSType: "IOS", Mobile: &MobileConfig{BundleID: "com.example.app"}}
	makeJWS := func(environment, bundle string) string {
		header, _ := json.Marshal(map[string]interface{}{"alg": "ES256", "x5c": []string{base64.StdEncoding.EncodeToString(certs[0]), base64.StdEncoding.EncodeToString(certs[1]), base64.StdEncoding.EncodeToString(certs[2])}})
		payload, _ := json.Marshal(map[string]interface{}{"transactionId": "1", "bundleId": bundle, "productId": "full", "environment": environment, "purchaseDate": now.Add(-time.Minute).UnixMilli(), "signedDate": now.UnixMilli(), "price": 9990, "currency": "USD", "quantity": 1, "inAppOwnershipType": "PURCHASED"})
		unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
		digest := sha256.Sum256([]byte(unsigned))
		r, s, _ := ecdsa.Sign(rand.Reader, keys[0], digest[:])
		sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
		return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	valid := makeJWS("Production", "com.example.app")
	got, err := v.Verify(context.Background(), app, MobilePurchase{ProductID: "full", SignedTransaction: valid}, now)
	if err != nil || got.Value != 9.99 {
		t.Fatal(got, err)
	}
	for _, jws := range []string{makeJWS("Sandbox", "com.example.app"), makeJWS("Production", "another.bundle"), valid[:len(valid)-10] + "tampered", valid + "extra"} {
		if _, err := v.Verify(context.Background(), app, MobilePurchase{ProductID: "full", SignedTransaction: jws}, now); err == nil {
			t.Fatal("invalid JWS accepted")
		}
	}
	if _, err := (&StoreVerifier{}).Verify(context.Background(), app, MobilePurchase{ProductID: "full", SignedTransaction: valid}, now); err == nil {
		t.Fatal("untrusted root accepted")
	}
}
