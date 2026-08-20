package browser

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostExprEmbedsArgumentsAndShape(t *testing.T) {
	expr, err := postExpr("https://chatgpt.com/backend-api/files/library",
		map[string]string{"Authorization": "Bearer tok"},
		map[string]any{"cursor": "abc"})
	if err != nil {
		t.Fatalf("postExpr: %v", err)
	}
	// The JS body must stay faithful to fetch_library (export_chats.py:476-483).
	for _, want := range []string{
		"method: 'POST'",
		"'Content-Type': 'application/json'",
		"body: JSON.stringify(body)",
		"return {status: r.status, body: await r.text()};",
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("expression is missing %q:\n%s", want, expr)
		}
	}
	// Content-Type is spread AFTER the caller headers, so it wins.
	if strings.Index(expr, "...headers") > strings.Index(expr, "'Content-Type'") {
		t.Errorf("Content-Type must be spread after ...headers:\n%s", expr)
	}

	arg := expr[strings.LastIndex(expr, "})(")+len("})("):]
	arg = strings.TrimSuffix(arg, ")")
	var got struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    map[string]any    `json:"body"`
	}
	if err := json.Unmarshal([]byte(arg), &got); err != nil {
		t.Fatalf("inlined argument is not valid JSON (%v): %s", err, arg)
	}
	if got.URL != "https://chatgpt.com/backend-api/files/library" {
		t.Errorf("url = %q", got.URL)
	}
	if got.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("headers = %v", got.Headers)
	}
	if got.Body["cursor"] != "abc" {
		t.Errorf("body = %v", got.Body)
	}
}

func TestPostExprNilHeaders(t *testing.T) {
	expr, err := postExpr("https://x/", nil, map[string]any{})
	if err != nil {
		t.Fatalf("postExpr: %v", err)
	}
	if strings.Contains(expr, `"headers":null`) {
		t.Errorf("nil headers must be encoded as an empty object:\n%s", expr)
	}
	if !strings.Contains(expr, `"headers":{}`) {
		t.Errorf("expected an empty headers object:\n%s", expr)
	}
}

// postExpr shares parseFetchResult with the GET path, so the {status, body}
// decoding is already covered by TestParseFetchResult; here we only confirm
// a POST-shaped result decodes.
func TestPostResultParsesViaFetchResult(t *testing.T) {
	status, body, err := parseFetchResult([]byte(`{"status":200,"body":"{\"items\":[]}"}`))
	if err != nil {
		t.Fatalf("parseFetchResult: %v", err)
	}
	if status != 200 || body != `{"items":[]}` {
		t.Errorf("got (%d, %q)", status, body)
	}
}

func TestGetBinaryReturnsBytesStatusAndType(t *testing.T) {
	payload := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01, 0x02, 0xff}
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(200)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	status, body, ctype, err := GetBinary(srv.Client(), srv.URL+"/blob?sig=xyz", nil)
	if err != nil {
		t.Fatalf("GetBinary: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if string(body) != string(payload) {
		t.Errorf("body = %v, want %v", body, payload)
	}
	if ctype != "image/png" {
		t.Errorf("content-type = %q", ctype)
	}
	if gotUA != downloadUserAgent {
		t.Errorf("User-Agent = %q, want the realistic browser UA", gotUA)
	}
}

func TestGetBinaryPropagatesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("denied"))
	}))
	defer srv.Close()

	status, body, _, err := GetBinary(srv.Client(), srv.URL, nil)
	if err != nil {
		t.Fatalf("GetBinary: %v", err)
	}
	if status != 403 {
		t.Errorf("status = %d, want 403", status)
	}
	if string(body) != "denied" {
		t.Errorf("body = %q", body)
	}
}

func TestGetBinaryTransportError(t *testing.T) {
	// A closed server address yields a connection error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, _, _, err := GetBinary(srv.Client(), url, nil); err == nil {
		t.Fatal("expected a transport error from a closed server")
	}
}

func TestBinExprShapeAndRoundTrip(t *testing.T) {
	expr, err := binExpr("https://x/blob", map[string]string{"H": "v"})
	if err != nil {
		t.Fatalf("binExpr: %v", err)
	}
	for _, want := range []string{
		"await r.arrayBuffer()",
		"btoa(bin)",
		"return {status: r.status, body: btoa(bin),",
		"contentType: r.headers.get('content-type') || ''};",
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("expression missing %q:\n%s", want, expr)
		}
	}

	// Simulate what the page would return and confirm parseBinaryResult
	// reconstructs the exact bytes.
	payload := []byte{0x00, 0x10, 0xff, 0x42}
	b64 := base64.StdEncoding.EncodeToString(payload)
	raw, _ := json.Marshal(binaryResult{Status: 200, Body: &b64, ContentType: "image/png"})
	status, data, ctype, err := parseBinaryResult(raw)
	if err != nil {
		t.Fatalf("parseBinaryResult: %v", err)
	}
	if status != 200 || string(data) != string(payload) || ctype != "image/png" {
		t.Errorf("got (%d, %v, %q), want (200, %v, %q)", status, data, ctype, payload, "image/png")
	}
}

func TestParseBinaryResult(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		status  int
		body    []byte
		wantErr bool
	}{
		{name: "null body", raw: `{"status":204,"body":null}`, status: 204, body: nil},
		{name: "empty result", raw: ``, wantErr: true},
		{name: "null result", raw: `null`, wantErr: true},
		{name: "bad base64", raw: `{"status":200,"body":"!!!not-base64!!!"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body, _, err := parseBinaryResult([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got status=%d body=%v", status, body)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBinaryResult: %v", err)
			}
			if status != tt.status || string(body) != string(tt.body) {
				t.Errorf("got (%d, %v), want (%d, %v)", status, body, tt.status, tt.body)
			}
		})
	}
}
