package panel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type API struct {
	baseURL *url.URL
	token   string
	cookie  string
	client  *http.Client
}

type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"msg"`
	Object  json.RawMessage `json:"obj"`
}

type openAPISpec struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]openAPISchema `json:"schemas"`
	} `json:"components"`
}

type openAPISchema struct {
	Ref        string                           `json:"$ref"`
	Required   []string                         `json:"required"`
	Type       string                           `json:"type"`
	Properties map[string]openAPISchemaProperty `json:"properties"`
	Items      *openAPISchemaProperty           `json:"items"`
}

type openAPISchemaProperty struct {
	Type  string                 `json:"type"`
	Items *openAPISchemaProperty `json:"items"`
}

func NewAPI(baseURL, token string, timeout time.Duration, tlsVerify bool, sessionCookie ...string) (*API, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("panel.base_url must be an absolute URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("panel.base_url must use http or https")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("panel.base_url cannot contain a query or fragment")
	}
	if timeout < time.Second || timeout > 2*time.Minute {
		return nil, errors.New("panel timeout is outside the supported range")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !tlsVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402: only allowed outside production by explicit config.
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	cookie := ""
	if len(sessionCookie) > 0 {
		cookie = strings.TrimSpace(sessionCookie[0])
	}
	return &API{baseURL: parsed, token: strings.TrimSpace(token), cookie: cookie, client: client}, nil
}

func (a *API) CheckConnection(ctx context.Context) (Info, error) {
	if a.token == "" {
		return Info{}, errors.New("panel API token is not configured")
	}
	var spec openAPISpec
	if err := a.getJSON(ctx, "panel/api/openapi.json", &spec); err != nil {
		return Info{}, fmt.Errorf("read panel OpenAPI: %w", err)
	}
	info := Info{Version: spec.Info.Version, Authenticated: true, NodeSync: "unknown"}
	for _, check := range []struct {
		path, method, name string
	}{
		{"/panel/api/server/status", "get", "server-status"},
		{"/panel/api/clients/list", "get", "client-list"},
		{"/panel/api/clients/get/{email}", "get", "client-get"},
		{"/panel/api/clients/get/subId/{subId}", "get", "client-sub-id-lookup"},
		{"/panel/api/clients/traffic/{email}", "get", "client-traffic-detail"},
		{"/panel/api/clients/bulkAdjust", "post", "client-bulk-adjust"},
		{"/panel/api/clients/update/{email}", "post", "client-update"},
		{"/panel/api/clients/resetTraffic/{email}", "post", "client-reset-traffic"},
	} {
		if _, ok := spec.Paths[check.path][check.method]; ok {
			info.Capabilities = append(info.Capabilities, check.name)
		}
	}
	if !hasCapability(info.Capabilities, "client-list") || !hasCapability(info.Capabilities, "client-get") {
		return info, errors.New("panel OpenAPI does not declare required client read APIs")
	}
	if !hasCapability(info.Capabilities, "client-bulk-adjust") {
		return info, errors.New("panel OpenAPI does not declare the verified client bulkAdjust API")
	}
	if err := validateBulkAdjustSchema(spec); err != nil {
		return info, fmt.Errorf("panel bulkAdjust request schema is not verified: %w", err)
	}
	var status envelope
	if err := a.getJSON(ctx, "panel/api/server/status", &status); err != nil {
		return info, fmt.Errorf("authenticate against panel server status: %w", err)
	}
	if !status.Success {
		return info, fmt.Errorf("panel rejected API token: %s", safeMessage(status.Message))
	}
	return info, nil
}

func (a *API) GetClient(ctx context.Context, email string) (Client, error) {
	if strings.TrimSpace(email) == "" {
		return Client{}, errors.New("client email is required")
	}
	path := "panel/api/clients/get/" + url.PathEscape(email)
	var result envelope
	if err := a.getJSON(ctx, path, &result); err != nil {
		return Client{}, err
	}
	if !result.Success {
		return Client{}, fmt.Errorf("panel client lookup failed: %s", safeMessage(result.Message))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result.Object, &fields); err != nil {
		return Client{}, fmt.Errorf("decode panel client: %w", err)
	}
	client, err := decodeClientPayload(fields, email)
	if err != nil {
		return Client{}, err
	}
	if !client.HasTrafficBreakdown {
		if up, down, ok := a.readClientTraffic(ctx, client.Email); ok {
			client.UploadBytes = up
			client.DownloadBytes = down
			client.HasTrafficBreakdown = true
		}
	}
	return client, nil
}

func (a *API) ListClients(ctx context.Context) ([]Client, error) {
	var result envelope
	if err := a.getJSON(ctx, "panel/api/clients/list", &result); err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, fmt.Errorf("panel client list failed: %s", safeMessage(result.Message))
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(result.Object, &rows); err != nil {
		return nil, fmt.Errorf("decode panel client list: %w", err)
	}
	clients := make([]Client, 0, len(rows))
	for _, fields := range rows {
		client, err := decodeClientPayload(fields, "")
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, nil
}

func (a *API) GetClientBySubscriptionID(ctx context.Context, subscriptionID string) (Client, error) {
	if strings.TrimSpace(subscriptionID) == "" {
		return Client{}, errors.New("panel subscription ID is required")
	}
	var result envelope
	path := "panel/api/clients/get/subId/" + url.PathEscape(subscriptionID)
	if err := a.getJSON(ctx, path, &result); err != nil {
		return Client{}, err
	}
	if !result.Success {
		return Client{}, fmt.Errorf("panel client lookup failed: %s", safeMessage(result.Message))
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(result.Object, &rows); err != nil {
		return Client{}, fmt.Errorf("decode panel client lookup: %w", err)
	}
	if len(rows) != 1 {
		return Client{}, errors.New("panel client subscription ID did not return one client")
	}
	client, err := decodeClientPayload(rows[0], "")
	if err != nil {
		return Client{}, err
	}
	if !client.HasTrafficBreakdown {
		if up, down, ok := a.readClientTraffic(ctx, client.Email); ok {
			client.UploadBytes = up
			client.DownloadBytes = down
			client.HasTrafficBreakdown = true
		}
	}
	return client, nil
}

func (a *API) readClientTraffic(ctx context.Context, email string) (int64, int64, bool) {
	var result envelope
	path := "panel/api/clients/traffic/" + url.PathEscape(email)
	if err := a.getJSON(ctx, path, &result); err != nil || !result.Success {
		return 0, 0, false
	}
	var traffic struct {
		Up   *int64 `json:"up"`
		Down *int64 `json:"down"`
	}
	if json.Unmarshal(result.Object, &traffic) != nil || traffic.Up == nil || traffic.Down == nil {
		return 0, 0, false
	}
	return *traffic.Up, *traffic.Down, true
}

func (a *API) Adjust(ctx context.Context, email string, days int, addBytes int64) error {
	if email == "" || (days == 0 && addBytes == 0) || days < 0 || addBytes < 0 {
		return fmt.Errorf("%w: invalid positive quota adjustment", ErrWriteNotSent)
	}
	if err := a.ValidateAdjust(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrWriteNotSent, err)
	}
	body := map[string]any{"emails": []string{email}, "addDays": days, "addBytes": addBytes}
	var result envelope
	if err := a.postJSON(ctx, "panel/api/clients/bulkAdjust", body, &result); err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("panel bulkAdjust failed: %s", safeMessage(result.Message))
	}
	var counts struct {
		Adjusted int `json:"adjusted"`
		Skipped  []struct {
			Email  string `json:"email"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(result.Object, &counts); err != nil {
		return fmt.Errorf("decode panel bulkAdjust result: %w", err)
	}
	if counts.Adjusted != 1 || len(counts.Skipped) > 0 {
		return fmt.Errorf("panel did not adjust the requested client")
	}
	return nil
}

