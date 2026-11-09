package ldc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL       *url.URL
	publicBaseURL *url.URL
	clientID      string
	clientSecret  string
	privateKey    ed25519.PrivateKey
	httpClient    *http.Client
}

type Order struct {
	OutTradeNo string
	TradeNo    string
	Money      string
	Status     int
}

type RefundOutcomeUnknownError struct{ Err error }

func (e *RefundOutcomeUnknownError) Error() string {
	return "LDC refund outcome is unknown: " + e.Err.Error()
}
func (e *RefundOutcomeUnknownError) Unwrap() error { return e.Err }

func New(baseURL, publicBaseURL, clientID, clientSecret, privateKey string) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, errors.New("LDC base URL must be a valid HTTPS URL")
	}
	basePath := strings.TrimRight(base.Path, "/")
	if basePath != "" && basePath != "/epay" {
		return nil, errors.New("LDC base URL must be the gateway origin or end in /epay")
	}
	base.Path = ""
	base.RawPath = ""
	public, err := url.Parse(strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"))
	if err != nil || public.Scheme != "https" || public.Host == "" {
		return nil, errors.New("public base URL must be a valid HTTPS URL")
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil, errors.New("LDC client ID or client secret is missing")
	}
	parsedKey, err := ParseEd25519PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL:       base,
		publicBaseURL: public,
		clientID:      strings.TrimSpace(clientID),
		clientSecret:  clientSecret,
		privateKey:    parsedKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// ParseEd25519PrivateKey accepts a PKCS#8 PEM key, a base64 encoded 32-byte
// Ed25519 seed, or a base64 encoded 64-byte Ed25519 private key.
func ParseEd25519PrivateKey(encoded string) (ed25519.PrivateKey, error) {
	value := strings.TrimSpace(encoded)
	if value == "" {
		return nil, errors.New("LDC Ed25519 private key is missing")
	}
	if strings.HasPrefix(value, "-----BEGIN") {
		block, _ := pem.Decode([]byte(value))
		if block == nil {
			return nil, errors.New("LDC Ed25519 private key PEM is invalid")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("LDC private key must be a PKCS#8 Ed25519 key")
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok || len(key) != ed25519.PrivateKeySize {
			return nil, errors.New("LDC private key must be an Ed25519 key")
		}
		return append(ed25519.PrivateKey(nil), key...), nil
	}
	compact := strings.Join(strings.Fields(value), "")
	var raw []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		raw, err = encoding.DecodeString(compact)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, errors.New("LDC Ed25519 private key must be PKCS#8 PEM or base64 encoded")
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return append(ed25519.PrivateKey(nil), raw...), nil
	default:
		return nil, errors.New("base64 LDC Ed25519 private key must decode to 32 or 64 bytes")
	}
}

func ValidateEd25519PrivateKey(value string) error {
	_, err := ParseEd25519PrivateKey(value)
	return err
}

func (c *Client) NotifyURL() string {
	return c.publicURL("/api/public/ldc/notify").String()
}

func (c *Client) ReturnURL(orderNo string) string {
	return c.publicURL("/?o=" + url.QueryEscape(orderNo)).String()
}

func (c *Client) ResumePaymentURL(orderNo string) string {
	resume := &url.URL{Scheme: "https", Host: "credit.linux.do", Path: "/paying"}
	query := resume.Query()
	query.Set("order_no", orderNo)
	resume.RawQuery = query.Encode()
	if link, err := c.validatePaymentURL(resume); err == nil {
		return link
	}
	return ""
}

func (c *Client) CreatePayment(ctx context.Context, outTradeNo, name, money string) (string, error) {
	if len(c.NotifyURL()) > 100 || len(c.ReturnURL(outTradeNo)) > 100 {
		return "", errors.New("LDC notify_url or return_url exceeds the 100-character limit")
	}
	values := url.Values{
		"client_id":    {c.clientID},
		"type":         {"ldcpay"},
		"out_trade_no": {outTradeNo},
		"money":        {money},
		"order_name":   {name},
		"notify_url":   {c.NotifyURL()},
		"return_url":   {c.ReturnURL(outTradeNo)},
	}
	values.Set("sign", Sign(values, c.clientSecret, c.privateKey))

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/epay/pay/submit.php"), strings.NewReader(values.Encode()))
	if err != nil {
		return "", fmt.Errorf("build LDC payment request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json, text/plain, */*")
	response, err := c.httpClient.Do(request)
	if err != nil && response == nil {
		return "", fmt.Errorf("submit LDC payment: %w", err)
	}
	if response == nil {
		return "", errors.New("LDC returned no response")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return "", fmt.Errorf("read LDC payment response: %w", readErr)
	}
	if location := strings.TrimSpace(response.Header.Get("Location")); location != "" {
		resolved, parseErr := url.Parse(location)
		if parseErr == nil {
			return c.validatePaymentURL(c.baseURL.ResolveReference(resolved))
		}
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return "", fmt.Errorf("LDC redirect response %d did not include a valid payment URL", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("LDC payment request returned HTTP %d: %s", response.StatusCode, safeBody(body))
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("LDC did not return a payment redirect: %s", safeBody(body))
	}
	if message := responseError(result); message != "" {
		return "", fmt.Errorf("LDC payment request failed: %s", message)
	}
	if link := responseURL(result); link != "" {
		resolved, parseErr := url.Parse(link)
		if parseErr == nil {
			return c.validatePaymentURL(c.baseURL.ResolveReference(resolved))
		}
	}
	return "", errors.New("LDC accepted the request without returning a payment redirect URL")
}

// Sign creates the official LDC request signature: sorted non-empty fields
// (excluding sign), followed by the client secret, signed with Ed25519.
func Sign(values url.Values, clientSecret string, privateKey ed25519.PrivateKey) string {
	names := make([]string, 0, len(values))
	for name, entries := range values {
		if name == "sign" || len(entries) == 0 || entries[0] == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var data strings.Builder
	for i, name := range names {
		if i > 0 {
			data.WriteByte('&')
		}
		data.WriteString(name)
		data.WriteByte('=')
		data.WriteString(values.Get(name))
	}
	data.WriteString(clientSecret)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(data.String())))
}

func (c *Client) QueryOrder(ctx context.Context, outTradeNo string) (Order, error) {
	values := url.Values{
		"act":          {"order"},
		"pid":          {c.clientID},
		"key":          {c.clientSecret},
		"out_trade_no": {outTradeNo},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/epay/api.php")+"?"+values.Encode(), nil)
	if err != nil {
		return Order{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Order{}, fmt.Errorf("query LDC order: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Order{}, fmt.Errorf("read LDC order response: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		return Order{}, ErrOrderNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Order{}, fmt.Errorf("LDC order query returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Code       int    `json:"code"`
		Message    string `json:"msg"`
		TradeNo    string `json:"trade_no"`
		OutTradeNo string `json:"out_trade_no"`
		PID        string `json:"pid"`
		Type       string `json:"type"`
		Money      string `json:"money"`
		Status     int    `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return Order{}, fmt.Errorf("decode LDC order response: %w", err)
	}
	if result.Code != 1 {
		if strings.Contains(result.Message, "不存在") || strings.Contains(result.Message, "已完成") {
			return Order{}, ErrOrderNotFound
		}
		return Order{}, fmt.Errorf("LDC order query failed: %s", result.Message)
	}
	// The shared order API documents `epay` in its response even for the
	// official ldcpay submission protocol.
	if result.OutTradeNo != outTradeNo || result.PID != c.clientID || (result.Type != "epay" && result.Type != "ldcpay") {
		return Order{}, errors.New("LDC order query returned mismatched order details")
	}
	if strings.TrimSpace(result.TradeNo) == "" {
		return Order{}, errors.New("LDC order query returned no trade number")
	}
	if _, err := MoneyCents(result.Money); err != nil {
		return Order{}, errors.New("LDC order query returned an invalid amount")
	}
	return Order{OutTradeNo: result.OutTradeNo, TradeNo: result.TradeNo, Money: result.Money, Status: result.Status}, nil
}

func (c *Client) Refund(ctx context.Context, tradeNo, money string) error {
	if strings.TrimSpace(tradeNo) == "" {
		return errors.New("LDC trade number is required for a refund")
	}
	if _, err := MoneyCents(money); err != nil {
		return errors.New("refund amount is invalid")
	}
	values := url.Values{
		"pid":      {c.clientID},
		"key":      {c.clientSecret},
		"trade_no": {tradeNo},
		"money":    {money},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/epay/api.php"), strings.NewReader(values.Encode()))
	if err != nil {
		return fmt.Errorf("build LDC refund request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return &RefundOutcomeUnknownError{Err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return &RefundOutcomeUnknownError{Err: err}
	}
	if response.StatusCode >= 500 {
		return &RefundOutcomeUnknownError{Err: fmt.Errorf("LDC returned HTTP %d", response.StatusCode)}
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return &RefundOutcomeUnknownError{Err: fmt.Errorf("LDC returned redirect HTTP %d", response.StatusCode)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("LDC refund request returned HTTP %d: %s", response.StatusCode, safeBody(body))
	}
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"msg"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return &RefundOutcomeUnknownError{Err: fmt.Errorf("decode LDC refund response: %w", err)}
	}
	if result.Code != 1 {
		return fmt.Errorf("LDC refund request failed: %s", result.Message)
	}
	return nil
}

var ErrOrderNotFound = errors.New("LDC order not found")

func MoneyCents(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
		return 0, errors.New("invalid amount")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("invalid amount")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > (int64(^uint64(0)>>1)-99)/100 {
		return 0, errors.New("invalid amount")
	}
	fraction := "00"
	if len(parts) == 2 {
		if len(parts[1]) == 0 || len(parts[1]) > 2 {
			return 0, errors.New("invalid amount")
		}
		fraction = parts[1]
		if len(fraction) == 1 {
			fraction += "0"
		}
	}
	decimal, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, errors.New("invalid amount")
	}
	return whole*100 + decimal, nil
}

func (c *Client) endpoint(path string) string {
	base := *c.baseURL
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawPath = ""
	return base.String()
}

func (c *Client) publicURL(pathAndQuery string) *url.URL {
	base := *c.publicBaseURL
	parsed, _ := url.Parse(pathAndQuery)
	base.Path = strings.TrimRight(base.Path, "/") + parsed.Path
	base.RawQuery = parsed.RawQuery
	base.Fragment = ""
	return &base
}

func (c *Client) validatePaymentURL(candidate *url.URL) (string, error) {
	if candidate == nil || candidate.Scheme != "https" || candidate.Host == "" || candidate.User != nil || candidate.Fragment != "" {
		return "", errors.New("LDC returned an invalid payment redirect URL")
	}
	if !strings.EqualFold(candidate.Hostname(), c.baseURL.Hostname()) && !strings.EqualFold(candidate.Hostname(), "credit.linux.do") {
		return "", errors.New("LDC returned a payment URL on an unexpected host")
	}
	return candidate.String(), nil
}

func responseURL(result any) string {
	var visit func(any) string
	visit = func(value any) string {
		switch typed := value.(type) {
		case map[string]any:
			for _, key := range []string{"url", "payurl", "pay_url", "payment_url", "redirect_url", "redirect"} {
				if candidate, ok := typed[key].(string); ok && strings.TrimSpace(candidate) != "" {
					return strings.TrimSpace(candidate)
				}
			}
			for _, key := range []string{"data", "result"} {
				if candidate, ok := typed[key]; ok {
					if link := visit(candidate); link != "" {
						return link
					}
				}
			}
		case string:
			if strings.HasPrefix(typed, "https://") {
				return typed
			}
		}
		return ""
	}
	return visit(result)
}

func responseError(result any) string {
	data, ok := result.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"error_msg", "error"} {
		if message, ok := data[key].(string); ok && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
	}
	code, hasCode := data["code"]
	if hasCode && fmt.Sprint(code) == "1" {
		return ""
	}
	for _, key := range []string{"msg", "message"} {
		if message, ok := data[key].(string); ok && strings.TrimSpace(message) != "" {
			message = strings.TrimSpace(message)
			if strings.Contains(strings.ToLower(message), "success") || strings.Contains(message, "成功") || strings.EqualFold(message, "ok") {
				return ""
			}
			return message
		}
	}
	return ""
}

func safeBody(body []byte) string {
	clean := strings.TrimSpace(string(bytes.ToValidUTF8(body, []byte("�"))))
	if len(clean) > 300 {
		clean = clean[:300]
	}
	return clean
}
