package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotEnvSyntax(t *testing.T) {
	input := "\ufeff# comment\r\n" +
		"EMPTY=\r\n" +
		"  export TOKEN = abc=def#literal # comment\r\n" +
		"SINGLE='literal $VALUE # value' # comment\r\n" +
		"DOUBLE=\"a\\nb\\\"c\\\\d\"\r\n" +
		"LITERAL=$(do-not-execute)\r\n" +
		"TOKEN=last-value\r\n" +
		"HASH=abc#def # comment\r\n"
	values, err := parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"EMPTY": "", "TOKEN": "last-value", "SINGLE": "literal $VALUE # value",
		"DOUBLE": "a\nb\"c\\d", "LITERAL": "$(do-not-execute)", "HASH": "abc#def"}
	for key, value := range want {
		if values[key] != value {
			t.Errorf("unexpected value for %s", key)
		}
	}
}

func TestLoadPreservesProcessEnvironment(t *testing.T) {
	const existing = "CONVERSION_ENV_TEST_EXISTING"
	const empty = "CONVERSION_ENV_TEST_EMPTY"
	const fromFile = "CONVERSION_ENV_TEST_FROM_FILE"
	t.Setenv(existing, "process-value")
	t.Setenv(empty, "")
	t.Setenv(fromFile, "restore-after-test")
	os.Unsetenv(fromFile)
	path := filepath.Join(t.TempDir(), ".env")
	content := existing + "=file-value\n" + empty + "=file-value\n" + fromFile + "='loaded-from-file'\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(existing) != "process-value" || os.Getenv(empty) != "" || os.Getenv(fromFile) != "loaded-from-file" {
		t.Fatal("dotenv did not respect environment precedence")
	}
}

func TestInvalidFileDoesNotPartiallyLoadOrLeakValues(t *testing.T) {
	const key = "CONVERSION_ENV_TEST_ATOMIC"
	t.Setenv(key, "restore-after-test")
	os.Unsetenv(key)
	for _, invalid := range []string{"not-an-assignment-secret", "1BAD=secret", "TOKEN='unterminated-secret", "TOKEN=\"secret\" trailing", "TOKEN=secret\x00value"} {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(key+"=valid\n"+invalid), 0600); err != nil {
			t.Fatal(err)
		}
		err := Load(path)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid file was accepted or leaked its contents")
		}
		if _, exists := os.LookupEnv(key); exists {
			t.Fatal("invalid file changed the process environment")
		}
	}
}

func TestMissingDefaultAndDisabledFile(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(previous)
	if err := Load(".env"); err != nil {
		t.Fatal("missing default .env should allow exported environment")
	}
	if err := Load(""); err != nil {
		t.Fatal(err)
	}
	if err := Load("explicit-missing.env"); err == nil {
		t.Fatal("missing explicitly selected file was ignored")
	}
}