func (a *API) SetQuota(ctx context.Context, client Client, expiryMS, totalBytes int64, setExpiry, setTotal bool) error {
	if len(client.Fields) == 0 || client.Email == "" || (!setExpiry && !setTotal) {
		return fmt.Errorf("%w: panel did not return a complete client record required for safe update", ErrWriteNotSent)
	}
	spec, err := a.readOpenAPISpec(ctx)
	if err != nil {
		return fmt.Errorf("%w: cannot confirm safe full client update: %w", ErrWriteNotSent, err)
	}
	clientSchema, err := validateSetQuotaSchema(spec, client, setExpiry, setTotal)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWriteNotSent, err)
	}
	fields := make(map[string]json.RawMessage, len(client.Fields))
	for key, value := range client.Fields {
		if property, supported := clientSchema.Properties[key]; supported {
			var normalized json.RawMessage
			var err error
			if key == "allowedIPs" {
				// Some panel versions return this field as JSON text even though Client expects []string.
				normalized, err = normalizeStringArrayField(value)
			} else {
				normalized, err = normalizeClientField(value, property)
			}
			if err != nil {
				return fmt.Errorf("%w: client field %s: %v", ErrWriteNotSent, key, err)
			}
			fields[key] = normalized
		}
	}
	if setExpiry {
		encoded, _ := json.Marshal(expiryMS)
		fields["expiryTime"] = encoded
	}
	if setTotal {
		encoded, _ := json.Marshal(totalBytes)
		fields["totalGB"] = encoded
	}
	var result envelope
	if err := a.postJSON(ctx, "panel/api/clients/update/"+url.PathEscape(client.Email), fields, &result); err != nil {
		return err
	}
	if !result.Success {
		return &WriteRejectedError{Message: safeMessage(result.Message)}
	}
	return nil
}

