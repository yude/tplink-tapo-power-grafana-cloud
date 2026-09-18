package tapo

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	initialHost       = "https://wap.tplinkcloud.com"
	accountAppName    = "TP-Link_Tapo_Android"
	accountAppVersion = "3.4.451"
	thingAppName      = "TP-Link_Tapo_Android"
	thingAppVersion   = "3.13.818"
	mfaRequiredCode   = -20677
)

var (
	//go:embed tplink-cloud-server-ca.pem
	tpLinkCloudServerCA []byte
	thingHostSuffix     = "-app-server.iot.i.tplinkcloud.com"
	readOnlyMethods     = map[string]struct{}{
		"get_current_power":      {},
		"get_device_usage":       {},
		"get_emeter_data":        {},
		"get_emeter_vgain_igain": {},
		"get_energy_data":        {},
		"get_energy_usage":       {},
	}
)

type MFACodeProvider interface {
	WaitCode(context.Context) (string, error)
}

type Client struct {
	username    string
	password    string
	terminalID  string
	mfaProvider MFACodeProvider
	publicHTTP  *http.Client
	thingHTTP   *http.Client
	now         func() time.Time
	logger      *slog.Logger
	session     *session
}

type session struct {
	RegionalURL  string
	Token        string
	RefreshToken string
}

type Thing struct {
	ThingName      string `json:"thingName"`
	DeviceID       string `json:"deviceId"`
	Nickname       string `json:"nickname"`
	Alias          string `json:"alias"`
	Model          string `json:"model"`
	DeviceModel    string `json:"deviceModel"`
	Category       string `json:"category"`
	DeviceType     string `json:"deviceType"`
	AppServerURLV2 string `json:"appServerUrlV2"`
	Status         any    `json:"status"`
}

func (t Thing) ID() string {
	if t.ThingName != "" {
		return t.ThingName
	}
	return t.DeviceID
}

func (t Thing) Name() string {
	if t.Nickname != "" {
		return decodeBase64Text(t.Nickname)
	}
	if t.Alias != "" {
		return decodeBase64Text(t.Alias)
	}
	if t.ModelName() != "" {
		return t.ModelName()
	}
	return "unknown"
}

func (t Thing) ModelName() string {
	if t.Model != "" {
		return t.Model
	}
	return t.DeviceModel
}

func (t Thing) Kind() string {
	if t.Category != "" {
		return t.Category
	}
	return t.DeviceType
}

type APIError struct {
	Operation string
	Code      int
	Message   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s failed (%d): %s", e.Operation, e.Code, e.Message)
}

func NewClient(username, password, terminalID string, provider MFACodeProvider, timeout time.Duration, logger *slog.Logger) (*Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(tpLinkCloudServerCA) {
		return nil, errors.New("parse embedded TP-Link Cloud Server CA")
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
		},
	}
	redirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		username:    username,
		password:    password,
		terminalID:  terminalID,
		mfaProvider: provider,
		publicHTTP:  &http.Client{Timeout: timeout, CheckRedirect: redirect},
		thingHTTP:   &http.Client{Timeout: timeout, CheckRedirect: redirect, Transport: transport},
		now:         time.Now,
		logger:      logger,
	}, nil
}

func (c *Client) Authenticate(ctx context.Context) error {
	if c.session != nil && c.session.Token != "" {
		return nil
	}
	status, err := c.signedPost(ctx, c.publicHTTP, initialHost,
		"/api/v2/account/getAccountStatusAndUrl", map[string]any{
			"appType": accountAppName, "cloudUserName": c.username,
		}, "", accountAppName, accountAppVersion)
	if err != nil {
		return err
	}
	if err := checkAPI(status, "regional endpoint discovery"); err != nil {
		return err
	}
	regionalURL, err := normalizePublicHost(stringAt(status, "result", "appServerUrl"))
	if err != nil {
		return fmt.Errorf("regional endpoint: %w", err)
	}

	loginBody := map[string]any{
		"appType":            accountAppName,
		"appVersion":         accountAppVersion,
		"cloudPassword":      c.password,
		"cloudUserName":      c.username,
		"platform":           "Android",
		"refreshTokenNeeded": true,
		"supportBindAccount": false,
		"terminalUUID":       c.terminalID,
		"terminalName":       "tapo-grafana-collector",
		"terminalMeta":       "kubernetes",
	}
	login, err := c.signedPost(ctx, c.publicHTTP, regionalURL,
		"/api/v2/account/login", loginBody, "", accountAppName, accountAppVersion)
	if err != nil {
		return err
	}
	mfaRequired := apiCode(login) == mfaRequiredCode
	if mfaRequired {
		c.info("TP-Link terminal verification required")
		login, err = c.completeMFA(ctx, regionalURL, login)
		if err != nil {
			return err
		}
	}
	if err := checkAPI(login, "TP-Link login"); err != nil {
		return err
	}
	token := stringAt(login, "result", "token")
	if token == "" {
		return errors.New("TP-Link login succeeded without a token")
	}
	c.session = &session{
		RegionalURL:  regionalURL,
		Token:        token,
		RefreshToken: stringAt(login, "result", "refreshToken"),
	}
	c.info("TP-Link authentication completed", "terminal_verification", mfaRequired)
	return nil
}

