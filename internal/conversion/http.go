package conversion

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

func decodeJSON(reader io.Reader, target interface{}) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

type API struct {
	Config Configuration
	Store  *Store
	Mobile *MobileAPI
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/mobile/") && a.Mobile != nil {
		a.Mobile.ServeHTTP(w, r)
		return
	}
	if (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") && r.Method == http.MethodGet {
		if r.URL.Path == "/readyz" && !a.Config.Ready() {
			fail(w, http.StatusServiceUnavailable, "waiting_for_x_token")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || len(parts) > 6 || parts[0] != "v1" || parts[1] != "apps" || parts[3] != "events" {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	app, ok := a.Config.Find(parts[2])
	if !ok || !app.Authenticate(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if len(parts) == 5 && r.Method == http.MethodGet {
		record, err := a.Store.Get(app.Key, parts[4])
		if err != nil {
			fail(w, http.StatusNotFound, "event_not_found")
			return
		}
		writeJSON(w, http.StatusOK, record)
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "use_application_json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if len(parts) == 6 && parts[5] == "retry" {
		var input struct {
			AcceptDuplicateRisk bool `json:"accept_duplicate_risk"`
		}
		if decodeJSON(r.Body, &input) != nil || !input.AcceptDuplicateRisk {
			fail(w, http.StatusBadRequest, "retry_requires_accept_duplicate_risk")
			return
		}
		record, err := a.Store.Retry(app.Key, parts[4], time.Now().UTC())
		if errors.Is(err, ErrNotFound) {
			fail(w, http.StatusNotFound, "event_not_found")
		} else if err != nil {
			fail(w, http.StatusConflict, "event_cannot_be_retried")
		} else {
			writeJSON(w, http.StatusAccepted, record)
		}
		return
	}
	if len(parts) != 4 {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	var input struct {
		Events []Event `json:"events"`
	}
	if err := decodeJSON(r.Body, &input); err != nil {
		fail(w, http.StatusBadRequest, "invalid_json_or_unknown_field")
		return
	}
	if len(input.Events) < 1 || len(input.Events) > MaxBatch {
		fail(w, http.StatusBadRequest, "send_1_to_500_events")
		return
	}
	now := time.Now().UTC()
	payloads := make([]Conversion, len(input.Events))
	seen := map[string]bool{}
	for index, event := range input.Events {
		if seen[event.EventID] {
			fail(w, http.StatusBadRequest, "duplicate_event_id_in_batch")
			return
		}
		seen[event.EventID] = true
		payloads[index], err = event.Normalize(app, now)
		if err != nil {
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("events[%d]: %s", index, err.Error()))
			return
		}
	}
	var results []Record
	for index, event := range input.Events {
		record, err := a.Store.Enqueue(app.Key, event.EventID, payloads[index], now)
		if errors.Is(err, ErrConflict) {
			fail(w, http.StatusConflict, "event_id_has_different_content")
			return
		}
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "outbox_unavailable_retry_with_same_event_ids")
			return
		}
		results = append(results, record)
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"events": results})
}