func normalizeClientField(raw json.RawMessage, schema openAPISchemaProperty) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("panel returned an empty client field")
	}
	switch schema.Type {
	case "array":
		if schema.Items != nil && schema.Items.Type == "string" {
			return normalizeStringArrayField(raw)
		}
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, errors.New("panel value does not match the declared array type")
		}
		return append(json.RawMessage(nil), raw...), nil
	case "string":
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return append(json.RawMessage(nil), raw...), nil
		}
		if number, ok := decodeJSONNumber(raw); ok {
			return json.Marshal(number.String())
		}
		var boolean bool
		if json.Unmarshal(raw, &boolean) == nil {
			return json.Marshal(strconv.FormatBool(boolean))
		}
		return nil, errors.New("panel value does not match the declared string type")
	case "integer", "number":
		if number, ok := decodeJSONNumber(raw); ok {
			if schema.Type == "integer" {
				if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
					return nil, errors.New("panel number is not a valid integer")
				}
			}
			return append(json.RawMessage(nil), raw...), nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("panel value does not match the declared numeric type")
		}
		value = strings.TrimSpace(value)
		if schema.Type == "integer" {
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				return nil, errors.New("panel string is not a valid integer")
			}
		} else if _, err := strconv.ParseFloat(value, 64); err != nil {
			return nil, errors.New("panel string is not a valid number")
		}
		return json.Marshal(json.Number(value))
	case "boolean":
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			return append(json.RawMessage(nil), raw...), nil
		}
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, errors.New("panel value does not match the declared boolean type")
		}
		value, err := strconv.ParseBool(strings.TrimSpace(encoded))
		if err != nil {
			return nil, errors.New("panel string is not a valid boolean")
		}
		return json.Marshal(value)
	default:
		return append(json.RawMessage(nil), raw...), nil
	}
}

func decodeJSONNumber(raw json.RawMessage) (json.Number, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	number, ok := value.(json.Number)
	return number, ok
}

