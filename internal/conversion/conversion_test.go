package conversion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testDevice = "11111111-2222-4333-8444-555555555555"
const ingestSecret = "test-ingest-secret-at-least-32-characters"

func testApp() App {
	return App{Key: "example-android", AppID: "com.example.app", OSType: "ANDROID",
		XAccountID: "example-account", XTokenEnv: "TEST_X_TOKEN", IngestTokenEnv: "TEST_INGEST_TOKEN",
		Events: []string{"INSTALL", "PURCHASE", "TUTORIAL_COMPLETE"}}
}

func testEvent(id string) Event {
	limited := false
	return Event{EventID: id, EventName: "INSTALL", ConversionTimestamp: time.Now().Add(-time.Minute).UnixMilli(),
		Identifiers: Identifiers{DeviceID: testDevice}, LimitAdTracking: &limited}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func normalize(t *testing.T, event Event) Conversion {
	t.Helper()
	payload, err := event.Normalize(testApp(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestIdentifiersNormalizeBeforeHashing(t *testing.T) {
	ids, err := (Identifiers{Email: "  Person@Example.COM  ", PhoneNumber: " +1 (555) 123-4567 "}).Normalize(false)
	if err != nil {
		t.Fatal(err)
	}
	if ids[0]["hashed_email"] != hash("person@example.com") || ids[1]["hashed_phone_number"] != hash("+15551234567") {
		t.Fatal("hashes do not match normalized identifiers")
	}
	raw, _ := json.Marshal(ids)
	if bytes.Contains(raw, []byte("Person")) || bytes.Contains(raw, []byte("+1555")) {
		t.Fatal("raw contact data leaked")
	}
	ids, err = (Identifiers{DeviceID: testDevice}).Normalize(false)
	if err != nil || ids[0]["device_id"] != testDevice {
		t.Fatal("advertising ID must stay raw")
	}
}

func TestIdentifierRequirements(t *testing.T) {
	for name, ids := range map[string]Identifiers{
		"empty": {}, "only-ip": {IPAddress: "203.0.113.7"}, "only-agent": {UserAgent: "ExampleApp/1.0"},
		"invalid-ip":        {IPAddress: "not-an-ip", UserAgent: "ExampleApp/1.0"},
		"invalid-hash":      {HashedEmail: "not-a-sha256"},
		"national-phone":    {PhoneNumber: "5551234567"},
		"conflicting-email": {Email: "a@example.com", HashedEmail: strings.Repeat("a", 64)},
		"anonymous-id":      {DeviceID: "anonymous-installation-id"},
		"zero-id":           {DeviceID: "00000000-0000-0000-0000-000000000000"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ids.Normalize(false); err == nil {
				t.Fatal("invalid identifiers accepted")
			}
		})
	}
	ids, err := (Identifiers{IPAddress: "2001:db8::1", UserAgent: "ExampleApp/1.0 (Android 15)"}).Normalize(false)
	if err != nil || len(ids) != 2 {
		t.Fatal("IP plus user agent must be accepted")
	}
	if _, err := (Identifiers{DeviceID: testDevice}).Normalize(true); err == nil {
		t.Fatal("limited advertising ID accepted")
	}
}

func TestEventValidationAndATT(t *testing.T) {
	for _, change := range []func(*Event){
		func(e *Event) { e.ConversionTimestamp /= 1000 },
		func(e *Event) { e.ConversionTimestamp = time.Now().Add(time.Hour).UnixMilli() },
		func(e *Event) { e.EventName = "APP_OPENED" },
		func(e *Event) { e.EventName = "PURCHASE" },
		func(e *Event) { e.LimitAdTracking = nil },
		func(e *Event) { e.EventID = "../../escape" },
	} {
		event := testEvent("validation")
		change(&event)
		if _, err := event.Normalize(testApp(), time.Now()); err == nil {
			t.Fatal("invalid event accepted")
		}
	}
	app := testApp()
	app.OSType = "IOS"
	event := testEvent("ios")
	for _, status := range []string{"", "not_determined", "denied", "restricted"} {
		event.ATTStatus = status
		if _, err := event.Normalize(app, time.Now()); err == nil {
			t.Fatal("iOS tracking without ATT authorization accepted")
		}
	}
	event.ATTStatus = "authorized"
	limited := true
	event.LimitAdTracking = &limited // iOS derives the flag from ATT, not caller assertions.
	payload, err := event.Normalize(app, time.Now())
	if err != nil || payload.LimitAdTracking {
		t.Fatal("ATT-derived tracking state is wrong")
	}
}

func TestPurchaseAndTrainingMapping(t *testing.T) {
	event := testEvent("order:123")
	event.EventName = "PURCHASE"
	price := 7.99
	event.Value, event.PriceCurrency, event.NumberItems = &price, "usd", 1
	event.Contents = []Content{{ContentID: "example.full_game", ContentType: "product", NumItems: 1}}
	payload := normalize(t, event)
	if payload.AppID != testApp().AppID || payload.OSType != "ANDROID" || payload.PriceCurrency != "USD" || payload.Value == nil || *payload.Value != 7.99 {
		t.Fatal("purchase mapping is wrong")
	}
	event = testEvent("training:installation-1")
	event.EventName = "TUTORIAL_COMPLETE"
	if normalize(t, event).EventName != "TUTORIAL_COMPLETE" {
		t.Fatal("training mapping is wrong")
	}
}

func TestDurabilityDeduplicationAndCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := normalize(t, testEvent("install:one"))
	for n := 0; n < 2; n++ {
		if _, err := store.Enqueue("app-one", "install:one", payload, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	changed := payload
	changed.EventName = "TUTORIAL_COMPLETE"
	if _, err := store.Enqueue("app-one", "install:one", changed, time.Now()); err != ErrConflict {
		t.Fatal("event identity conflict was not detected")
	}
	if _, err := store.Enqueue("app-two", "install:one", payload, time.Now()); err != nil {
		t.Fatal("different apps must have separate deduplication scopes")
	}
	if competing, err := OpenStore(dir); err == nil {
		competing.Close()
		t.Fatal("multiple dispatchers acquired the same data directory")
	}
	claimed, err := store.Claim("app-one", time.Now())
	if err != nil || len(claimed) != 1 {
		t.Fatal("duplicate event was queued")
	}
	store.Close()
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	record, _ := reopened.Get("app-one", "install:one")
	if record.State != "uncertain" || record.ErrorCode != "interrupted_delivery" {
		t.Fatal("crashed in-flight request must not be resent automatically")
	}
	claimed, _ = reopened.Claim("app-one", time.Now())
	if len(claimed) != 0 {
		t.Fatal("uncertain event was automatically replayed")
	}
	if _, err := reopened.Retry("app-one", "install:one", time.Now()); err != nil {
		t.Fatal(err)
	}
	claimed, _ = reopened.Claim("app-one", time.Now())
	if len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatal("explicit retry did not preserve event identity")
	}
}

func TestSuccessDropsIdentifiersButPreservesDeduplication(t *testing.T) {
	store := testStore(t)
	event := testEvent("contact-install")
	event.Identifiers = Identifiers{Email: "Person@example.com"}
	payload := normalize(t, event)
	store.Enqueue(testApp().Key, event.EventID, payload, time.Now())
	records, _ := store.Claim(testApp().Key, time.Now())
	if err := store.Finish(records, Delivery{State: "sent", HTTPStatus: 200}, time.Now()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(store.dir, recordKey(testApp().Key, event.EventID)+".json"))
	if err != nil || bytes.Contains(body, []byte("hashed_email")) || bytes.Contains(body, []byte("@example")) || bytes.Contains(body, []byte(hash("person@example.com"))) {
		t.Fatal("sent payload was not scrubbed")
	}
	record, err := store.Enqueue(testApp().Key, event.EventID, payload, time.Now())
	if err != nil || record.State != "sent" {
		t.Fatal("sent event tombstone did not deduplicate")
	}
}

func TestDispatchBatchesAndXContract(t *testing.T) {
	t.Setenv("TEST_X_TOKEN", "x-test-token-never-an-ingest-token")
	store := testStore(t)
	app := testApp()
	payload := normalize(t, testEvent("batch"))
	for n := 0; n < 501; n++ {
		if _, err := store.Enqueue(app.Key, fmt.Sprintf("install:%03d", n), payload, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var counts []int
	var countsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/12/accounts/example-account/measurement/app_conversions" || r.Header.Get("X-App-Conversion-Token") != os.Getenv(app.XTokenEnv) || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect X request")
		}
		var request struct {
			Conversions []Conversion `json:"conversions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		countsMu.Lock()
		counts = append(counts, len(request.Conversions))
		countsMu.Unlock()
		if request.Conversions[0].AppID != app.AppID || request.Conversions[0].Identifiers[0]["device_id"] != testDevice {
			t.Error("incorrect conversion payload")
		}
		fmt.Fprintf(w, `{"data":{"conversions_processed":%d,"debug_id":"abc-123"}}`, len(request.Conversions))
	}))
	defer server.Close()
	x := NewXClient()
	x.baseURL = server.URL + "/12/accounts/"
	dispatcher := Dispatcher{Config: Configuration{Apps: []App{app}}, Store: store, Client: x}
	for n := 0; n < 3; n++ {
		if err := dispatcher.DispatchOnce(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	countsMu.Lock()
	defer countsMu.Unlock()
	if len(counts) != 2 || counts[0] != 500 || counts[1] != 1 {
		t.Fatalf("batch sizes: %v", counts)
	}
	record, _ := store.Get(app.Key, "install:000")
	if record.State != "sent" || record.HTTPStatus != 200 || record.DebugID != "abc-123" {
		t.Fatal("X acknowledgement was not saved")
	}
}

func TestDeliveryFailuresNeverBlindlyReplay(t *testing.T) {
	t.Setenv("TEST_X_TOKEN", "x-secret")
	for _, test := range []struct {
		status      int
		body, state string
	}{
		{200, `{"data":{"conversions_processed":0}}`, "uncertain"},
		{200, `{"data":{}}`, "uncertain"},
		{200, `not json`, "uncertain"}, {500, `{}`, "uncertain"},
		{400, `{}`, "failed"}, {422, `{}`, "failed"},
		{401, `{}`, "pending"}, {403, `{}`, "pending"}, {429, `{}`, "pending"},
	} {
		t.Run(fmt.Sprintf("%d-%s", test.status, test.body), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			x := NewXClient()
			x.baseURL = server.URL + "/"
			result := x.Send(context.Background(), testApp(), []Conversion{normalize(t, testEvent("failure"))})
			if result.State != test.state || (result.State == "pending" && result.RetryAfter != 2*time.Minute) {
				t.Fatalf("incorrect result: %+v", result)
			}
		})
	}
}

func TestNoRedirectOrMissingTokenLeak(t *testing.T) {
	t.Setenv("TEST_X_TOKEN", "secret")
	var called atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	x := NewXClient()
	x.baseURL = server.URL + "/"
	result := x.Send(context.Background(), testApp(), []Conversion{normalize(t, testEvent("redirect"))})
	if called.Load() || result.State != "uncertain" {
		t.Fatal("X token could follow a redirect")
	}
	t.Setenv("TEST_X_TOKEN", "")
	result = x.Send(context.Background(), testApp(), []Conversion{normalize(t, testEvent("disabled"))})
	if result.State != "pending" || result.Code != "x_token_missing" {
		t.Fatal("missing token should leave events pending")
	}
	payload := normalize(t, testEvent("wrong-app"))
	payload.AppID = "com.example.other"
	result = x.Send(context.Background(), testApp(), []Conversion{payload})
	if result.State != "failed" || result.Code != "app_configuration_mismatch" {
		t.Fatal("conversion was routed through another application's configuration")
	}
}

func apiRequest(api *API, method, path, token string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	return w
}

func TestAPIAuthIsolationAndIdempotency(t *testing.T) {
	t.Setenv("TEST_INGEST_TOKEN", ingestSecret)
	t.Setenv("SECOND_INGEST_TOKEN", "another-ingest-secret-at-least-32-characters")
	second := testApp()
	second.Key, second.AppID, second.IngestTokenEnv = "second", "com.example.second", "SECOND_INGEST_TOKEN"
	api := &API{Config: Configuration{Apps: []App{testApp(), second}}, Store: testStore(t)}
	event := testEvent("install:api")
	body, _ := json.Marshal(map[string]interface{}{"events": []Event{event}})
	path := "/v1/apps/example-android/events"
	for _, invalid := range []string{"", "wrong-token", os.Getenv(second.IngestTokenEnv)} {
		if apiRequest(api, "POST", path, invalid, body).Code != 401 {
			t.Fatal("invalid app credentials accepted")
		}
	}
	for n := 0; n < 2; n++ {
		if response := apiRequest(api, "POST", path, ingestSecret, body); response.Code != 202 {
			t.Fatalf("submission failed: %d %s", response.Code, response.Body.String())
		}
	}
	if apiRequest(api, "GET", "/v1/apps/second/events/"+event.EventID, os.Getenv(second.IngestTokenEnv), nil).Code != 404 {
		t.Fatal("cross-app delivery status leaked")
	}
	event.EventName = "TUTORIAL_COMPLETE"
	body, _ = json.Marshal(map[string]interface{}{"events": []Event{event}})
	if apiRequest(api, "POST", path, ingestSecret, body).Code != 409 {
		t.Fatal("conflicting identity accepted")
	}
	if apiRequest(api, "POST", path, ingestSecret, []byte(`{"events":[],"app_id":"arbitrary"}`)).Code != 400 {
		t.Fatal("unknown field accepted")
	}
	if apiRequest(api, "POST", path, ingestSecret, []byte(`{"events":[]} {}`)).Code != 400 {
		t.Fatal("multiple JSON values accepted")
	}
	if apiRequest(api, "POST", path, ingestSecret, []byte(`{"events":[]}`)).Code != 400 {
		t.Fatal("empty batch accepted")
	}
	tooMany := make([]Event, 501)
	body, _ = json.Marshal(map[string]interface{}{"events": tooMany})
	if apiRequest(api, "POST", path, ingestSecret, body).Code != 400 {
		t.Fatal("oversize batch accepted")
	}
}

func TestConfigAndConcurrentDuplicateSubmission(t *testing.T) {
	app := testApp()
	config := Configuration{Apps: []App{app}}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(app.IngestTokenEnv, "")
	if config.ValidateIngestSecrets() == nil {
		t.Fatal("missing ingress secret accepted")
	}
	t.Setenv(app.IngestTokenEnv, ingestSecret)
	if err := config.ValidateIngestSecrets(); err != nil {
		t.Fatal(err)
	}
	if (Configuration{Apps: []App{app, app}}).Validate() == nil {
		t.Fatal("duplicate app configuration accepted")
	}
	store := testStore(t)
	payload := normalize(t, testEvent("parallel"))
	var workers sync.WaitGroup
	for n := 0; n < 20; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := store.Enqueue(app.Key, "parallel", payload, time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	claimed, err := store.Claim(app.Key, time.Now())
	if err != nil || len(claimed) != 1 {
		t.Fatal("concurrent duplicate submission was not deduplicated")
	}
}
