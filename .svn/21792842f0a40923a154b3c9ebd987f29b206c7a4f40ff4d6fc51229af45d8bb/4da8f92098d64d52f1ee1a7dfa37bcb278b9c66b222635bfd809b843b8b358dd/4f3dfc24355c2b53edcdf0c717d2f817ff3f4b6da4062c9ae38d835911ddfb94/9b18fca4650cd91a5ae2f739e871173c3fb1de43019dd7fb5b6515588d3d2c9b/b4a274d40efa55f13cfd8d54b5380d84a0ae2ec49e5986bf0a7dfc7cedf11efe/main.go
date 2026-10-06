package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

// cliproxy_invoke_host calls back into the host. The host owns the response
// buffer, so every non-NULL ptr it hands back must go to cliproxy_release_host.
static int cliproxy_invoke_host(const cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void cliproxy_release_host(const cliproxy_host_api* host, void* ptr, size_t len) {
	if (host == NULL || host->free_buffer == NULL || ptr == NULL) {
		return;
	}
	host->free_buffer(ptr, len);
}

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const turnStateHeader = "X-Codex-Turn-State"

const selectedAuthMetadataKey = "selected_auth_id"

const selectedAuthIndexMetadataKey = "selected_auth_index"

const logPrefix = "[codex-turn-state] "

const (
	roleProbe    = "probe"
	roleBusiness = "business"
)

const runtimeOverrideFileName = "runtime.json"

var state = pluginState{
	config:  defaultConfig(),
	cookies: make(map[string]*routeCookieEntry),
}

var hostAPI unsafe.Pointer

func hostAPIAvailable() bool {
	return atomic.LoadPointer(&hostAPI) != nil
}

func hostCall(method string, request []byte) ([]byte, error) {
	raw := atomic.LoadPointer(&hostAPI)
	if raw == nil {
		return nil, fmt.Errorf("host API unavailable: this plugin was initialised without one")
	}
	host := (*C.cliproxy_host_api)(raw)

	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var requestPtr *C.uint8_t
	if len(request) > 0 {
		requestPtr = (*C.uint8_t)(unsafe.Pointer(&request[0]))
	}

	var response C.cliproxy_buffer
	rc := C.cliproxy_invoke_host(host, cMethod, requestPtr, C.size_t(len(request)), &response)

	runtime.KeepAlive(request)

	if response.ptr != nil {
		defer C.cliproxy_release_host(host, response.ptr, response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host call %s failed with code %d", method, int(rc))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, nil
	}
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}

type decisionCounters struct {
	Harvest int64 `json:"harvest"`

	Steer int64 `json:"steer"`
	Pass  int64 `json:"pass"`
	Skip  int64 `json:"skip"`
}

type pluginState struct {
	mu     sync.Mutex
	config pluginConfig

	cookies map[string]*routeCookieEntry

	cookiesDirty   bool
	cookiesFlushed time.Time

	counts   decisionCounters
	countsAt time.Time

	configErrors []string
}

type pluginConfig struct {
	CloudMint cloudMintConfig `yaml:"cloud_mint"`

	Role string `yaml:"role"`

	StoreDir string `yaml:"store_dir"`

	TemplateLength int `yaml:"template_length"`
	ReplaceLength  int `yaml:"replace_length"`

	TTLSeconds int `yaml:"ttl_seconds"`

	DryRun bool `yaml:"dry_run"`

	BlockDegraded bool `yaml:"block_degraded"`

	LogDecisions bool `yaml:"log_decisions"`

	Models []string `yaml:"models"`

	ProbeAccounts []string `yaml:"probe_accounts"`

	ProbeProxies []string `yaml:"probe_proxies"`

	ProbeProxiesRotating []string `yaml:"probe_proxies_rotating"`

	ProbeManagementKey string `yaml:"probe_management_key"`

	ProbeBaseURL string `yaml:"probe_base_url"`
}

const defaultProbeBaseURL = "http://127.0.0.1:8317"

func defaultConfig() pluginConfig {
	return pluginConfig{
		CloudMint:      defaultCloudMintConfig(),
		Role:           "",
		StoreDir:       "",
		TemplateLength: 292,
		ReplaceLength:  312,

		TTLSeconds:   3900,
		DryRun:       false,
		LogDecisions: true,
		ProbeBaseURL: defaultProbeBaseURL,
	}
}

func (c pluginConfig) isProbe() bool {
	return strings.EqualFold(strings.TrimSpace(c.Role), roleProbe)
}

func (c pluginConfig) ttl() time.Duration {
	return time.Duration(c.TTLSeconds) * time.Second
}

func maskProxyURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil || parsed.Host == "" {
		return "<unparsable proxy url>"
	}
	if parsed.User == nil {
		return parsed.String()
	}

	stripped := *parsed
	stripped.User = nil
	out := stripped.String()
	marker := parsed.Scheme + "://"
	if !strings.HasPrefix(out, marker) {

		return "<unparsable proxy url>"
	}
	return marker + "***@" + out[len(marker):]
}

func secretPresence(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unset"
	}
	return "set"
}

