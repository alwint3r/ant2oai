package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var configEnvironment = []string{
	"ANT2OAI_CONFIG", "ANT2OAI_AUTH_MODE", "ANT2OAI_AUTH_FILE", "ANT2OAI_ACCOUNT",
	"ANT2OAI_LISTEN", "OPENAI_BASE_URL", "OPENAI_MODEL", "ANT2OAI_MODEL_MAP",
	"ANT2OAI_TOKEN_LIMIT_FIELD", "ANT2OAI_STREAM_USAGE", "ANT2OAI_OMIT_SAMPLING",
	"ANT2OAI_OMIT_REASONING_EFFORT", "ANT2OAI_TIMEOUT", "ANT2OAI_UPSTREAM_IDLE_TIMEOUT",
	"ANT2OAI_MAX_BODY_BYTES", "ANT2OAI_MAX_CONCURRENT", "ANT2OAI_REQUESTS_PER_MINUTE",
	"OPENAI_API_KEY", "ANT2OAI_API_KEY",
}

// clearConfigEnvironment removes the developer's settings for one test. Setenv
// comes first because it registers the restoration of the original value.
func clearConfigEnvironment(t *testing.T) {
	t.Helper()

	for _, name := range configEnvironment {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func defaultConfig() config {
	return config{
		authMode: "api-key", account: "default", listenAddresses: []string{"127.0.0.1:8080"},
		upstreamBaseURL: "https://api.openai.com/v1", modelMap: map[string]string{},
		tokenLimitField: "max_tokens", streamUsage: true, requestTimeout: 10 * time.Minute,
		upstreamIdle: 2 * time.Minute, maxBodyBytes: 32 << 20, maxConcurrent: 64,
		requestsPerMinute: 600,
	}
}

func writeConfigFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("set config mode: %v", err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	clearConfigEnvironment(t)

	cfg, err := loadConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if want := defaultConfig(); !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v\nwant %+v", cfg, want)
	}
}

func TestLoadConfigReadsEveryOption(t *testing.T) {
	want := defaultConfig()
	want.authFile = "/tmp/auth.json"
	want.account = "work"
	want.listenAddresses = []string{"127.0.0.1:9000", "localhost:9001"}
	want.upstreamBaseURL = "https://upstream.test/v1"
	want.model = "fallback"
	want.modelMap = map[string]string{"a": "b"}
	want.tokenLimitField = "max_completion_tokens"
	want.streamUsage = false
	want.omitSampling = true
	want.omitReasoningEffort = true
	want.requestTimeout = time.Second
	want.upstreamIdle = 2 * time.Second
	want.maxBodyBytes = 1024
	want.maxConcurrent = 3
	want.requestsPerMinute = 4

	t.Run("flags", func(t *testing.T) {
		clearConfigEnvironment(t)

		cfg, err := loadConfig([]string{
			"-auth-file", "/tmp/auth.json", "-account", "work",
			"-listen", "127.0.0.1:9000, localhost:9001",
			"-upstream-base-url", "https://upstream.test/v1/", "-model", "fallback",
			"-model-map", `{"a":"b"}`, "-token-limit-field", "max_completion_tokens",
			"-stream-usage=false", "-omit-sampling", "-omit-reasoning-effort", "-timeout", "1s",
			"-upstream-idle-timeout", "2s", "-max-body-bytes", "1024", "-max-concurrent", "3",
			"-requests-per-minute", "4",
		}, io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("config = %+v\nwant %+v", cfg, want)
		}
	})

	t.Run("environment", func(t *testing.T) {
		clearConfigEnvironment(t)
		for name, value := range map[string]string{
			"ANT2OAI_AUTH_FILE": "/tmp/auth.json", "ANT2OAI_ACCOUNT": "work",
			"ANT2OAI_LISTEN":  "127.0.0.1:9000,localhost:9001",
			"OPENAI_BASE_URL": "https://upstream.test/v1", "OPENAI_MODEL": "fallback",
			"ANT2OAI_MODEL_MAP":             `{"a":"b"}`,
			"ANT2OAI_TOKEN_LIMIT_FIELD":     "max_completion_tokens",
			"ANT2OAI_STREAM_USAGE":          "false",
			"ANT2OAI_OMIT_SAMPLING":         "true",
			"ANT2OAI_OMIT_REASONING_EFFORT": "true",
			"ANT2OAI_TIMEOUT":               "1s",
			"ANT2OAI_UPSTREAM_IDLE_TIMEOUT": "2s",
			"ANT2OAI_MAX_BODY_BYTES":        "1024",
			"ANT2OAI_MAX_CONCURRENT":        "3",
			"ANT2OAI_REQUESTS_PER_MINUTE":   "4",
		} {
			t.Setenv(name, value)
		}

		cfg, err := loadConfig(nil, io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("config = %+v\nwant %+v", cfg, want)
		}
	})

	t.Run("JSON file", func(t *testing.T) {
		clearConfigEnvironment(t)
		path := writeConfigFile(t, `{"auth_file":"/tmp/auth.json","account":"work",
			"listen":"127.0.0.1:9000,localhost:9001",
			"upstream_base_url":"https://upstream.test/v1","model":"fallback",
			"model_map":{"a":"b"},"token_limit_field":"max_completion_tokens",
			"stream_usage":false,"omit_sampling":true,"omit_reasoning_effort":true,
			"timeout":"1s","upstream_idle_timeout":"2s","max_body_bytes":1024,
			"max_concurrent":3,"requests_per_minute":4}`, 0600)

		cfg, err := loadConfig([]string{"-config", path}, io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("config = %+v\nwant %+v", cfg, want)
		}
	})
}

func TestConfigPrecedence(t *testing.T) {
	file := `{"model":"file"}`

	cases := []struct {
		name        string
		useFile     bool
		environment string
		flag        string
		want        string
	}{
		{name: "default", want: ""},
		{name: "file over default", useFile: true, want: "file"},
		{name: "environment over file", useFile: true, environment: "env", want: "env"},
		{name: "flag over environment", useFile: true, environment: "env", flag: "flag", want: "flag"},
		{name: "flag over file", useFile: true, flag: "flag", want: "flag"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			var arguments []string
			if testCase.useFile {
				arguments = append(arguments, "-config", writeConfigFile(t, file, 0600))
			}
			if testCase.environment != "" {
				t.Setenv("OPENAI_MODEL", testCase.environment)
			}
			if testCase.flag != "" {
				arguments = append(arguments, "-model", testCase.flag)
			}

			cfg, err := loadConfig(arguments, io.Discard)
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.model != testCase.want {
				t.Errorf("model = %q, want %q", cfg.model, testCase.want)
			}
		})
	}
}

