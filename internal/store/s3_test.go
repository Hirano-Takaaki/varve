package store

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestNewRejectsUnreadableCABundle(t *testing.T) {
	_, err := New(Options{
		Endpoint: "https://example.com", Bucket: "b",
		AccessKey: "a", SecretKey: "s",
		CACertFile: filepath.Join(t.TempDir(), "missing.pem"),
	})
	if err == nil || !strings.Contains(err.Error(), "read CA bundle") {
		t.Fatalf("expected read failure, got %v", err)
	}
}

func TestNewRejectsCABundleWithoutCertificates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(Options{
		Endpoint: "https://example.com", Bucket: "b",
		AccessKey: "a", SecretKey: "s", CACertFile: path,
	})
	if err == nil || !strings.Contains(err.Error(), "no certificates found") {
		t.Fatalf("expected empty-bundle failure, got %v", err)
	}
}

// 私有 CA の証明書を渡したクライアントが、その CA の TLS サーバに
// 接続できることを実際のハンドシェイクで確かめる。
func TestClientTrustsPrivateCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	pem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pem, 0o600); err != nil {
		t.Fatal(err)
	}

	// CA を渡さない場合は検証に失敗する。
	plain, err := New(Options{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Get(context.Background(), "anything"); err == nil {
		t.Fatal("expected TLS verification to fail without the CA")
	}

	// 渡した場合は TLS 検証を通り、リクエストが成立する。
	trusted, err := New(Options{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", PathStyle: true, CACertFile: caFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trusted.Get(context.Background(), "anything"); err != nil {
		t.Fatalf("expected the request to succeed over TLS, got %v", err)
	}
}
