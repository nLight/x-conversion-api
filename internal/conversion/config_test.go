package conversion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigFromEnvironmentWithoutFile(t *testing.T) {
	app := testApp()
	data, err := json.Marshal(Configuration{Apps: []App{app}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONVERSION_APPS_JSON", string(data))
	config, err := LoadConfig(filepath.Join(t.TempDir(), "not-mounted.json"))
	if err != nil || len(config.Apps) != 1 || config.Apps[0].AppID != app.AppID {
		t.Fatal("environment config should work without a mounted file")
	}
	other := app
	other.AppID = "com.example.file"
	fileData, _ := json.Marshal(Configuration{Apps: []App{other}})
	path := filepath.Join(t.TempDir(), "apps.json")
	if err := os.WriteFile(path, fileData, 0600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(path)
	if err != nil || config.Apps[0].AppID != app.AppID {
		t.Fatal("nonempty environment config must take precedence over a file")
	}
}

func TestLoadConfigFallsBackOnlyWhenEnvironmentIsEmpty(t *testing.T) {
	fileData, _ := json.Marshal(Configuration{Apps: []App{testApp()}})
	path := filepath.Join(t.TempDir(), "apps.json")
	if err := os.WriteFile(path, fileData, 0600); err != nil {
		t.Fatal(err)
	}
	for _, empty := range []string{"", " \n\t "} {
		t.Setenv("CONVERSION_APPS_JSON", empty)
		config, err := LoadConfig(path)
		if err != nil || len(config.Apps) != 1 {
			t.Fatal("empty environment config should preserve file-based setup")
		}
	}
	for _, invalid := range []string{
		`{"apps":["private-test-value"`,
		`{"apps":[],"private-test-value":true}`,
		`{"apps":[]}`, `null`,
		string(fileData) + ` {}`,
	} {
		t.Setenv("CONVERSION_APPS_JSON", invalid)
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("invalid environment config silently fell back to the file")
		}
		if strings.Contains(err.Error(), "private-test-value") {
			t.Fatal("configuration error exposed the JSON contents")
		}
	}
}
