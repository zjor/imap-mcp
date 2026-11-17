package apierr

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(context.Background(), rec, 403, "host_not_allowed", "IMAP host not allowed")
	if rec.Code != 403 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	var got map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	e := got["error"]
	if e["code"] != "host_not_allowed" || e["message"] != "IMAP host not allowed" {
		t.Errorf("body = %s", rec.Body)
	}
	if _, ok := e["request_id"]; !ok {
		t.Errorf("no request_id: %s", rec.Body)
	}
}
