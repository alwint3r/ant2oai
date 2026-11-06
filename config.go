package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	maxAccountLabelBytes = 64
	maxBodyLimitBytes    = 256 << 20
	maxConcurrencyLimit  = 4096
	maxPortNumber        = 65535
)

type config struct {
	authMode            string
	authFile            string
	account             string
	requestConsent      bool
	listenAddresses     []string
	upstreamBaseURL     string
	upstreamAPIKey      string
	proxyAPIKey         string
	model               string
	modelMap            map[string]string
	tokenLimitField     string
	streamUsage         bool
	omitSampling        bool
	omitReasoningEffort bool
	requestTimeout      time.Duration
	upstreamIdle        time.Duration
	maxBodyBytes        int64
	maxConcurrent       int
	requestsPerMinute   int
}

// configOptions lists the flags that a JSON file and the environment can also set.
var configOptions = []configOption{
	{"auth-mode", "ANT2OAI_AUTH_MODE"},
	{"auth-file", "ANT2OAI_AUTH_FILE"},
	{"account", "ANT2OAI_ACCOUNT"},
	{"listen", "ANT2OAI_LISTEN"},
	{"upstream-base-url", "OPENAI_BASE_URL"},
	{"model", "OPENAI_MODEL"},
	{"model-map", "ANT2OAI_MODEL_MAP"},
	{"token-limit-field", "ANT2OAI_TOKEN_LIMIT_FIELD"},
	{"stream-usage", "ANT2OAI_STREAM_USAGE"},
	{"omit-sampling", "ANT2OAI_OMIT_SAMPLING"},
	{"omit-reasoning-effort", "ANT2OAI_OMIT_REASONING_EFFORT"},
	{"timeout", "ANT2OAI_TIMEOUT"},
	{"upstream-idle-timeout", "ANT2OAI_UPSTREAM_IDLE_TIMEOUT"},
	{"max-body-bytes", "ANT2OAI_MAX_BODY_BYTES"},
	{"max-concurrent", "ANT2OAI_MAX_CONCURRENT"},
	{"requests-per-minute", "ANT2OAI_REQUESTS_PER_MINUTE"},
}

// unparsedOptions holds flag values that need parsing after all sources are applied.
type unparsedOptions struct {
	configPath   string
	modelMapJSON string
	listenList   string
}

func loadConfig(arguments []string, output io.Writer) (config, error) {
	var cfg config
	var unparsed unparsedOptions
	flags := newFlagSet(&cfg, &unparsed, output)
	if err := flags.Parse(arguments); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("put the command before options; use -help")
	}
	if err := applyConfigSources(flags, unparsed.configPath, &cfg.proxyAPIKey); err != nil {
		return cfg, err
	}

	// Keys have no flags, so they cannot appear in a process listing.
	cfg.upstreamAPIKey = os.Getenv("OPENAI_API_KEY")
	if value, exists := os.LookupEnv("ANT2OAI_API_KEY"); exists {
		cfg.proxyAPIKey = value
	}
	if strings.ContainsAny(cfg.upstreamAPIKey+cfg.proxyAPIKey, "\x00\r\n") {
		return cfg, errors.New("API keys must not contain NUL bytes or line breaks")
	}

	modelMap, err := parseModelMap(unparsed.modelMapJSON)
	if err != nil {
		return cfg, err
	}
	cfg.modelMap = modelMap

	if err := validateSettings(cfg); err != nil {
		return cfg, err
	}

	upstreamBaseURL, err := normalizeUpstreamBaseURL(cfg.upstreamBaseURL, cfg.usesChatGPT())
	if err != nil {
		return cfg, err
	}
	cfg.upstreamBaseURL = upstreamBaseURL

	listenAddresses, err := parseListenAddresses(unparsed.listenList, cfg.proxyAPIKey != "")
	if err != nil {
		return cfg, err
	}
	cfg.listenAddresses = listenAddresses
	return cfg, nil
}