func maskProxyURLs(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		out = append(out, maskProxyURL(value))
	}
	return out
}

var proxySchemes = map[string]bool{"http": true, "https": true, "socks5": true, "socks5h": true}

func normaliseProbeScope(accounts, models, proxies, rotating []string) ([]string, []string, []string, []string, []string) {
	var problems []string

	cleanAccounts := make([]string, 0, len(accounts))
	for _, raw := range accounts {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}

		if !strings.HasPrefix(strings.ToLower(name), "codex-") || !strings.HasSuffix(strings.ToLower(name), ".json") {
			problems = append(problems, fmt.Sprintf("probe_accounts: %q is not a Codex credential filename (expected codex-*.json)", name))
			continue
		}
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			problems = append(problems, fmt.Sprintf("probe_accounts: %q contains a path component", name))
			continue
		}
		cleanAccounts = append(cleanAccounts, name)
	}

	cleanModels := make([]string, 0, len(models))
	for _, raw := range models {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, " \t\r\n") {
			problems = append(problems, fmt.Sprintf("models: %q contains whitespace", name))
			continue
		}
		cleanModels = append(cleanModels, name)
	}

	cleanProxies, proxyProblems := normaliseProxyList(proxies, "probe_proxies")
	problems = append(problems, proxyProblems...)
	cleanRotating, rotatingProblems := normaliseProxyList(rotating, "probe_proxies_rotating")
	problems = append(problems, rotatingProblems...)

	return cleanAccounts, cleanModels, cleanProxies, cleanRotating, problems
}

