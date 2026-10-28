// send-test submits a real test-device INSTALL and waits for an actual X 200 acknowledgement.
// There is no documented sandbox flag in the supplied X API; never run this on every deploy.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"conversion-api/internal/conversion"
	"conversion-api/internal/envfile"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	baseURL := flag.String("url", "http://127.0.0.1:8080", "conversion service URL")
	configPath := flag.String("config", "config/apps.json", "application configuration file")
	appKey := flag.String("app", "", "configured Android app key (default: first configured app)")
	envPath := flag.String("env-file", ".env", "dotenv file; empty disables loading")
	flag.Parse()
	if err := envfile.Load(*envPath); err != nil {
		return err
	}
	config, err := conversion.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	key := *appKey
	if key == "" {
		key = config.Apps[0].Key
	}
	app, ok := config.Find(key)
	if !ok || app.OSType != "ANDROID" {
		return fmt.Errorf("choose a configured Android app")
	}
	endpoint, err := url.Parse(*baseURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && (endpoint.Hostname() == "127.0.0.1" || endpoint.Hostname() == "localhost"))) {
		return fmt.Errorf("use HTTPS, or HTTP on localhost")
	}
	token := os.Getenv(app.IngestTokenEnv)
	if len(token) < 32 {
		return fmt.Errorf("set %s", app.IngestTokenEnv)
	}
	deviceID := os.Getenv("X_TEST_DEVICE_ID")
	if deviceID == "" {
		return fmt.Errorf("set X_TEST_DEVICE_ID to your consenting Android test device's raw advertising ID")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	limited := false
	event := conversion.Event{EventID: "test-install-" + hex.EncodeToString(random), EventName: "INSTALL",
		ConversionTimestamp: time.Now().UnixMilli(), Identifiers: conversion.Identifiers{DeviceID: deviceID}, LimitAdTracking: &limited}
	if _, err := event.Normalize(app, time.Now()); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]interface{}{"events": []conversion.Event{event}})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	eventsURL := strings.TrimRight(*baseURL, "/") + "/v1/apps/" + app.Key + "/events"
	request, err := http.NewRequest(http.MethodPost, eventsURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("cannot contact conversion service")
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("event submission returned HTTP %d", response.StatusCode)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		request, _ = http.NewRequest(http.MethodGet, eventsURL+"/"+event.EventID, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err = client.Do(request)
		if err != nil {
			return fmt.Errorf("cannot read delivery status; inspect event %s before retrying", event.EventID)
		}
		var record conversion.Record
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&record)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			return fmt.Errorf("cannot read delivery status; inspect event %s", event.EventID)
		}
		if record.State == "sent" && record.HTTPStatus == http.StatusOK {
			fmt.Printf("X HTTP 200: conversions_processed=1 debug_id=%s event_id=%s\n", record.DebugID, event.EventID)
			return nil
		}
		if record.State == "failed" || record.State == "uncertain" {
			return fmt.Errorf("event %s: %s (%s), X HTTP %d", event.EventID, record.State, record.ErrorCode, record.HTTPStatus)
		}
	}
	return fmt.Errorf("X has not acknowledged event %s yet; inspect its status instead of creating another test", event.EventID)
}