func newFlagSet(cfg *config, unparsed *unparsedOptions, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("ant2oai", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: ant2oai [serve|login|login-status|logout|models] [options]")
		fmt.Fprintln(output,
			"Translate Anthropic Messages to Chat Completions or ChatGPT Responses.")
		fmt.Fprintln(output, "Upstream API key: OPENAI_API_KEY.")
		fmt.Fprintln(output,
			"Proxy key: ANT2OAI_API_KEY or proxy_api_key in a protected JSON file.")
		flags.PrintDefaults()
	}

	flags.StringVar(&unparsed.configPath, "config", "",
		"JSON configuration file (ANT2OAI_CONFIG)")
	flags.StringVar(&cfg.authMode, "auth-mode", "api-key",
		"api-key for Chat Completions; chatgpt for OAuth Responses (ANT2OAI_AUTH_MODE)")
	flags.StringVar(&cfg.authFile, "auth-file", "",
		"OAuth credential file; default: OS config directory/ant2oai/chatgpt.json "+
			"(ANT2OAI_AUTH_FILE)")
	flags.StringVar(&cfg.account, "account", "default",
		"Saved ChatGPT account label (ANT2OAI_ACCOUNT)")
	flags.BoolVar(&cfg.requestConsent, "reconsent", false,
		"Ask for ChatGPT plan permission again during login")
	flags.StringVar(&unparsed.listenList, "listen", "127.0.0.1:8080",
		"HTTP listen addresses, separated by commas (ANT2OAI_LISTEN)")
	flags.StringVar(&cfg.upstreamBaseURL, "upstream-base-url", "https://api.openai.com/v1",
		"Upstream API base URL, including API prefix (OPENAI_BASE_URL)")
	flags.StringVar(&cfg.model, "model", "",
		"Fallback upstream model ID (OPENAI_MODEL)")
	flags.StringVar(&unparsed.modelMapJSON, "model-map", "{}",
		"JSON object mapping incoming model IDs to upstream IDs (ANT2OAI_MODEL_MAP)")
	flags.StringVar(&cfg.tokenLimitField, "token-limit-field", "max_tokens",
		"max_tokens or max_completion_tokens (ANT2OAI_TOKEN_LIMIT_FIELD)")
	flags.BoolVar(&cfg.streamUsage, "stream-usage", true,
		"Request stream_options.include_usage (ANT2OAI_STREAM_USAGE)")
	flags.BoolVar(&cfg.omitSampling, "omit-sampling", false,
		"Omit temperature and top_p (ANT2OAI_OMIT_SAMPLING)")
	flags.BoolVar(&cfg.omitReasoningEffort, "omit-reasoning-effort", false,
		"Omit the reasoning effort taken from output_config.effort (ANT2OAI_OMIT_REASONING_EFFORT)")
	flags.DurationVar(&cfg.requestTimeout, "timeout", 10*time.Minute,
		"Total upstream request timeout, including streaming (ANT2OAI_TIMEOUT)")
	flags.DurationVar(&cfg.upstreamIdle, "upstream-idle-timeout", 2*time.Minute,
		"Response-header and streaming idle timeout (ANT2OAI_UPSTREAM_IDLE_TIMEOUT)")
	flags.Int64Var(&cfg.maxBodyBytes, "max-body-bytes", 32<<20,
		"Maximum incoming JSON body size in bytes (ANT2OAI_MAX_BODY_BYTES)")
	flags.IntVar(&cfg.maxConcurrent, "max-concurrent", 64,
		"Maximum active inference requests (ANT2OAI_MAX_CONCURRENT)")
	flags.IntVar(&cfg.requestsPerMinute, "requests-per-minute", 600,
		"Global rate limit; burst equals max-concurrent (ANT2OAI_REQUESTS_PER_MINUTE)")
	return flags
}

// applyConfigSources applies the four precedence levels to parsed flags, from the
// lowest level to the highest: defaults, JSON file, environment, command line.
func applyConfigSources(flags *flag.FlagSet, configPath string, proxyAPIKey *string) error {
	// Parsing already happened to select the file, so remember the explicit values.
	commandLine := make(map[string]string)
	flags.Visit(func(option *flag.Flag) { commandLine[option.Name] = option.Value.String() })
	if _, explicit := commandLine["config"]; !explicit {
		configPath = os.Getenv("ANT2OAI_CONFIG")
	}

	for _, option := range configOptions {
		if err := flags.Set(option.name, flags.Lookup(option.name).DefValue); err != nil {
			return errors.New("could not restore configuration defaults")
		}
	}

	if configPath != "" {
		if err := applyJSONConfig(flags, configPath, configOptions, proxyAPIKey); err != nil {
			return err
		}
	}

	for _, option := range configOptions {
		if _, explicit := commandLine[option.name]; explicit {
			continue
		}
		if value, exists := os.LookupEnv(option.environment); exists {
			if err := flags.Set(option.name, value); err != nil {
				return fmt.Errorf("invalid %s", option.environment)
			}
		}
	}

	for name, value := range commandLine {
		if err := flags.Set(name, value); err != nil {
			return fmt.Errorf("invalid -%s", name)
		}
	}
	return nil
}

