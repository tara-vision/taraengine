package client

import (
	"context"
	"net/http"
	"testing"
)

// A body that cannot be encoded is refused before anything is sent.
func TestRequestRefusesAnUnencodableBody(t *testing.T) {
	c := New("http://127.0.0.1:1", "")
	if _, err := c.request(context.Background(), http.MethodPost, "/api/load", func() {}); err == nil {
		t.Fatal("a body that cannot be encoded was accepted")
	}
}