func (c *Client) ResetSession() {
	c.session = nil
}

func (c *Client) completeMFA(ctx context.Context, regionalURL string, login map[string]any) (map[string]any, error) {
	processID := firstNonEmpty(
		stringAt(login, "result", "MFAProcessId"),
		stringAt(login, "result", "mfaProcessId"),
	)
	if processID == "" {
		return nil, errors.New("TP-Link requested terminal verification without an MFA process ID")
	}
	if c.mfaProvider == nil {
		return nil, errors.New("TP-Link terminal verification required; configure TAPO_MFA_CODE_FILE")
	}
	sent, err := c.signedPost(ctx, c.publicHTTP, regionalURL,
		"/api/v2/account/getEmailVC4TerminalMFA", map[string]any{
			"appType":       accountAppName,
			"cloudPassword": c.password,
			"cloudUserName": c.username,
			"terminalUUID":  c.terminalID,
		}, "", accountAppName, accountAppVersion)
	if err != nil {
		return nil, err
	}
	if err := checkAPI(sent, "email terminal verification code request"); err != nil {
		return nil, err
	}
	c.info("TP-Link verification email requested; waiting for code")
	code, err := c.mfaProvider.WaitCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for TP-Link email verification code: %w", err)
	}
	c.info("TP-Link verification code received; completing terminal verification")
	verified, err := c.signedPost(ctx, c.publicHTTP, regionalURL,
		"/api/v2/account/checkMFACodeAndLogin", map[string]any{
			"appType":             accountAppName,
			"cloudUserName":       c.username,
			"code":                code,
			"MFAProcessId":        processID,
			"MFAType":             2,
			"terminalBindEnabled": true,
		}, "", accountAppName, accountAppVersion)
	if err != nil {
		return nil, err
	}
	return verified, nil
}

func (c *Client) info(message string, args ...any) {
	if c.logger != nil {
		c.logger.Info(message, args...)
	}
}

func (c *Client) ListThings(ctx context.Context) ([]Thing, error) {
	if err := c.Authenticate(ctx); err != nil {
		return nil, err
	}
	serviceResponse, err := c.signedPost(ctx, c.publicHTTP, c.session.RegionalURL,
		"/api/v2/common/getAppServiceUrlByCloudUserName", map[string]any{
			"cloudUserName": c.username,
			"serviceIds": []string{
				"nbu.iot-app-server.app-v2",
				"nbu.iot-cloud-gateway.app-v2",
				"nbu.iot-security.appdevice-v2",
			},
		}, c.session.Token, thingAppName, thingAppVersion)
	if err != nil {
		return nil, err
	}
	if err := checkAPI(serviceResponse, "Thing API service discovery"); err != nil {
		return nil, err
	}
	appServer := ""
	for _, item := range sliceAt(serviceResponse, "result", "serviceList") {
		service, _ := item.(map[string]any)
		if stringValue(service["serviceId"]) == "nbu.iot-app-server.app-v2" {
			appServer = stringValue(service["serviceUrl"])
		}
	}
	appServer, err = normalizeThingHost(appServer)
	if err != nil {
		return nil, err
	}

	things := make([]Thing, 0)
	for page := 0; page < 100; page++ {
		query := url.Values{
			"page":                            {strconv.Itoa(page)},
			"pageSize":                        {"100"},
			"includeKasaShareDevices":         {"false"},
			"includePcDevice":                 {"false"},
			"includeMatterDevice":             {"false"},
			"includeExternalVendorDeviceInfo": {"false"},
		}.Encode()
		var response struct {
			ErrorCode any     `json:"error_code"`
			ErrorMsg  string  `json:"errorMsg"`
			Message   string  `json:"msg"`
			Data      []Thing `json:"data"`
			Total     int     `json:"total"`
		}
		if err := c.thingJSON(ctx, http.MethodGet, appServer+"/v2/things?"+query, nil, &response); err != nil {
			return nil, err
		}
		if code := flexibleInt(response.ErrorCode); code != 0 {
			return nil, &APIError{Operation: "Thing API device listing", Code: code, Message: firstNonEmpty(response.Message, response.ErrorMsg)}
		}
		for i := range response.Data {
			if response.Data[i].AppServerURLV2 == "" {
				response.Data[i].AppServerURLV2 = appServer
			}
		}
		things = append(things, response.Data...)
		if len(response.Data) < 100 || (response.Total > 0 && len(things) >= response.Total) {
			break
		}
	}
	return things, nil
}

