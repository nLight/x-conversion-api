package conversion

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Configuration contains references to environment variables, never their values.
type Configuration struct {
	Apps []App `json:"apps"`
}

type App struct {
	Key            string   `json:"key"`
	AppID          string   `json:"app_id"`
	OSType         string   `json:"os_type"`
	XAccountID     string   `json:"x_account_id"`
	XTokenEnv      string   `json:"x_token_env"`
	IngestTokenEnv string   `json:"ingest_token_env"`
	Events         []string `json:"events"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func LoadConfig(path string) (Configuration, error) {
	// Hosted deployments can provide private app settings without a bind-mounted
	// configuration file. Invalid nonempty JSON must fail rather than fall back.
	if raw := os.Getenv("CONVERSION_APPS_JSON"); strings.TrimSpace(raw) != "" {
		var config Configuration
		if err := decodeJSON(strings.NewReader(raw), &config); err != nil {
			// Do not include JSON or decoder errors: the input is deployment config.
			return config, fmt.Errorf("CONVERSION_APPS_JSON must contain valid application configuration JSON")
		}
		return config, config.Validate()
	}
	f, err := os.Open(path)
	if err != nil {
		return Configuration{}, err
	}
	defer f.Close()
	var config Configuration
	if err = decodeJSON(f, &config); err != nil {
		return config, fmt.Errorf("configuration: %w", err)
	}
	return config, config.Validate()
}

func (c Configuration) Validate() error {
	if len(c.Apps) == 0 {
		return fmt.Errorf("configure at least one application")
	}
	keys, pairs, ingestEnvs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, app := range c.Apps {
		pair := app.OSType + ":" + app.AppID
		if !namePattern.MatchString(app.Key) || keys[app.Key] || pairs[pair] {
			return fmt.Errorf("application keys and platform/app_id pairs must be valid and unique")
		}
		keys[app.Key], pairs[pair] = true, true
		if strings.TrimSpace(app.AppID) == "" || (app.OSType != "ANDROID" && app.OSType != "IOS") {
			return fmt.Errorf("%s: app_id and ANDROID or IOS os_type are required", app.Key)
		}
		if !namePattern.MatchString(app.XAccountID) || !envPattern.MatchString(app.XTokenEnv) || !envPattern.MatchString(app.IngestTokenEnv) {
			return fmt.Errorf("%s: invalid account or environment variable name", app.Key)
		}
		if ingestEnvs[app.IngestTokenEnv] || app.IngestTokenEnv == app.XTokenEnv {
			return fmt.Errorf("%s: use a separate ingestion token for each app, distinct from the X token", app.Key)
		}
		ingestEnvs[app.IngestTokenEnv] = true
		if len(app.Events) == 0 {
			return fmt.Errorf("%s: an event allowlist is required", app.Key)
		}
		for _, event := range app.Events {
			if event != "INSTALL" && event != "PURCHASE" && event != "TUTORIAL_COMPLETE" {
				return fmt.Errorf("%s: unsupported event %q", app.Key, event)
			}
		}
	}
	return nil
}

// ValidateIngestSecrets fails closed rather than starting a public unauthenticated API.
// An X token may be absent during setup; events remain queued until it is configured.
func (c Configuration) ValidateIngestSecrets() error {
	seen := map[[32]byte]bool{}
	for _, app := range c.Apps {
		token := os.Getenv(app.IngestTokenEnv)
		hash := sha256.Sum256([]byte(token))
		if len(token) < 32 || strings.TrimSpace(token) != token || seen[hash] {
			return fmt.Errorf("set %s to a unique random secret of at least 32 characters", app.IngestTokenEnv)
		}
		if token == os.Getenv(app.XTokenEnv) {
			return fmt.Errorf("%s must differ from the X token", app.IngestTokenEnv)
		}
		seen[hash] = true
	}
	return nil
}

func (c Configuration) Find(key string) (App, bool) {
	for _, app := range c.Apps {
		if app.Key == key {
			return app, true
		}
	}
	return App{}, false
}

func (a App) Authenticate(header string) bool {
	secret := os.Getenv(a.IngestTokenEnv)
	if len(secret) < 32 || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	expected := sha256.Sum256([]byte(secret))
	actual := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	return subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
}

func (c Configuration) Ready() bool {
	for _, app := range c.Apps {
		if strings.TrimSpace(os.Getenv(app.XTokenEnv)) == "" {
			return false
		}
	}
	return true
}
