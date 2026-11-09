package panel

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Mock struct {
	mu      sync.Mutex
	clients map[string]Client
}

func NewMock() *Mock {
	now := time.Now().UTC()
	clients := map[string]Client{}
	for _, client := range []Client{
		{Email: "user001", SubscriptionID: "user001-sub", Enabled: true, HasEnabled: true, ExpiryMS: now.Add(10 * 24 * time.Hour).UnixMilli(), TotalBytes: 100 * 1024 * 1024 * 1024, UploadBytes: 30 * 1024 * 1024 * 1024, DownloadBytes: 50 * 1024 * 1024 * 1024, InboundIDs: []int64{1, 2, 3}},
		{Email: "expired-demo", SubscriptionID: "expired-demo-sub", Enabled: true, HasEnabled: true, ExpiryMS: now.Add(-5 * 24 * time.Hour).UnixMilli(), TotalBytes: 100 * 1024 * 1024 * 1024, UploadBytes: 10 * 1024 * 1024 * 1024, DownloadBytes: 20 * 1024 * 1024 * 1024, InboundIDs: []int64{1}},
		{Email: "unlimited-demo", SubscriptionID: "unlimited-demo-sub", Enabled: true, HasEnabled: true, ExpiryMS: 0, TotalBytes: 0, InboundIDs: []int64{2}},
	} {
		clients[client.Email] = client
	}
	return &Mock{clients: clients}
}

func (m *Mock) CheckConnection(context.Context) (Info, error) {
	return Info{Version: "mock", Authenticated: true, Capabilities: []string{"client-list", "client-get", "client-bulk-adjust", "client-update", "client-reset-traffic"}, NodeSync: "unknown"}, nil
}

func (m *Mock) GetClient(_ context.Context, email string) (Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.clients[email]
	if !ok {
		return Client{}, errors.New("client not found")
	}
	return client, nil
}

func (m *Mock) GetClientBySubscriptionID(_ context.Context, subscriptionID string) (Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, client := range m.clients {
		if client.SubscriptionID == subscriptionID {
			return client, nil
		}
	}
	return Client{}, errors.New("client not found")
}

func (m *Mock) ListClients(context.Context) ([]Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	clients := make([]Client, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	return clients, nil
}

func (m *Mock) ValidateSetQuota(context.Context, Client, bool, bool) error { return nil }
func (m *Mock) ValidateResetTraffic(context.Context) error                 { return nil }

func (m *Mock) ResetTraffic(_ context.Context, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.clients[email]
	if !ok {
		return errors.New("client not found")
	}
	client.UploadBytes = 0
	client.DownloadBytes = 0
	client.UsedBytes = 0
	client.HasTrafficBreakdown = true
	client.Enabled = true
	client.HasEnabled = true
	m.clients[email] = client
	return nil
}

func (m *Mock) Adjust(_ context.Context, email string, days int, addBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.clients[email]
	if !ok {
		return errors.New("client not found")
	}
	if (days > 0 && client.ExpiryMS == 0) || (addBytes > 0 && client.TotalBytes == 0) {
		return errors.New("unlimited quota cannot be adjusted in mock panel")
	}
	if days > 0 {
		client.ExpiryMS += int64(days) * int64(24*time.Hour/time.Millisecond)
	}
	if addBytes > 0 {
		client.TotalBytes += addBytes
	}
	m.clients[email] = client
	return nil
}

func (m *Mock) SetQuota(_ context.Context, original Client, expiryMS, totalBytes int64, setExpiry, setTotal bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.clients[original.Email]
	if !ok {
		return errors.New("client not found")
	}
	if setExpiry {
		client.ExpiryMS = expiryMS
	}
	if setTotal {
		client.TotalBytes = totalBytes
	}
	m.clients[original.Email] = client
	return nil
}