func TestExplicitFlagWithDefaultValueOverridesEnvironment(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ANT2OAI_TIMEOUT", "1s")

	cfg, err := loadConfig([]string{"-timeout", "10m"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.requestTimeout != 10*time.Minute {
		t.Errorf("timeout = %v, want 10m", cfg.requestTimeout)
	}
}

func TestExplicitFlagIgnoresInvalidEnvironmentValue(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("ANT2OAI_TIMEOUT", "soon")

	cfg, err := loadConfig([]string{"-timeout", "1s"}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.requestTimeout != time.Second {
		t.Errorf("timeout = %v, want 1s", cfg.requestTimeout)
	}
}

func TestConfigPathSelection(t *testing.T) {
	t.Run("environment selects the file", func(t *testing.T) {
		clearConfigEnvironment(t)
		t.Setenv("ANT2OAI_CONFIG", writeConfigFile(t, `{"model":"file"}`, 0600))

		cfg, err := loadConfig(nil, io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.model != "file" {
			t.Errorf("model = %q, want file", cfg.model)
		}
	})

	t.Run("flag replaces the environment value", func(t *testing.T) {
		clearConfigEnvironment(t)
		t.Setenv("ANT2OAI_CONFIG", "/nonexistent/config.json")

		cfg, err := loadConfig([]string{"-config", writeConfigFile(t, `{"model":"file"}`, 0600)},
			io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.model != "file" {
			t.Errorf("model = %q, want file", cfg.model)
		}
	})

	t.Run("empty flag disables the file", func(t *testing.T) {
		clearConfigEnvironment(t)
		t.Setenv("ANT2OAI_CONFIG", "/nonexistent/config.json")

		if _, err := loadConfig([]string{"-config="}, io.Discard); err != nil {
			t.Errorf("loadConfig: %v", err)
		}
	})
}

func TestConfigKeys(t *testing.T) {
	t.Run("keys come from the environment", func(t *testing.T) {
		clearConfigEnvironment(t)
		t.Setenv("OPENAI_API_KEY", "upstream")
		t.Setenv("ANT2OAI_API_KEY", "proxy")

		cfg, err := loadConfig(nil, io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.upstreamAPIKey != "upstream" || cfg.proxyAPIKey != "proxy" {
			t.Errorf("keys = %q/%q", cfg.upstreamAPIKey, cfg.proxyAPIKey)
		}
	})

	t.Run("environment replaces the file key", func(t *testing.T) {
		clearConfigEnvironment(t)
		t.Setenv("ANT2OAI_API_KEY", "environment")
		path := writeConfigFile(t, `{"proxy_api_key":"file"}`, 0600)

		cfg, err := loadConfig([]string{"-config", path}, io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.proxyAPIKey != "environment" {
			t.Errorf("proxy key = %q", cfg.proxyAPIKey)
		}
	})

	t.Run("empty environment value clears the file key", func(t *testing.T) {
		clearConfigEnvironment(t)
		t.Setenv("ANT2OAI_API_KEY", "")
		path := writeConfigFile(t, `{"proxy_api_key":"file"}`, 0600)

		cfg, err := loadConfig([]string{"-config", path}, io.Discard)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.proxyAPIKey != "" {
			t.Errorf("proxy key = %q, want empty", cfg.proxyAPIKey)
		}
	})

	for _, name := range []string{"OPENAI_API_KEY", "ANT2OAI_API_KEY"} {
		for _, value := range []string{"a\nb", "a\rb"} {
			t.Run("line break in "+name, func(t *testing.T) {
				clearConfigEnvironment(t)
				t.Setenv(name, value)

				_, err := loadConfig(nil, io.Discard)
				assertError(t, err, "API keys must not contain NUL bytes or line breaks")
			})
		}
	}
}

func TestListenAddresses(t *testing.T) {
	const keyRequired = "a proxy key is required when listening outside loopback; " +
		"set ANT2OAI_API_KEY or JSON proxy_api_key"

	cases := []struct {
		name      string
		listen    string
		hasKey    bool
		want      []string
		wantError string
	}{
		{name: "IPv4 loopback", listen: "127.0.0.1:0", want: []string{"127.0.0.1:0"}},
		{name: "IPv6 loopback", listen: "[::1]:8080", want: []string{"[::1]:8080"}},
		{name: "localhost ignores case", listen: "LocalHost:1", want: []string{"LocalHost:1"}},
		{
			name: "spaces around addresses", listen: " 127.0.0.1:1 , localhost:2 ",
			want: []string{"127.0.0.1:1", "localhost:2"},
		},
		{name: "highest port", listen: "127.0.0.1:65535", want: []string{"127.0.0.1:65535"}},
		{
			name: "outside loopback with a key", listen: "0.0.0.0:1", hasKey: true,
			want: []string{"0.0.0.0:1"},
		},
		{name: "outside loopback without a key", listen: "0.0.0.0:1", wantError: keyRequired},
		{name: "all interfaces without a key", listen: ":8080", wantError: keyRequired},
		{name: "one outside address needs a key", listen: "127.0.0.1:1,0.0.0.0:2", wantError: keyRequired},
		{
			name: "no port", listen: "localhost",
			wantError: `listen address "localhost" must have the form host:port`,
		},
		{
			name: "empty address after a comma", listen: "127.0.0.1:1,",
			wantError: `listen address "" must have the form host:port`,
		},
		{
			name: "port over the limit", listen: "127.0.0.1:65536",
			wantError: `listen address "127.0.0.1:65536" needs a port from 0 to 65535`,
		},
		{
			name: "named port", listen: "127.0.0.1:http",
			wantError: `listen address "127.0.0.1:http" needs a port from 0 to 65535`,
		},
		{
			name: "negative port", listen: "127.0.0.1:-1",
			wantError: `listen address "127.0.0.1:-1" needs a port from 0 to 65535`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			if testCase.hasKey {
				t.Setenv("ANT2OAI_API_KEY", "proxy")
			}

			cfg, err := loadConfig([]string{"-listen", testCase.listen}, io.Discard)
			if testCase.wantError != "" {
				assertError(t, err, testCase.wantError)
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if !reflect.DeepEqual(cfg.listenAddresses, testCase.want) {
				t.Errorf("addresses = %q, want %q", cfg.listenAddresses, testCase.want)
			}
		})
	}
}

func TestUpstreamBaseURL(t *testing.T) {
	const invalidURL = "upstream-base-url must be an absolute HTTP or HTTPS URL"
	const noExtras = "upstream-base-url must not contain credentials, a query, or a fragment"
	const chatGPTOnly = "ChatGPT OAuth requires https://api.openai.com/v1; " +
		"unset OPENAI_BASE_URL or override upstream-base-url"

	cases := []struct {
		name      string
		url       string
		arguments []string
		want      string
		wantError string
	}{
		{name: "trailing slashes are removed", url: "http://host/v1//", want: "http://host/v1"},
		{name: "no path", url: "https://host", want: "https://host"},
		{name: "empty", url: "", wantError: invalidURL},
		{name: "relative", url: "/v1", wantError: invalidURL},
		{name: "no host", url: "http://", wantError: invalidURL},
		{name: "malformed", url: "://", wantError: invalidURL},
		{name: "scheme", url: "ftp://host", wantError: "upstream-base-url must use HTTP or HTTPS"},
		{name: "credentials", url: "http://user:pass@host", wantError: noExtras},
		{name: "query", url: "http://host?a=1", wantError: noExtras},
		{name: "empty query", url: "http://host?", wantError: noExtras},
		{name: "fragment", url: "http://host#a", wantError: noExtras},
		{
			name: "full endpoint", url: "http://host/v1/chat/completions/",
			wantError: "upstream-base-url must end before /chat/completions",
		},
		{
			name: "ChatGPT with the OpenAI URL", url: "https://api.openai.com/v1/",
			arguments: []string{"-auth-mode", "chatgpt"}, want: "https://api.openai.com/v1",
		},
		{
			name: "ChatGPT with another URL", url: "https://host/v1",
			arguments: []string{"-auth-mode", "chatgpt"}, wantError: chatGPTOnly,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			arguments := append([]string{"-upstream-base-url=" + testCase.url}, testCase.arguments...)

			cfg, err := loadConfig(arguments, io.Discard)
			if testCase.wantError != "" {
				assertError(t, err, testCase.wantError)
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.upstreamBaseURL != testCase.want {
				t.Errorf("URL = %q, want %q", cfg.upstreamBaseURL, testCase.want)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	const invalidModelMap = "model-map must be a JSON object of non-empty model IDs"
	const emptyModelID = "model-map must contain non-empty model IDs"
	const invalidAccount = "account must be a non-empty label of at most 64 bytes " +
		"without control characters"
	const invalidBodyLimit = "max-body-bytes must be between 1 and 268435456"
	const invalidCapacity = "max-concurrent must be between 1 and 4096; " +
		"requests-per-minute must be positive"
	const invalidTimeout = "timeouts must be positive"

	cases := []struct {
		name      string
		arguments []string
		want      string
	}{
		{"argument after options", []string{"serve"}, "put the command before options; use -help"},
		{"auth mode", []string{"-auth-mode", "other"}, "auth-mode must be api-key or chatgpt"},
		{"model map syntax", []string{"-model-map", "{"}, invalidModelMap},
		{"model map null", []string{"-model-map", "null"}, invalidModelMap},
		{"model map array", []string{"-model-map", "[]"}, invalidModelMap},
		{"model map value type", []string{"-model-map", `{"a":1}`}, invalidModelMap},
		{"empty mapped model", []string{"-model-map", `{"a":" "}`}, emptyModelID},
		{"empty incoming model", []string{"-model-map", `{"":"b"}`}, emptyModelID},
		{"empty account", []string{"-account", " "}, invalidAccount},
		{"account over the limit", []string{"-account", strings.Repeat("a", 65)}, invalidAccount},
		{"account with a tab", []string{"-account", "a\tb"}, invalidAccount},
		{"account with a line break", []string{"-account", "a\nb"}, invalidAccount},
		{
			"token limit field", []string{"-token-limit-field", "other"},
			"token-limit-field must be max_tokens or max_completion_tokens",
		},
		{"zero timeout", []string{"-timeout", "0"}, invalidTimeout},
		{"negative idle timeout", []string{"-upstream-idle-timeout", "-1s"}, invalidTimeout},
		{"zero body limit", []string{"-max-body-bytes", "0"}, invalidBodyLimit},
		{"body limit too high", []string{"-max-body-bytes", "268435457"}, invalidBodyLimit},
		{"zero concurrency", []string{"-max-concurrent", "0"}, invalidCapacity},
		{"concurrency too high", []string{"-max-concurrent", "4097"}, invalidCapacity},
		{"zero request rate", []string{"-requests-per-minute", "0"}, invalidCapacity},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)

			_, err := loadConfig(testCase.arguments, io.Discard)
			assertError(t, err, testCase.want)
		})
	}
}

func TestConfigAcceptsLimits(t *testing.T) {
	cases := []struct {
		name      string
		arguments []string
	}{
		{"longest account", []string{"-account", strings.Repeat("a", 64)}},
		{"smallest body limit", []string{"-max-body-bytes", "1"}},
		{"largest body limit", []string{"-max-body-bytes", "268435456"}},
		{"smallest concurrency", []string{"-max-concurrent", "1"}},
		{"largest concurrency", []string{"-max-concurrent", "4096"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)

			if _, err := loadConfig(testCase.arguments, io.Discard); err != nil {
				t.Errorf("loadConfig: %v", err)
			}
		})
	}
}

func TestInvalidEnvironmentValue(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"ANT2OAI_TIMEOUT", "soon"},
		{"ANT2OAI_UPSTREAM_IDLE_TIMEOUT", "soon"},
		{"ANT2OAI_STREAM_USAGE", "maybe"},
		{"ANT2OAI_MAX_CONCURRENT", "many"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv(testCase.name, testCase.value)

			_, err := loadConfig(nil, io.Discard)
			assertError(t, err, "invalid "+testCase.name)
		})
	}
}

func TestFlagParsing(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		clearConfigEnvironment(t)
		var output bytes.Buffer

		_, err := loadConfig([]string{"-help"}, &output)
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("error = %v, want %v", err, flag.ErrHelp)
		}

		help := output.String()
		for _, want := range []string{
			"Usage: ant2oai [serve|login|login-status|logout|models] [options]",
			"Translate Anthropic Messages to Chat Completions or ChatGPT Responses.",
			"Upstream API key: OPENAI_API_KEY.",
			"Proxy key: ANT2OAI_API_KEY or proxy_api_key in a protected JSON file.",
			"-requests-per-minute", "(ANT2OAI_REQUESTS_PER_MINUTE)", "default \"127.0.0.1:8080\"",
		} {
			if !strings.Contains(help, want) {
				t.Errorf("help lacks %q:\n%s", want, help)
			}
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		clearConfigEnvironment(t)
		var output bytes.Buffer

		if _, err := loadConfig([]string{"-unknown"}, &output); err == nil {
			t.Fatal("error = nil")
		}
		if !strings.Contains(output.String(), "Usage: ant2oai") {
			t.Errorf("output lacks usage: %s", output.String())
		}
	})
}

func TestJSONConfigRejectsInvalidFiles(t *testing.T) {
	const invalidJSON = "config must contain one JSON object"
	const invalidSize = "config must be valid JSON within the 1 MiB and nesting limits"
	const unknownField = "config contains unknown fields; use documented snake_case keys " +
		"and OPENAI_API_KEY for upstream credentials"

	invalidField := func(name string) string {
		return "invalid config field " + name + "; check its JSON type and value"
	}

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"malformed", `{`, invalidJSON},
		{"array", `[]`, invalidJSON},
		{"null", `null`, invalidJSON},
		{"trailing object", `{"model":"m"}{}`, invalidJSON},
		{"file over the limit", `{"model":"` + strings.Repeat("a", 1<<20) + `"}`, invalidSize},
		{"deep", strings.Repeat("[", 200), invalidSize},
		{"invalid UTF-8", "{\"model\":\"\xff\"}", invalidSize},
		{"unknown field", `{"unknown":1}`, unknownField},
		{"dashes instead of underscores", `{"max-concurrent":5}`, unknownField},
		{"upstream key is not a file field", `{"upstream_api_key":"x"}`, unknownField},
		{"config path is not a file field", `{"config":"x"}`, unknownField},
		{"null field", `{"model":null}`, "config field model cannot be null"},
		{"model map type", `{"model_map":"{}"}`, "config field model_map must be a JSON object"},
		{"string field type", `{"model":1}`, invalidField("model")},
		{"boolean as a string", `{"stream_usage":"false"}`, invalidField("stream_usage")},
		{"boolean as a number", `{"omit_sampling":1}`, invalidField("omit_sampling")},
		{"integer as a string", `{"max_concurrent":"5"}`, invalidField("max_concurrent")},
		{"fractional integer", `{"max_body_bytes":1.5}`, invalidField("max_body_bytes")},
		{"duration as a number", `{"timeout":5}`, invalidField("timeout")},
		{"invalid duration", `{"timeout":"soon"}`, invalidField("timeout")},
		{"key type", `{"proxy_api_key":1}`, "config field proxy_api_key must be a string"},
		{"null key", `{"proxy_api_key":null}`, "config field proxy_api_key must be a string"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			path := writeConfigFile(t, testCase.content, 0600)

			_, err := loadConfig([]string{"-config", path}, io.Discard)
			assertError(t, err, testCase.want)
		})
	}
}