func normaliseProxyList(proxies []string, field string) ([]string, []string) {
	var problems []string
	clean := make([]string, 0, len(proxies))
	for index, raw := range proxies {
		candidate := strings.TrimSpace(raw)
		if candidate == "" {
			continue
		}
		parsed, errParse := url.Parse(candidate)
		switch {
		case errParse != nil || parsed.Host == "":

			problems = append(problems, fmt.Sprintf("%s[%d]: not a valid URL", field, index))
			continue
		case !proxySchemes[strings.ToLower(parsed.Scheme)]:
			problems = append(problems, fmt.Sprintf("%s: unsupported scheme %q in %s (want http, https, socks5 or socks5h)", field, parsed.Scheme, maskProxyURL(candidate)))
			continue
		}
		clean = append(clean, candidate)
	}
	return clean, problems
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registerRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor        bool `json:"request_interceptor"`
	ResponseInterceptor       bool `json:"response_interceptor"`
	StreamChunkInterceptor    bool `json:"response_stream_interceptor"`
	WebSocketResponseObserver bool `json:"websocket_response_observer"`
	ManagementAPI             bool `json:"management_api"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}

	if host != nil {
		atomic.StorePointer(&hostAPI, unsafe.Pointer(host))
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	currentCloudMintService().close()

	flushObservationsNow()

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.cookiesDirty && state.config.StoreDir != "" {
		if err := writeRouteCookiePool(state.config.StoreDir, state.cookies, time.Now(), state.config.ttl()); err != nil {
			log.Printf(logPrefix+"pool flush on shutdown failed: %v", err)
		}
	}
	state.cookies = make(map[string]*routeCookieEntry)
	state.cookiesDirty = false
	state.cookiesFlushed = time.Time{}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodRequestInterceptBefore:

		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodRequestInterceptAfter:
		return interceptAfterAuth(request)
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponse(request)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return interceptStreamChunk(request)
	case pluginabi.MethodWebSocketResponseEvent:
		return observeWebSocketEvent(request)
	case pluginabi.MethodManagementRegister:
		return managementRegister(request)
	case pluginabi.MethodManagementHandle:
		return managementHandle(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req registerRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if req.SchemaVersion < 2 {
		return fmt.Errorf("codex-turn-state requires host schema version 2 or newer")
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}

	if err := cfg.CloudMint.validate(); err != nil {
		return err
	}

	if ov, okOverride := readRuntimeOverride(strings.TrimSpace(cfg.StoreDir)); okOverride {
		if ov.Role != nil {
			cfg.Role = *ov.Role
		}
		if ov.DryRun != nil {
			cfg.DryRun = *ov.DryRun
		}
	}

	role := strings.ToLower(strings.TrimSpace(cfg.Role))
	switch role {
	case "":
		role = roleBusiness
	case roleProbe, roleBusiness:
	default:
		return fmt.Errorf("role must be %q or %q, got %q", roleProbe, roleBusiness, cfg.Role)
	}
	cfg.Role = role
	cfg.StoreDir = strings.TrimSpace(cfg.StoreDir)

	cfg.ProbeManagementKey = strings.TrimSpace(cfg.ProbeManagementKey)

	if cfg.ProbeBaseURL = strings.TrimSpace(cfg.ProbeBaseURL); cfg.ProbeBaseURL == "" {
		cfg.ProbeBaseURL = defaultProbeBaseURL
	}

	if cfg.TemplateLength < 1 {
		return fmt.Errorf("template_length must be greater than zero")
	}
	if cfg.ReplaceLength < 1 {
		return fmt.Errorf("replace_length must be greater than zero")
	}
	if cfg.TemplateLength == cfg.ReplaceLength {
		return fmt.Errorf("template_length and replace_length must differ, both are %d", cfg.TemplateLength)
	}
	if cfg.TTLSeconds < 1 {
		return fmt.Errorf("ttl_seconds must be greater than zero")
	}

	if cfg.isProbe() && cfg.StoreDir == "" {
		return fmt.Errorf("role %q requires store_dir", roleProbe)
	}

	var scopeProblems []string
	cfg.ProbeAccounts, cfg.Models, cfg.ProbeProxies, cfg.ProbeProxiesRotating, scopeProblems =
		normaliseProbeScope(cfg.ProbeAccounts, cfg.Models, cfg.ProbeProxies, cfg.ProbeProxiesRotating)

	scopeSource := "config.yaml"
	if saved, errScope := loadProbeScope(cfg.StoreDir); errScope != nil {
		scopeProblems = append(scopeProblems,
			"probe scope file unreadable, falling back to config.yaml: "+errScope.Error())
	} else if saved != nil {
		var savedProblems []string
		cfg.ProbeAccounts, cfg.Models, cfg.ProbeProxies, cfg.ProbeProxiesRotating, savedProblems =
			normaliseProbeScope(saved.Accounts, saved.Models, saved.Proxies, saved.Rotating)
		scopeProblems = append(scopeProblems, savedProblems...)
		scopeSource = scopeFileName + " (saved " + saved.UpdatedAt + ")"
	}

	state.mu.Lock()

	cloudChanged := state.config.CloudMint != cfg.CloudMint || state.config.DryRun != cfg.DryRun || state.config.Role != cfg.Role
	cleared, _ := swapConfigLocked(cfg)
	state.cookies = loadRouteCookiePool(cfg.StoreDir)
	state.cookiesDirty = false
	state.cookiesFlushed = time.Time{}
	state.configErrors = scopeProblems
	state.mu.Unlock()
	if cloudChanged {
		resetCloudMintService()
	}

	loadObservations(cfg.StoreDir)

	pool := "pool kept"
	if cleared {
		pool = "pool cleared"
	}

	log.Printf(logPrefix+"configured role=%s store_dir=%q template_length=%d replace_length=%d ttl_seconds=%d dry_run=%t models=%d probe_accounts=%d probe_proxies=%d probe_proxies_rotating=%d probe_base_url=%q probe_management_key=%s scope_from=%s (%s)",
		cfg.Role, cfg.StoreDir, cfg.TemplateLength, cfg.ReplaceLength, cfg.TTLSeconds, cfg.DryRun,
		len(cfg.Models), len(cfg.ProbeAccounts), len(cfg.ProbeProxies), len(cfg.ProbeProxiesRotating), cfg.ProbeBaseURL,
		secretPresence(cfg.ProbeManagementKey), scopeSource, pool)
	for _, problem := range scopeProblems {

		log.Printf(logPrefix+"config error (probe scope, not fatal): %s", problem)
	}
	return nil
}

func poolInvalidatedBy(oldCfg, newCfg pluginConfig) bool {
	return oldCfg.StoreDir != newCfg.StoreDir
}

func swapConfigLocked(cfg pluginConfig) (cleared, roleChanged bool) {
	cleared = poolInvalidatedBy(state.config, cfg)
	roleChanged = !strings.EqualFold(state.config.Role, cfg.Role)
	state.config = cfg
	if cleared {
		state.cookies = make(map[string]*routeCookieEntry)
		state.cookiesDirty = false
		state.cookiesFlushed = time.Time{}
	}

	if roleChanged {
		state.counts = decisionCounters{}
		state.countsAt = time.Now()
	}
	return cleared, roleChanged
}

func pluginRegistration() registration {

	capabilities := registrationCapability{
		RequestInterceptor:        true,
		ManagementAPI:             true,
		ResponseInterceptor:       true,
		StreamChunkInterceptor:    true,
		WebSocketResponseObserver: true,
	}

	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Codex Cloud Mint",
			Version:          "0.3.3-ws-chain",
			Author:           "arden-aaai",
			GitHubRepository: "https://github.com/arden-aaai/cpa-plugin-codex-turn-state",
			ConfigFields: []pluginapi.ConfigField{

				{
					Name:        "role",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{roleProbe, roleBusiness},
					Description: "Whether this process rewrites requests. \"business\" merges the pool's best live __cflb/__oailb pair into attributable Codex requests; \"probe\" leaves every request exactly as it found it. Empty means business. Both roles collect pairs and observe serving states off upstream responses -- role does not switch that off.",
				},
				{
					Name:        "store_dir",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Directory holding the route-cookie pool file (route-cookies.json) and the other plugin-owned documents (runtime.json, probe-scope.json, observations.json). Required for role=probe.",
				},
				{
					Name:        "template_length",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Turn-state length classifying a NORMAL serving state (default 292). An anchor, not a whitelist: each bucket learns its own recurring signature. Observation only -- nothing is stored or substituted off it.",
				},
				{
					Name:        "replace_length",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Turn-state length classifying a DEGRADED serving state (default 312). Observation only -- nothing is stored or substituted off it.",
				},
				{
					Name:        "ttl_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "How long a pooled __cflb/__oailb pair stays usable, measured from when it was last seen and shortened by the pair's own declared deadline (default 3600, matching the upstream's declared one-hour Max-Age/Expires).",
				},
				{
					Name:        "dry_run",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Log decisions without rewriting the outgoing Cookie header.",
				},
				{
					Name:        "log_decisions",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Emit one log line per harvest or steer decision.",
				},
				{
					Name:        "models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Official model ids the probe uses for its minting payload. Recorded so the running config and the probe script cannot drift apart; the pair itself is model-agnostic.",
				},
				{
					Name:        "probe_accounts",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Credential filenames the probe run may borrow. PROBE SCOPE ONLY: the business path never reads this, and one usable credential is enough -- a minted pair is not bound to the account that minted it.",
				},
				{
					Name:        "probe_proxies",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Ordered exits the probe tries per bucket, applied to the probed account's own proxy_url. PROBE SCOPE ONLY; never read by the business path. May contain credentials, so it is masked in every log line; the status document shows it in the clear at the operator's explicit request.",
				},
				{
					Name:        "probe_proxies_rotating",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Exits whose address changes on every connection (a residential gateway). PROBE SCOPE ONLY; never read by the business path. Separate from probe_proxies because a rotating entry is re-dialed on a 312 -- the next request is a different address -- while a static one is not. Masked in every log line.",
				},
				{
					Name:        "probe_management_key",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Bearer the probe runner sends to /v0/management/*, for the two read-only calls that list the accounts and download one token. PROBE SCOPE ONLY; never read by the business path. It exists so the dashboard needs no key from the operator, and it is NEVER displayed anywhere, masked or otherwise -- not on the status page, not in the config response, not in a log line (which reports only set/unset).",
				},
				{
					Name:        "probe_base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Where the probe runner sends the above (default http://127.0.0.1:8317, CPA's own loopback listener). PROBE SCOPE ONLY; never read by the business path. Not a secret.",
				},
				{
					Name:        "cloud_mint",
					Type:        pluginapi.ConfigFieldTypeObject,
					Description: "云端打票（默认关闭）：enabled/url/proxy_url/proxy_env/key_env/transport/gateway/ticket_length/ttl_seconds/wait_ms/timeout_ms。密钥只从环境变量读取；冷启动短等待，未就绪返回 503。",
				},
			},
		},
		Capabilities: capabilities,
	}
}

func interceptAfterAuth(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	if cfg.isProbe() {
		return noop()
	}

	authID := metadataString(req.Metadata, selectedAuthMetadataKey)
	authIndex := metadataString(req.Metadata, selectedAuthIndexMetadataKey)
	model := pickModel(req.Model, req.RequestedModel)
	value := headerValue(req.Headers, turnStateHeader)

	rememberRequestAuth(req.RequestID, authID)
	if cfg.CloudMint.Enabled {
		return okEnvelope(interceptCloudMint(req, cfg))
	}

	if authID == "" && model != "" {
		if sole, _, errSole := soleEnabledCodexAuth(); errSole == nil && sole != "" {
			authID = sole
		}
	}

	codex := false
	if authID != "" || authIndex != "" {
		if verified, resolved := selectedAuthIsCodex(authID, authIndex); resolved {
			codex = verified
		} else {
			codex = looksCodexAuthID(authID)
		}
	}
	if !codex {
		if value != "" || authID != "" {
			logDecision("skip", authID, model, len(value), "request not attributable to a Codex account")
		}
		return noop()
	}
	cloudRememberRequest(req, pluginapi.RequestInterceptResponse{})

	now := time.Now()
	state.mu.Lock()
	set, pairKey, haveCookies := state.bestRouteCookieLocked(now, cfg.ttl())
	state.mu.Unlock()
	if !haveCookies {
		logDecision("pass", authID, model, len(value), "no live route-cookie pair in the pool")
		return noop()
	}

	current := headerValue(req.Headers, "Cookie")
	merged := mergeRouteCookies(current, set.pairs)
	if merged == current {

		rememberUnchangedRoute(req.RequestID, pairKey)
		logDecision("pass", authID, model, len(value), "route cookies already current")
		return noop()
	}

	if cfg.DryRun {
		logDecision("steer", authID, model, len(value), "route cookies ready but withheld (dry_run)")
		return noop()
	}

	markRequestSteered(req.RequestID, pairKey)
	logDecision("steer", authID, model, len(value), "route cookies merged")

	out := pluginapi.RequestInterceptResponse{}
	out.ClearHeaders = append(out.ClearHeaders, "Cookie")
	out.Headers = http.Header{}
	out.Headers.Set("Cookie", merged)
	cloudRememberRequest(req, out)
	return okEnvelope(out)
}

func interceptResponse(raw []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	if cloudHasRequestLog(req.RequestID) || headerValue(req.ResponseHeaders, turnStateHeader) != "" {
		cloudLogResponse(req.RequestID, req.ResponseHeaders, "")
		cloudLogResponseBody(req.RequestID, req.Body)
	}
	harvestFromResponse(cfg, req.ResponseHeaders, req.Metadata, pickModel(req.Model, req.RequestedModel), req.RequestID)

	if cfg.BlockDegraded && len(req.Body) > 0 {
		if entry, found := recallModelScan(req.RequestID); found && entry.model != "" {
			if served, ok := servedModelFromJSONBody(req.Body); ok && served != entry.model {
				recordDowngrade(entry.authID, entry.model, served, len(headerValue(req.ResponseHeaders, turnStateHeader)), entry.steered)
				logDecision("block", entry.authID, entry.model, 0, "degraded response withheld; served="+served)
				if entry.steered && entry.pairKey != "" {
					state.mu.Lock()
					state.markRouteCookieOutcomeLocked(entry.pairKey, observationLimited, time.Now())
					state.mu.Unlock()
				}
				return okEnvelope(pluginapi.ResponseInterceptResponse{Body: degradedErrorBody(entry.model, served)})
			}
		}
	}
	return okEnvelope(pluginapi.ResponseInterceptResponse{})
}

func interceptStreamChunk(raw []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if req.ChunkIndex != pluginapi.StreamChunkHeaderInitIndex {
		cloudLogStreamChunk(req.RequestID, req.Body)

		if streamBlocked(req.RequestID) {
			return okEnvelope(pluginapi.StreamChunkInterceptResponse{DropChunk: true})
		}
		if served, mismatched := noteServedModelChunk(req); mismatched {
			state.mu.Lock()
			block := state.config.BlockDegraded
			state.mu.Unlock()
			if block {
				blockStream(req.RequestID)
				model := pickModel(req.Model, req.RequestedModel)
				logDecision("block", "", model, 0, "degraded response withheld; served="+served)
				return okEnvelope(pluginapi.StreamChunkInterceptResponse{Body: degradedStreamEvent(model, served)})
			}
		}
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	state.mu.Lock()
	cfg := state.config
	state.mu.Unlock()

	if cloudHasRequestLog(req.RequestID) || headerValue(req.ResponseHeaders, turnStateHeader) != "" {
		cloudLogResponse(req.RequestID, req.ResponseHeaders, "")
	}
	harvestFromResponse(cfg, req.ResponseHeaders, req.Metadata, pickModel(req.Model, req.RequestedModel), req.RequestID)
	return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
}

func observeWebSocketEvent(raw []byte) ([]byte, error) {
	var event pluginapi.WebSocketResponseEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	if cloudObserveWSChain(event) {
		return okEnvelope(struct{}{})
	}
	if event.EventType == "error" || event.EventType == "response.failed" {
		cloudRecordLog("WS 异常", "账号 #%s · %s · 请求模型 %s", cloudFingerprint(event.AuthID), event.EventType, cloudSafeLabel(pickModel(event.Model, event.RequestedModel)))
	}
	return okEnvelope(struct{}{})
}

type pendingAuthEntry struct {
	authID string

	steered bool

	pairKey string
	seenAt  time.Time
}

var pendingAuth = struct {
	mu   sync.Mutex
	byID map[string]pendingAuthEntry
}{byID: make(map[string]pendingAuthEntry)}

const (
	pendingAuthTTL = 15 * time.Minute

	pendingAuthMax = 4096
)

func rememberRequestAuth(requestID, authID string) {
	if requestID == "" {
		return
	}
	now := time.Now()
	pendingAuth.mu.Lock()
	defer pendingAuth.mu.Unlock()
	if authID == "" {
		delete(pendingAuth.byID, requestID)
		return
	}
	if len(pendingAuth.byID) >= pendingAuthMax {
		for key, entry := range pendingAuth.byID {
			if now.Sub(entry.seenAt) > pendingAuthTTL {
				delete(pendingAuth.byID, key)
			}
		}
	}

	pendingAuth.byID[requestID] = pendingAuthEntry{authID: authID, seenAt: now}
}

func markRequestSteered(requestID, pairKey string) {
	if requestID == "" {
		return
	}
	pendingAuth.mu.Lock()
	defer pendingAuth.mu.Unlock()
	if entry, ok := pendingAuth.byID[requestID]; ok {
		entry.steered = true
		entry.pairKey = pairKey
		pendingAuth.byID[requestID] = entry
	}
}

func recallRequestRecord(requestID string) (authID string, steered bool, pairKey string) {
	if requestID == "" {
		return "", false, ""
	}
	pendingAuth.mu.Lock()
	defer pendingAuth.mu.Unlock()
	entry, ok := pendingAuth.byID[requestID]
	if !ok {
		return "", false, ""
	}
	delete(pendingAuth.byID, requestID)
	if time.Since(entry.seenAt) > pendingAuthTTL {
		return "", false, ""
	}
	return entry.authID, entry.steered, entry.pairKey
}

type pendingModelScanEntry struct {
	authID  string
	model   string
	tsLen   int
	steered bool
	pairKey string
	seenAt  time.Time
}

var pendingModelScans = struct {
	mu   sync.Mutex
	byID map[string]pendingModelScanEntry
}{byID: make(map[string]pendingModelScanEntry)}

const pendingModelScanMaxChunk = 8

func rememberModelScan(requestID, authID, model string, tsLen int, steered bool, pairKey string) {
	if requestID == "" || authID == "" || model == "" {
		return
	}
	now := time.Now()
	pendingModelScans.mu.Lock()
	defer pendingModelScans.mu.Unlock()
	if len(pendingModelScans.byID) >= pendingAuthMax {
		for key, entry := range pendingModelScans.byID {
			if now.Sub(entry.seenAt) > pendingAuthTTL {
				delete(pendingModelScans.byID, key)
			}
		}
	}
	pendingModelScans.byID[requestID] = pendingModelScanEntry{
		authID: authID, model: model, tsLen: tsLen, steered: steered, pairKey: pairKey, seenAt: now,
	}
}

func recallModelScan(requestID string) (pendingModelScanEntry, bool) {
	pendingModelScans.mu.Lock()
	defer pendingModelScans.mu.Unlock()
	entry, ok := pendingModelScans.byID[requestID]
	if ok {
		delete(pendingModelScans.byID, requestID)
		if time.Since(entry.seenAt) > pendingAuthTTL {
			return pendingModelScanEntry{}, false
		}
	}
	return entry, ok
}

func dropModelScan(requestID string) {
	pendingModelScans.mu.Lock()
	delete(pendingModelScans.byID, requestID)
	pendingModelScans.mu.Unlock()
}

var blockedStreams = struct {
	mu  sync.Mutex
	ids map[string]time.Time
}{ids: map[string]time.Time{}}

func blockStream(requestID string) {
	if requestID == "" {
		return
	}
	now := time.Now()
	blockedStreams.mu.Lock()
	defer blockedStreams.mu.Unlock()
	if len(blockedStreams.ids) >= pendingAuthMax {
		for id, at := range blockedStreams.ids {
			if now.Sub(at) > pendingAuthTTL {
				delete(blockedStreams.ids, id)
			}
		}
	}
	blockedStreams.ids[requestID] = now
}

func streamBlocked(requestID string) bool {
	if requestID == "" {
		return false
	}
	blockedStreams.mu.Lock()
	defer blockedStreams.mu.Unlock()
	at, ok := blockedStreams.ids[requestID]
	if !ok {
		return false
	}
	if time.Since(at) > pendingAuthTTL {
		delete(blockedStreams.ids, requestID)
		return false
	}
	return true
}

func noteServedModelChunk(req pluginapi.StreamChunkInterceptRequest) (served string, mismatched bool) {
	if req.RequestID == "" {
		return "", false
	}
	if req.ChunkIndex > pendingModelScanMaxChunk {
		dropModelScan(req.RequestID)
		return "", false
	}
	served, ok := servedModelFromChunk(req.Body)
	if !ok {
		return "", false
	}
	entry, found := recallModelScan(req.RequestID)
	if !found {
		return "", false
	}
	if served == entry.model {

		return "", false
	}
	recordDowngrade(entry.authID, entry.model, served, entry.tsLen, entry.steered)
	logDecision("downgrade", entry.authID, entry.model, entry.tsLen, "served="+served)
	if entry.steered && entry.pairKey != "" {

		state.mu.Lock()
		state.markRouteCookieOutcomeLocked(entry.pairKey, observationLimited, time.Now())
		state.mu.Unlock()
	}
	return served, true
}

func servedModelFromChunk(body []byte) (string, bool) {
	const needle = `"model":"`
	i := bytes.Index(body, []byte(needle))
	if i < 0 {
		return "", false
	}
	rest := body[i+len(needle):]
	j := bytes.IndexByte(rest, '"')
	if j <= 0 {
		return "", false
	}
	return string(rest[:j]), true
}