func parseModelMap(source string) (map[string]string, error) {
	var modelMap map[string]string
	if err := json.Unmarshal([]byte(source), &modelMap); err != nil || modelMap == nil {
		return nil, errors.New("model-map must be a JSON object of non-empty model IDs")
	}
	for incoming, upstream := range modelMap {
		if strings.TrimSpace(incoming) == "" || strings.TrimSpace(upstream) == "" {
			return nil, errors.New("model-map must contain non-empty model IDs")
		}
	}
	return modelMap, nil
}

func validateSettings(cfg config) error {
	if cfg.authMode != "api-key" && cfg.authMode != "chatgpt" {
		return errors.New("auth-mode must be api-key or chatgpt")
	}
	if strings.TrimSpace(cfg.account) == "" || len(cfg.account) > maxAccountLabelBytes ||
		strings.ContainsAny(cfg.account, "\r\n\t") {
		return errors.New("account must be a non-empty label of at most 64 bytes " +
			"without control characters")
	}

	if cfg.tokenLimitField != "max_tokens" && cfg.tokenLimitField != "max_completion_tokens" {
		return errors.New("token-limit-field must be max_tokens or max_completion_tokens")
	}
	if cfg.requestTimeout <= 0 || cfg.upstreamIdle <= 0 {
		return errors.New("timeouts must be positive")
	}
	if cfg.maxBodyBytes < 1 || cfg.maxBodyBytes > maxBodyLimitBytes {
		return errors.New("max-body-bytes must be between 1 and 268435456")
	}
	if cfg.maxConcurrent < 1 || cfg.maxConcurrent > maxConcurrencyLimit ||
		cfg.requestsPerMinute < 1 {
		return errors.New("max-concurrent must be between 1 and 4096; " +
			"requests-per-minute must be positive")
	}
	return nil
}

func normalizeUpstreamBaseURL(baseURL string, usesChatGPT bool) (string, error) {
	upstreamURL, err := url.Parse(baseURL)
	if err != nil || upstreamURL.Hostname() == "" {
		return "", errors.New("upstream-base-url must be an absolute HTTP or HTTPS URL")
	}
	if upstreamURL.Scheme != "http" && upstreamURL.Scheme != "https" {
		return "", errors.New("upstream-base-url must use HTTP or HTTPS")
	}
	hasQueryOrFragment := upstreamURL.RawQuery != "" || upstreamURL.ForceQuery ||
		upstreamURL.Fragment != ""
	if upstreamURL.User != nil || hasQueryOrFragment {
		return "", errors.New("upstream-base-url must not contain credentials, " +
			"a query, or a fragment")
	}

	normalized := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(normalized, "/chat/completions") {
		return "", errors.New("upstream-base-url must end before /chat/completions")
	}
	if usesChatGPT && normalized != chatGPTResource {
		return "", errors.New("ChatGPT OAuth requires https://api.openai.com/v1; " +
			"unset OPENAI_BASE_URL or override upstream-base-url")
	}
	return normalized, nil
}

// parseListenAddresses validates a comma-separated address list. One proxy key
// covers all addresses, so one non-loopback address requires it.
func parseListenAddresses(listenList string, hasProxyKey bool) ([]string, error) {
	var addresses []string
	for _, entry := range strings.Split(listenList, ",") {
		address := strings.TrimSpace(entry)
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("listen address %q must have the form host:port", address)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 0 || portNumber > maxPortNumber {
			return nil, fmt.Errorf("listen address %q needs a port from 0 to 65535", address)
		}
		if !isLoopbackHost(host) && !hasProxyKey {
			return nil, errors.New("a proxy key is required when listening outside loopback; " +
				"set ANT2OAI_API_KEY or JSON proxy_api_key")
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

func (cfg config) usesChatGPT() bool {
	return cfg.authMode == "chatgpt"
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func (cfg config) upstreamModel(incoming string) string {
	if model, exists := cfg.modelMap[incoming]; exists {
		return model
	}
	if cfg.model != "" {
		return cfg.model
	}
	return incoming
}