func (c *Client) Call(ctx context.Context, thing Thing, method string, params any) (map[string]any, error) {
	if _, allowed := readOnlyMethods[method]; !allowed {
		return nil, fmt.Errorf("rejected non-read-only Tapo method %q", method)
	}
	if c.session == nil {
		return nil, errors.New("TP-Link session is not initialized")
	}
	host, err := normalizeThingHost(thing.AppServerURLV2)
	if err != nil {
		return nil, err
	}
	inner := map[string]any{"method": method}
	if params != nil {
		inner["params"] = params
	}
	body := map[string]any{
		"serviceId": "passthrough",
		"inputParams": map[string]any{
			"requestData": map[string]any{
				"method": "multipleRequest",
				"params": map[string]any{"requests": []any{inner}},
			},
		},
	}
	var response map[string]any
	endpoint := host + "/v1/things/" + url.PathEscape(thing.ID()) + "/services-sync"
	if err := c.thingJSON(ctx, http.MethodPost, endpoint, body, &response); err != nil {
		return nil, err
	}
	if code := apiCode(response); code != 0 {
		return nil, &APIError{Operation: "Thing API " + method, Code: code, Message: messageFrom(response)}
	}
	responseData := valueAt(response, "outputParams", "responseData")
	if text, ok := responseData.(string); ok {
		if err := json.Unmarshal([]byte(text), &responseData); err != nil {
			return nil, fmt.Errorf("decode Thing API responseData: %w", err)
		}
	}
	data, ok := responseData.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Thing API %s returned no responseData", method)
	}
	responses := sliceAt(data, "result", "responses")
	if len(responses) == 0 {
		if code := apiCode(data); code != 0 {
			return nil, &APIError{Operation: "Thing API " + method, Code: code, Message: messageFrom(data)}
		}
		if result, ok := data["result"].(map[string]any); ok {
			return result, nil
		}
		return data, nil
	}
	first, _ := responses[0].(map[string]any)
	if code := apiCode(first); code != 0 {
		return nil, &APIError{Operation: "Thing API " + method, Code: code, Message: messageFrom(first)}
	}
	result, ok := first["result"].(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return result, nil
}

func (c *Client) signedPost(ctx context.Context, client *http.Client, host, path string, body map[string]any, token, appName, appVersion string) (map[string]any, error) {
	host, err := normalizePublicHost(host)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	sig, err := sign(payload, path, c.now())
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"appName":  {appName},
		"appVer":   {appVersion},
		"brand":    {"TPLINK"},
		"locale":   {"en_US"},
		"model":    {"Kubernetes"},
		"netType":  {"wifi"},
		"ospf":     {"Android 14"},
		"termID":   {c.terminalID},
		"termMeta": {"Kubernetes"},
		"termName": {"tapo-grafana-collector"},
	}
	if token != "" {
		query.Set("token", token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+path+"?"+query.Encode(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Content-MD5", sig.ContentMD5)
	req.Header.Set("X-Authorization", sig.Authorization)
	var response map[string]any
	if err := doJSON(client, req, &response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) thingJSON(ctx context.Context, method, endpoint string, body any, target any) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if _, err := normalizeThingHost(parsed.Scheme + "://" + parsed.Host); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "ut|"+c.session.Token)
	req.Header.Set("app-cid", "app:"+thingAppName+":"+c.terminalID)
	req.Header.Set("x-app-name", thingAppName)
	req.Header.Set("x-app-version", thingAppVersion)
	req.Header.Set("x-term-id", c.terminalID)
	req.Header.Set("x-ospf", "Android 15")
	req.Header.Set("x-net-type", "wifi")
	req.Header.Set("x-strict", "0")
	req.Header.Set("x-locale", "en_US")
	req.Header.Set("x-app-brand", "TPLINK")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", thingAppName+"/"+thingAppVersion+"(Kubernetes/;Android 15)")
	return doJSON(c.thingHTTP, req, target)
}

func doJSON(client *http.Client, req *http.Request, target any) error {
	response, err := client.Do(req)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return fmt.Errorf("%s %s: %w", req.Method, requestURL(req), err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned HTTP %d%s", req.Method, requestURL(req), response.StatusCode, httpErrorDetails(body))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode %s %s response: %w", req.Method, requestURL(req), err)
	}
	return nil
}

