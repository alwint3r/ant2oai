package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

type configOption struct {
	name        string
	environment string
}

const maxConfigFileBytes = 1 << 20

func applyJSONConfig(
	flags *flag.FlagSet, path string, options []configOption, proxyAPIKey *string,
) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, maxConfigFileBytes+1))
	if err != nil || len(raw) > maxConfigFileBytes || checkJSONDepth(raw) != nil {
		return errors.New("config must be valid JSON within the 1 MiB and nesting limits")
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return errors.New("config must contain one JSON object")
	}

	keyValue, hasProxyAPIKey := values["proxy_api_key"]
	var fileProxyAPIKey string
	if hasProxyAPIKey {
		delete(values, "proxy_api_key")
		fileProxyAPIKey, err = parseFileProxyKey(keyValue, file)
		if err != nil {
			return err
		}
	}

	for _, option := range options {
		key := strings.ReplaceAll(option.name, "-", "_")
		value, exists := values[key]
		if !exists {
			continue
		}

		delete(values, key)
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("config field %s cannot be null", key)
		}

		// Keep the flag parser as the single parser for effective option values.
		// The file still requires JSON booleans, integers, and strings by type.
		var text string
		var typeError error
		if option.name == "model-map" {
			if !isJSONObject(value) {
				return errors.New("config field model_map must be a JSON object")
			}
			text = string(value)
		} else {
			switch flags.Lookup(option.name).Value.(flag.Getter).Get().(type) {
			case bool:
				var boolean bool
				typeError = json.Unmarshal(value, &boolean)
				text = fmt.Sprint(boolean)
			case int:
				var integer int
				typeError = json.Unmarshal(value, &integer)
				text = fmt.Sprint(integer)
			case int64:
				var integer int64
				typeError = json.Unmarshal(value, &integer)
				text = fmt.Sprint(integer)
			case string, time.Duration:
				typeError = json.Unmarshal(value, &text)
			default:
				return errors.New("unsupported configuration value type")
			}
		}
		if typeError != nil || flags.Set(option.name, text) != nil {
			return fmt.Errorf("invalid config field %s; check its JSON type and value", key)
		}
	}

	if len(values) != 0 {
		return errors.New("config contains unknown fields; use documented snake_case keys " +
			"and OPENAI_API_KEY for upstream credentials")
	}
	if hasProxyAPIKey {
		*proxyAPIKey = fileProxyAPIKey
	}
	return nil
}

// parseFileProxyKey decodes the proxy key. Credentials have no command-line flag.
// A file containing a proxy key is a local secret store, so check the permissions
// of the opened file.
func parseFileProxyKey(value json.RawMessage, file *os.File) (string, error) {
	var key string
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &key) != nil {
		return "", errors.New("config field proxy_api_key must be a string")
	}
	if key == "" {
		return "", nil
	}

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect config permissions: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("config containing proxy_api_key must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return "", errors.New("config containing proxy_api_key requires owner-only permissions; " +
			"use chmod 600 on the config file")
	}
	return key, nil
}