func servedModelFromJSONBody(body []byte) (string, bool) {
	i := bytes.Index(body, []byte(`"model"`))
	if i < 0 {
		return "", false
	}
	rest := bytes.TrimSpace(body[i+len(`"model"`):])
	if len(rest) < 2 || rest[0] != ':' {
		return "", false
	}
	rest = bytes.TrimSpace(rest[1:])
	if len(rest) < 2 || rest[0] != '"' {
		return "", false
	}
	j := bytes.IndexByte(rest[1:], '"')
	if j <= 0 || j > 96 {
		return "", false
	}
	return string(rest[1 : 1+j]), true
}

func degradedStreamEvent(requested, served string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"status": "failed",
			"error": map[string]any{
				"code":    "degraded_model_blocked",
				"message": "upstream served " + served + " instead of requested " + requested,
			},
		},
	})
	return []byte("event: response.failed\ndata: " + string(payload) + "\n\n")
}

func degradedErrorBody(requested, served string) []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    "degraded_model_blocked",
			"message": "upstream served " + served + " instead of requested " + requested,
		},
	})
	return body
}

func harvestFromResponse(cfg pluginConfig, headers http.Header, metadata map[string]any, model, requestID string) {
	value := headerValue(headers, turnStateHeader)
	now := time.Now()
	cookies := routeCookiesFromResponseHeaders(headers, now)

	relayedAuth, steered, pairKey := recallRequestRecord(requestID)

	authID := metadataString(metadata, selectedAuthMetadataKey)
	if authID == "" {

		authID = relayedAuth
	}

	if authID == "" && model != "" && len(value) > 0 && cfg.isProbe() {
		if sole, _, errSole := soleEnabledCodexAuth(); errSole == nil && sole != "" {
			authID = sole
		}
	}

	recordObservation(cfg, authID, model, len(value), steered)

	if routeCookieDeletion(headers, now) {
		state.mu.Lock()
		state.revokeRouteCookieLocked(pairKey, now)
		state.mu.Unlock()
	} else if len(cookies.pairs) > 0 {

		state.mu.Lock()
		state.noteRouteCookiesLocked(cookies, "")
		state.mu.Unlock()
		logDecision("harvest", authID, model, len(value), "route-cookie pair pooled")
	}

	if steered && pairKey != "" {
		kind := classifyObservation(cfg, len(value))
		state.mu.Lock()
		state.markRouteCookieOutcomeLocked(pairKey, kind, now)
		state.mu.Unlock()
	}

	rememberModelScan(requestID, authID, model, len(value), steered, pairKey)
}