func httpErrorDetails(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	details := make([]string, 0, 4)
	for _, key := range []string{"status", "code", "error_code", "errorCode", "error", "message", "msg", "errorMsg"} {
		value, exists := payload[key]
		if !exists {
			continue
		}
		var text string
		switch typed := value.(type) {
		case string:
			text = typed
		case float64:
			text = strconv.FormatFloat(typed, 'f', -1, 64)
		default:
			continue
		}
		text = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(text)
		if len(text) > 200 {
			text = text[:200]
		}
		details = append(details, key+"="+text)
	}
	if len(details) == 0 {
		return ""
	}
	return " (" + strings.Join(details, ", ") + ")"
}

func requestURL(req *http.Request) string {
	value := *req.URL
	value.RawQuery = ""
	value.ForceQuery = false
	value.Fragment = ""
	return value.Redacted()
}

func normalizePublicHost(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" {
		return "", errors.New("invalid TP-Link API host")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "tplinkcloud.com" && !strings.HasSuffix(host, ".tplinkcloud.com") {
		return "", fmt.Errorf("rejected unexpected TP-Link API host %q", host)
	}
	if strings.HasPrefix(host, "n-") {
		host = strings.TrimPrefix(host, "n-")
	}
	if strings.HasSuffix(host, "-wap.i.tplinkcloud.com") {
		host = strings.TrimSuffix(host, "-wap.i.tplinkcloud.com") + "-wap.tplinkcloud.com"
	}
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return "https://" + host, nil
}

func normalizeThingHost(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" || parsed.Port() != "" {
		return "", errors.New("invalid TP-Link Thing API host")
	}
	host := strings.ToLower(parsed.Hostname())
	if !strings.HasSuffix(host, thingHostSuffix) || strings.TrimSuffix(host, thingHostSuffix) == "" {
		return "", fmt.Errorf("rejected unexpected TP-Link Thing API host %q", host)
	}
	for _, char := range strings.TrimSuffix(host, thingHostSuffix) {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return "", fmt.Errorf("rejected unexpected TP-Link Thing API host %q", host)
		}
	}
	return "https://" + host, nil
}

func checkAPI(response map[string]any, operation string) error {
	if code := apiCode(response); code != 0 {
		return &APIError{Operation: operation, Code: code, Message: messageFrom(response)}
	}
	return nil
}

func apiCode(response map[string]any) int {
	if code := flexibleInt(response["error_code"]); code != 0 {
		return code
	}
	if result, ok := response["result"].(map[string]any); ok {
		return flexibleInt(result["errorCode"])
	}
	return flexibleInt(response["errorCode"])
}

func flexibleInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		value, _ := typed.Int64()
		return int(value)
	case string:
		value, _ := strconv.Atoi(typed)
		return value
	default:
		return 0
	}
}

func messageFrom(response map[string]any) string {
	if message := firstNonEmpty(stringValue(response["msg"]), stringValue(response["errorMsg"])); message != "" {
		return message
	}
	if result, ok := response["result"].(map[string]any); ok {
		return firstNonEmpty(stringValue(result["errorMsg"]), "unknown error")
	}
	return "unknown error"
}

func valueAt(value map[string]any, path ...string) any {
	var current any = value
	for _, key := range path {
		mapped, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = mapped[key]
	}
	return current
}

func stringAt(value map[string]any, path ...string) string {
	return stringValue(valueAt(value, path...))
}

func sliceAt(value map[string]any, path ...string) []any {
	items, _ := valueAt(value, path...).([]any)
	return items
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
