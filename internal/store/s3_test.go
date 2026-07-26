package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSignedPathStylePut(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := New(Options{
		Endpoint: server.URL, Bucket: "bucket", Prefix: "root",
		Region: "us-east-1", AccessKey: "access", SecretKey: "secret",
		PathStyle: true, Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Put(context.Background(), client.Key("refs", "x.json"), []byte("body"), "application/json"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bucket/root/refs/x.json" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=access/") {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotBody != "body" {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestRequiresHTTPSUnlessExplicit(t *testing.T) {
	_, err := New(Options{Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("error = %v", err)
	}
}