func normalizeStringArrayField(raw json.RawMessage) (json.RawMessage, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err == nil {
		return append(json.RawMessage(nil), raw...), nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, errors.New("panel value is neither a string array nor a string-encoded array")
	}
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return json.Marshal([]string{})
	}
	if strings.HasPrefix(encoded, "[") {
		if err := json.Unmarshal([]byte(encoded), &values); err != nil {
			return nil, errors.New("panel string-encoded array is invalid")
		}
		return json.Marshal(values)
	}
	values = make([]string, 0)
	for _, item := range strings.Split(encoded, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return json.Marshal(values)
}

func (a *API) ValidateSetQuota(ctx context.Context, client Client, setExpiry, setTotal bool) error {
	var spec openAPISpec
	if err := a.getJSON(ctx, "panel/api/openapi.json", &spec); err != nil {
		return fmt.Errorf("cannot confirm safe full client update: %w", err)
	}
	_, err := validateSetQuotaSchema(spec, client, setExpiry, setTotal)
	return err
}

func (a *API) ValidateResetTraffic(ctx context.Context) error {
	spec, err := a.readOpenAPISpec(ctx)
	if err != nil {
		return err
	}
	if _, ok := spec.Paths["/panel/api/clients/resetTraffic/{email}"]["post"]; !ok {
		return errors.New("panel does not declare POST /panel/api/clients/resetTraffic/{email}")
	}
	if _, ok := spec.Paths["/panel/api/clients/traffic/{email}"]["get"]; !ok {
		return errors.New("panel does not declare GET /panel/api/clients/traffic/{email} required to verify a reset")
	}
	return nil
}

func (a *API) ResetTraffic(ctx context.Context, email string) error {
	if strings.TrimSpace(email) == "" {
		return fmt.Errorf("%w: client email is required", ErrWriteNotSent)
	}
	if err := a.ValidateResetTraffic(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrWriteNotSent, err)
	}
	var result envelope
	path := "panel/api/clients/resetTraffic/" + url.PathEscape(email)
	if err := a.postJSON(ctx, path, nil, &result); err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("panel client traffic reset failed: %s", safeMessage(result.Message))
	}
	return nil
}

func validateSetQuotaSchema(spec openAPISpec, client Client, setExpiry, setTotal bool) (openAPISchema, error) {
	if client.Email == "" || (!setExpiry && !setTotal) {
		return openAPISchema{}, errors.New("invalid full client update")
	}
	if _, ok := spec.Paths["/panel/api/clients/update/{email}"]["post"]; !ok {
		return openAPISchema{}, errors.New("panel does not declare the client update API")
	}
	clientSchema, ok := spec.Components.Schemas["Client"]
	if !ok || len(clientSchema.Required) == 0 || len(clientSchema.Properties) == 0 {
		return openAPISchema{}, errors.New("panel OpenAPI does not declare the client schema")
	}
	for _, field := range clientSchema.Required {
		if _, ok := client.Fields[field]; !ok {
			return openAPISchema{}, fmt.Errorf("panel client record is missing required field %q; safe update refused", field)
		}
	}
	for _, field := range clientSchema.Required {
		if _, ok := clientSchema.Properties[field]; !ok {
			return openAPISchema{}, fmt.Errorf("panel Client schema does not define required field %q", field)
		}
	}
	if setExpiry {
		if _, ok := clientSchema.Properties["expiryTime"]; !ok {
			return openAPISchema{}, errors.New("panel Client schema does not define expiryTime")
		}
	}
	if setTotal {
		if _, ok := clientSchema.Properties["totalGB"]; !ok {
			return openAPISchema{}, errors.New("panel Client schema does not define totalGB")
		}
	}
	return clientSchema, nil
}

func (a *API) ValidateAdjust(ctx context.Context) error {
	var spec openAPISpec
	if err := a.getJSON(ctx, "panel/api/openapi.json", &spec); err != nil {
		return fmt.Errorf("read panel OpenAPI: %w", err)
	}
	return validateBulkAdjustSchema(spec)
}