type runtimeOverride struct {
	Role   *string `json:"role,omitempty"`
	DryRun *bool   `json:"dry_run,omitempty"`
}

func readRuntimeOverride(dir string) (runtimeOverride, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return runtimeOverride{}, false
	}
	data, errRead := os.ReadFile(filepath.Join(dir, runtimeOverrideFileName))
	if errRead != nil {
		return runtimeOverride{}, false
	}
	var ov runtimeOverride
	if errUnmarshal := json.Unmarshal(data, &ov); errUnmarshal != nil {
		log.Printf(logPrefix+"ignoring malformed %s: %v", runtimeOverrideFileName, errUnmarshal)
		return runtimeOverride{}, false
	}
	if ov.Role == nil && ov.DryRun == nil {
		return runtimeOverride{}, false
	}
	return ov, true
}

func writeRuntimeOverride(dir string, role string, dryRun bool) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("store_dir is empty, cannot persist the dashboard override")
	}
	ov := runtimeOverride{Role: &role, DryRun: &dryRun}
	data, errMarshal := json.MarshalIndent(ov, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	return atomicWrite(filepath.Join(dir, runtimeOverrideFileName), append(data, '\n'))
}

const scopeFileName = "probe-scope.json"

type probeScope struct {
	Accounts []string `json:"probe_accounts"`
	Models   []string `json:"models"`
	Proxies  []string `json:"probe_proxies"`

	Rotating  []string `json:"probe_proxies_rotating,omitempty"`
	UpdatedAt string   `json:"updated_at"`
}