func TestJSONConfigFileAccess(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		clearConfigEnvironment(t)

		_, err := loadConfig([]string{"-config", "/nonexistent/config.json"}, io.Discard)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want a missing file error", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		clearConfigEnvironment(t)

		_, err := loadConfig([]string{"-config", t.TempDir()}, io.Discard)
		assertError(t, err, "config must be valid JSON within the 1 MiB and nesting limits")
	})
}

func TestJSONConfigProxyKeyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	const sharedFile = "config containing proxy_api_key requires owner-only permissions; " +
		"use chmod 600 on the config file"

	cases := []struct {
		name    string
		content string
		mode    os.FileMode
		want    string
	}{
		{name: "protected key", content: `{"proxy_api_key":"k"}`, mode: 0600},
		{name: "shared key", content: `{"proxy_api_key":"k"}`, mode: 0644, want: sharedFile},
		{name: "group readable key", content: `{"proxy_api_key":"k"}`, mode: 0640, want: sharedFile},
		{name: "shared file without a key", content: `{"model":"m"}`, mode: 0644},
		{name: "shared file with an empty key", content: `{"proxy_api_key":""}`, mode: 0644},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearConfigEnvironment(t)
			path := writeConfigFile(t, testCase.content, testCase.mode)

			_, err := loadConfig([]string{"-config", path}, io.Discard)
			if testCase.want != "" {
				assertError(t, err, testCase.want)
				return
			}
			if err != nil {
				t.Errorf("loadConfig: %v", err)
			}
		})
	}
}

func TestJSONConfigKeyAllowsOutsideListener(t *testing.T) {
	clearConfigEnvironment(t)
	path := writeConfigFile(t, `{"listen":"0.0.0.0:1","proxy_api_key":"k"}`, 0600)

	cfg, err := loadConfig([]string{"-config", path}, io.Discard)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.proxyAPIKey != "k" {
		t.Errorf("proxy key = %q", cfg.proxyAPIKey)
	}
}