func validateBulkAdjustSchema(spec openAPISpec) error {
	rawOperation, ok := spec.Paths["/panel/api/clients/bulkAdjust"]["post"]
	if !ok {
		return errors.New("panel does not declare POST /panel/api/clients/bulkAdjust")
	}
	var operation struct {
		RequestBody struct {
			Content map[string]struct {
				Schema  openAPISchema   `json:"schema"`
				Example json.RawMessage `json:"example"`
			} `json:"content"`
		} `json:"requestBody"`
	}
	if err := json.Unmarshal(rawOperation, &operation); err != nil {
		return fmt.Errorf("decode bulkAdjust operation: %w", err)
	}
	request, ok := operation.RequestBody.Content["application/json"]
	if !ok {
		return errors.New("panel does not declare a JSON request body for bulkAdjust")
	}
	schema := request.Schema
	if schema.Ref != "" {
		const prefix = "#/components/schemas/"
		if !strings.HasPrefix(schema.Ref, prefix) {
			return errors.New("bulkAdjust request schema uses an unsupported reference")
		}
		var found bool
		schema, found = spec.Components.Schemas[strings.TrimPrefix(schema.Ref, prefix)]
		if !found {
			return errors.New("bulkAdjust request schema reference is missing")
		}
	}
	var example map[string]json.RawMessage
	if len(request.Example) > 0 {
		if err := json.Unmarshal(request.Example, &example); err != nil {
			return fmt.Errorf("decode bulkAdjust request example: %w", err)
		}
	}
	if !validBulkStringArray(schema, example, "emails") {
		return errors.New("bulkAdjust schema must declare emails as an array of strings")
	}
	if !validBulkInteger(schema, example, "addDays") {
		return errors.New("bulkAdjust schema must declare addDays as a number")
	}
	if !validBulkInteger(schema, example, "addBytes") {
		return errors.New("bulkAdjust schema must declare addBytes as a number")
	}
	return nil
}

func validBulkStringArray(schema openAPISchema, example map[string]json.RawMessage, field string) bool {
	if property, ok := schema.Properties[field]; ok {
		return property.Type == "array" && property.Items != nil && property.Items.Type == "string"
	}
	if len(schema.Properties) > 0 {
		return false
	}
	var values []string
	return len(example[field]) > 0 && json.Unmarshal(example[field], &values) == nil && len(values) > 0
}

func validBulkInteger(schema openAPISchema, example map[string]json.RawMessage, field string) bool {
	if property, ok := schema.Properties[field]; ok {
		return property.Type == "integer" || property.Type == "number"
	}
	if len(schema.Properties) > 0 {
		return false
	}
	var value int64
	return len(example[field]) > 0 && json.Unmarshal(example[field], &value) == nil
}

func (a *API) readOpenAPISpec(ctx context.Context) (openAPISpec, error) {
	var spec openAPISpec
	if err := a.getJSON(ctx, "panel/api/openapi.json", &spec); err != nil {
		return openAPISpec{}, fmt.Errorf("read panel OpenAPI: %w", err)
	}
	return spec, nil
}

func decodeClientPayload(fields map[string]json.RawMessage, requestedEmail string) (Client, error) {
	updateFields := fields
	decodedFields := fields
	if nested, ok := fields["client"]; ok {
		var clientFields map[string]json.RawMessage
		if err := json.Unmarshal(nested, &clientFields); err != nil {
			return Client{}, fmt.Errorf("decode panel client payload: %w", err)
		}
		decodedFields = make(map[string]json.RawMessage, len(fields)+len(clientFields))
		for key, value := range clientFields {
			decodedFields[key] = value
		}
		for key, value := range fields {
			if key != "client" {
				if _, exists := decodedFields[key]; !exists {
					decodedFields[key] = value
				}
			}
		}
		updateFields = clientFields
	}
	client, err := decodeClient(decodedFields, requestedEmail)
	if err != nil {
		return Client{}, err
	}
	client.Fields = updateFields
	return client, nil
}