func loadProbeScope(dir string) (*probeScope, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	data, errRead := os.ReadFile(filepath.Join(dir, scopeFileName))
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return nil, nil
		}
		return nil, errRead
	}
	var scope probeScope
	if errUnmarshal := json.Unmarshal(data, &scope); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return &scope, nil
}

func writeProbeScope(dir string, scope probeScope) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("store_dir is empty, so there is nowhere to save the probe scope")
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	data, errMarshal := json.MarshalIndent(scope, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	return atomicWrite(filepath.Join(dir, scopeFileName), append(data, '\n'))
}

func pickModel(model, requestedModel string) string {
	if resolved := strings.TrimSpace(model); resolved != "" {
		return resolved
	}
	return strings.TrimSpace(requestedModel)
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func looksCodexAuthID(authID string) bool {
	name := strings.ToLower(strings.TrimSpace(authID))
	return strings.HasPrefix(name, "codex-") && strings.HasSuffix(name, ".json")
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func logDecision(decision, authID, model string, valueLen int, reason string) {
	if decision == "" {
		return
	}
	state.mu.Lock()
	enabled := state.config.LogDecisions

	switch decision {
	case "harvest":
		state.counts.Harvest++
	case "steer":
		state.counts.Steer++
	case "pass":
		state.counts.Pass++
	case "skip":
		state.counts.Skip++
	}
	state.mu.Unlock()
	if !enabled {
		return
	}
	log.Printf(logPrefix+"%s auth=%s model=%s len=%d (%s)", decision, orDash(authID), orDash(model), valueLen, reason)
}

func noop() ([]byte, error) {
	return okEnvelope(pluginapi.RequestInterceptResponse{})
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
