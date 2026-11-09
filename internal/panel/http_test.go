package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testOpenAPI = `{"info":{"version":"3.x"},"paths":{"/panel/api/server/status":{"get":{}},"/panel/api/clients/list":{"get":{}},"/panel/api/clients/get/{email}":{"get":{}},"/panel/api/clients/bulkAdjust":{"post":{"requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object"},"example":{"emails":["alice","bob"],"addDays":30,"addBytes":1073741824}}}}}},"/panel/api/clients/update/{email}":{"post":{}}},"components":{"schemas":{"Client":{"required":["email","expiryTime","totalGB","enable"],"properties":{"email":{"type":"string"},"expiryTime":{"type":"integer"},"totalGB":{"type":"integer"},"enable":{"type":"boolean"}}}}}}`

func TestAPIUsesCustomPanelPathAndVerifiedBulkAdjustment(t *testing.T) {
	var gotAuth string
	var gotDays int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasPrefix(r.URL.Path, "/secret-panel/") {
			t.Errorf("request path lost panel prefix: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/secret-panel/panel/api/openapi.json":
			_, _ = w.Write([]byte(testOpenAPI))
		case r.URL.Path == "/secret-panel/panel/api/server/status":
			_, _ = w.Write([]byte(`{"success":true,"obj":{}}`))
		case r.URL.Path == "/secret-panel/panel/api/clients/get/alice@example.com":
			_, _ = w.Write([]byte(`{"success":true,"obj":{"client":{"email":"alice@example.com","expiryTime":2000000000000,"totalGB":10737418240,"enable":true},"inboundIds":[3],"traffic":{"up":1024,"down":2048}}}`))
		case r.URL.Path == "/secret-panel/panel/api/clients/bulkAdjust":
			var request struct {
				Emails  []string `json:"emails"`
				AddDays int      `json:"addDays"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			gotDays = request.AddDays
			if len(request.Emails) != 1 || request.Emails[0] != "alice@example.com" {
				t.Errorf("unexpected client scope in bulkAdjust: %#v", request.Emails)
			}
			_, _ = w.Write([]byte(`{"success":true,"obj":{"adjusted":1,"skipped":[]}}`))
		case r.URL.Path == "/secret-panel/panel/api/clients/update/alice@example.com":
			var fields map[string]any
			if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
				t.Error(err)
			}
			if fields["enable"] != true || fields["email"] != "alice@example.com" {
				t.Errorf("full update did not preserve required client fields: %#v", fields)
			}
			if _, ok := fields["traffic"]; ok {
				t.Errorf("read-only traffic metadata leaked into client update: %#v", fields)
			}
			if _, ok := fields["inboundIds"]; ok {
				t.Errorf("inbound metadata leaked into client update: %#v", fields)
			}
			_, _ = w.Write([]byte(`{"success":true,"msg":"Client updated"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewAPI(server.URL+"/secret-panel", "test-token", 2*time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	info, err := client.CheckConnection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "3.x" || !info.Authenticated {
		t.Fatalf("unexpected panel connection info: %#v", info)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	account, err := client.GetClient(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if account.UploadBytes != 1024 || account.DownloadBytes != 2048 || len(account.InboundIDs) != 1 {
		t.Fatalf("unexpected client status: %#v", account)
	}
	if err := client.Adjust(context.Background(), account.Email, 30, 10); err != nil {
		t.Fatal(err)
	}
	if gotDays != 30 {
		t.Fatalf("bulkAdjust addDays = %d", gotDays)
	}
	if err := client.SetQuota(context.Background(), account, 2000000000000, 20, true, true); err != nil {
		t.Fatal(err)
	}
}

func TestSetQuotaRefusesIncompleteClientWithoutSendingUpdate(t *testing.T) {
	updates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/openapi.json") {
			_, _ = w.Write([]byte(testOpenAPI))
			return
		}
		if strings.Contains(r.URL.Path, "/clients/update/") {
			updates++
		}
		_, _ = w.Write([]byte(`{"success":true,"obj":{}}`))
	}))
	defer server.Close()
	client, err := NewAPI(server.URL, "test-token", 2*time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	account := Client{Email: "alice", Fields: map[string]json.RawMessage{"email": json.RawMessage(`"alice"`)}}
	if err := client.SetQuota(context.Background(), account, 2000000000000, 10, true, true); err == nil {
		t.Fatal("incomplete full-row update was accepted")
	}
	if updates != 0 {
		t.Fatal("panel update was sent after incomplete record validation failed")
	}
}

func TestAdjustRefusesUnverifiedRequestSchemaWithoutSendingWrite(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/openapi.json") {
			_, _ = w.Write([]byte(`{"paths":{"/panel/api/clients/bulkAdjust":{"post":{}}}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/clients/bulkAdjust") {
			writes++
			_, _ = w.Write([]byte(`{"success":true,"obj":{"adjusted":1,"skipped":[]}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := NewAPI(server.URL, "test-token", 2*time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Adjust(context.Background(), "alice@example.com", 30, 1024)
	if !errors.Is(err, ErrWriteNotSent) {
		t.Fatalf("Adjust error = %v, want ErrWriteNotSent", err)
	}
	if writes != 0 {
		t.Fatalf("bulkAdjust requests = %d, want 0", writes)
	}
}