func (a *API) getJSON(ctx context.Context, path string, target any) error {
	return a.requestJSON(ctx, http.MethodGet, path, nil, target)
}

func (a *API) postJSON(ctx context.Context, path string, body any, target any) error {
	return a.requestJSON(ctx, http.MethodPost, path, body, target)
}

func (a *API) requestJSON(ctx context.Context, method, path string, body, target any) error {
	u, err := url.JoinPath(a.baseURL.String(), path)
	if err != nil {
		return fmt.Errorf("build panel URL: %w", err)
	}
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode panel request: %w", err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return fmt.Errorf("create panel request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if a.cookie != "" {
		req.Header.Set("Cookie", a.cookie)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("panel request outcome is uncertain: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read panel response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var details struct {
			Msg     string `json:"msg"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(data, &details)
		message := details.Msg
		if message == "" {
			message = details.Message
		}
		if message == "" {
			message = details.Error
		}
		if message != "" {
			message = safeMessage(message)
		}
		return &HTTPStatusError{StatusCode: resp.StatusCode, Message: message}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode panel response: %w", err)
	}
	return nil
}

func decodeClient(fields map[string]json.RawMessage, requestedEmail string) (Client, error) {
	client := Client{Email: requestedEmail, Fields: fields}
	if err := json.Unmarshal(fields["email"], &client.Email); err != nil || client.Email == "" {
		return Client{}, errors.New("panel client record has no email")
	}
	for _, key := range []string{"subId", "subscriptionId"} {
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &client.SubscriptionID) == nil && client.SubscriptionID != "" {
			break
		}
	}
	if err := json.Unmarshal(fields["expiryTime"], &client.ExpiryMS); err != nil {
		return Client{}, errors.New("panel client expiry time is missing or invalid")
	}
	if err := json.Unmarshal(fields["totalGB"], &client.TotalBytes); err != nil {
		return Client{}, errors.New("panel client traffic quota is missing or invalid")
	}
	if raw, ok := fields["enable"]; ok {
		if err := json.Unmarshal(raw, &client.Enabled); err != nil {
			return Client{}, errors.New("panel client enabled flag is invalid")
		}
		client.HasEnabled = true
	}
	_ = json.Unmarshal(fields["inboundIds"], &client.InboundIDs)
	var traffic struct {
		Up   *int64 `json:"up"`
		Down *int64 `json:"down"`
	}
	if raw, ok := fields["traffic"]; ok && json.Unmarshal(raw, &traffic) == nil {
		if traffic.Up != nil {
			client.UploadBytes = *traffic.Up
		}
		if traffic.Down != nil {
			client.DownloadBytes = *traffic.Down
		}
		client.HasTrafficBreakdown = traffic.Up != nil && traffic.Down != nil
	}
	hasUpload, hasDownload := client.HasTrafficBreakdown, client.HasTrafficBreakdown
	if raw, ok := fields["up"]; ok {
		if json.Unmarshal(raw, &client.UploadBytes) == nil {
			hasUpload = true
		}
	}
	if raw, ok := fields["down"]; ok {
		if json.Unmarshal(raw, &client.DownloadBytes) == nil {
			hasDownload = true
		}
	}
	client.HasTrafficBreakdown = hasUpload && hasDownload
	if raw, ok := fields["usedTraffic"]; ok {
		_ = json.Unmarshal(raw, &client.UsedBytes)
	}
	if client.TotalBytes < 0 {
		return Client{}, errors.New("panel client has a negative traffic quota")
	}
	return client, nil
}

func hasCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

func safeMessage(message string) string {
	message = strings.TrimSpace(message)
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		return "unspecified panel error"
	}
	return message
}
