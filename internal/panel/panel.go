package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrWriteNotSent marks a panel mutation rejected by local validation before
// any request that could change a client was sent.
var ErrWriteNotSent = errors.New("panel write was not sent")

type HTTPStatusError struct {
	StatusCode int
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("panel returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("panel returned HTTP %d", e.StatusCode)
}

func IsAuthorizationDenied(err error) bool {
	var statusErr *HTTPStatusError
	return errors.As(err, &statusErr) && (statusErr.StatusCode == 401 || statusErr.StatusCode == 403)
}

type WriteRejectedError struct {
	Message string
}

func (e *WriteRejectedError) Error() string {
	return fmt.Sprintf("panel client update failed: %s", e.Message)
}

func IsDefinitiveWriteRejection(err error) bool {
	var rejected *WriteRejectedError
	return errors.As(err, &rejected) || IsAuthorizationDenied(err)
}

type Client struct {
	Email               string                     `json:"email"`
	SubscriptionID      string                     `json:"-"`
	Enabled             bool                       `json:"enabled"`
	HasEnabled          bool                       `json:"hasEnabled"`
	ExpiryMS            int64                      `json:"expiryTime"`
	TotalBytes          int64                      `json:"totalBytes"`
	UsedBytes           int64                      `json:"-"`
	UploadBytes         int64                      `json:"uploadBytes"`
	DownloadBytes       int64                      `json:"downloadBytes"`
	HasTrafficBreakdown bool                       `json:"-"`
	InboundIDs          []int64                    `json:"inboundIds"`
	Fields              map[string]json.RawMessage `json:"-"`
}

type Info struct {
	Version       string   `json:"version"`
	Authenticated bool     `json:"authenticated"`
	Capabilities  []string `json:"capabilities"`
	NodeSync      string   `json:"nodeSync"`
}

type ClientAPI interface {
	CheckConnection(context.Context) (Info, error)
	ListClients(context.Context) ([]Client, error)
	GetClient(context.Context, string) (Client, error)
	GetClientBySubscriptionID(context.Context, string) (Client, error)
	ValidateSetQuota(context.Context, Client, bool, bool) error
	ValidateResetTraffic(context.Context) error
	Adjust(context.Context, string, int, int64) error
	SetQuota(context.Context, Client, int64, int64, bool, bool) error
	ResetTraffic(context.Context, string) error
}
