package service

import (
	"context"
	"errors"
	"strings"
)

var ErrClientLookupRejected = errors.New("无法查询账号，请检查订阅 ID")

type ClientLookupResult struct {
	SubscriptionID string   `json:"subscriptionId"`
	ClientEmail    string   `json:"-"`
	Snapshot       Snapshot `json:"snapshot"`
}

func (a *App) LookupClient(ctx context.Context, subscriptionID string) (ClientLookupResult, error) {
	subscriptionID, err := normalizeSubscriptionID(subscriptionID)
	if err != nil {
		return ClientLookupResult{}, ErrClientLookupRejected
	}
	client, err := a.Panel.GetClientBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		return ClientLookupResult{}, ErrClientLookupRejected
	}
	if client.Email == "" || len(client.InboundIDs) == 0 {
		return ClientLookupResult{}, ErrClientLookupRejected
	}
	if client.SubscriptionID != "" {
		subscriptionID = client.SubscriptionID
	}
	return ClientLookupResult{SubscriptionID: subscriptionID, ClientEmail: client.Email, Snapshot: clientSnapshot(client)}, nil
}

func normalizeSubscriptionID(subscriptionID string) (string, error) {
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" || len(subscriptionID) > 256 || strings.ContainsAny(subscriptionID, "/\\?#") || strings.Contains(subscriptionID, "://") {
		return "", ErrClientLookupRejected
	}
	return subscriptionID, nil
}
